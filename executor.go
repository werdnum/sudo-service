package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// stdinMountDir is where the optional stdin payload Secret is mounted in the
// executor pod. The command's stdin is redirected from <dir>/stdin.
const stdinMountDir = "/var/run/sudo-service"

// executorNamespace is the namespace the executor Job runs in: the requested
// one, or the controller namespace by default. Targeting another namespace is
// what lets the command mount that namespace's Secrets/PVCs as files.
func executorNamespace(sr *SudoRequest) string {
	if sr.Spec.Namespace != "" {
		return sr.Spec.Namespace
	}
	return ControllerNamespace
}

// clusterAdminEnabled reports whether the executor should run under the
// cluster-admin-bound executor SA. cluster-admin is only available in the
// controller namespace (that is where the SA lives); elsewhere the Job runs
// under the target namespace's unprivileged default SA. Within the controller
// namespace it defaults to true (historical behaviour) unless explicitly
// disabled. validateSpecExtras has already rejected clusterAdmin=true paired
// with a non-controller namespace, so this never returns true off-namespace.
func clusterAdminEnabled(sr *SudoRequest) bool {
	if executorNamespace(sr) != ControllerNamespace {
		return false
	}
	if sr.Spec.Privileges.ClusterAdmin != nil {
		return *sr.Spec.Privileges.ClusterAdmin
	}
	return true
}

// executorServiceAccount returns the SA name and automount setting for the
// executor pod. cluster-admin requests ride the dedicated executor SA; everything
// else rides the namespace default SA with its token automount disabled, since
// those jobs only ever touch mounted files, never the API.
func executorServiceAccount(sr *SudoRequest) (name string, automount *bool) {
	if clusterAdminEnabled(sr) {
		return ExecutorSAName, nil
	}
	no := false
	return "default", &no
}

// hasSpecExtras reports whether the request uses any of the widened pod fields
// or privilege toggles. Such requests always need a human: they are excluded
// from the auto-approve allowlist, which only reasons about command+image.
func hasSpecExtras(sr *SudoRequest) bool {
	return sr.Spec.Namespace != "" ||
		sr.Spec.Stdin != "" ||
		len(sr.Spec.Env) > 0 ||
		len(sr.Spec.EnvFrom) > 0 ||
		len(sr.Spec.Volumes) > 0 ||
		len(sr.Spec.VolumeMounts) > 0 ||
		len(sr.Spec.InitContainers) > 0 ||
		sr.Spec.Privileges.ClusterAdmin != nil
}

// getExecutorJob returns the executor Job named in status.executorJobName, or
// (nil, nil) if it has been GC'd. Used by handleApproved to detect the
// "Job ran and was deleted before we saw it complete" case so we don't replay
// the privileged command.
func (r *SudoRequestReconciler) getExecutorJob(ctx context.Context, sr *SudoRequest) (*batchv1.Job, error) {
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: executorNamespace(sr), Name: sr.Status.ExecutorJobName}, &job)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// findOrCreateJob returns the existing executor Job for the SudoRequest, or creates one.
// Job naming is deterministic so we don't double-create on requeue.
func (r *SudoRequestReconciler) findOrCreateJob(ctx context.Context, sr *SudoRequest) (*batchv1.Job, error) {
	ns := executorNamespace(sr)
	name := jobName(sr)
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &job)
	if err == nil {
		// On requeue the Job already exists; make sure its stdin payload does too
		// (it is owned by the Job, so this is a no-op once both are present).
		if err := r.ensureStdinSecret(ctx, sr, &job); err != nil {
			return nil, err
		}
		return &job, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	job = r.buildExecutorJob(sr, ns, name)
	if err := r.Create(ctx, &job); err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	// Materialise the stdin payload (owned by the Job, same namespace) after the
	// Job exists so the Secret is garbage-collected with it. The pod blocks on the
	// missing mount until this lands; the kubelet retries, so the brief window is
	// harmless.
	if err := r.ensureStdinSecret(ctx, sr, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// buildExecutorJob renders the executor Job, splicing in the request's curated
// pod extras (env, volumes, init containers, stdin) on top of the locked-down
// defaults. validateSpecExtras has already vetted everything spliced in here.
func (r *SudoRequestReconciler) buildExecutorJob(sr *SudoRequest, ns, name string) batchv1.Job {
	one := int32(0)
	ttl := ttlSecondsAfterApproval(sr)
	runAsNonRoot := true
	runAsUser := int64(1000)
	saName, automount := executorServiceAccount(sr)

	executor := corev1.Container{
		Name:    "executor",
		Image:   imageFor(sr),
		Command: executorCommand(sr),
		Env:     sr.Spec.Env,
		EnvFrom: sr.Spec.EnvFrom,
		// Copy so the stdin-mount append below never mutates the spec's slice.
		VolumeMounts:    append([]corev1.VolumeMount(nil), sr.Spec.VolumeMounts...),
		SecurityContext: hardenedContainerSecurityContext(),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}

	volumes := append([]corev1.Volume(nil), sr.Spec.Volumes...)
	if sr.Spec.Stdin != "" {
		executor.VolumeMounts = append(executor.VolumeMounts, corev1.VolumeMount{
			Name:      "sudo-service-stdin",
			MountPath: stdinMountDir,
			ReadOnly:  true,
		})
		mode := int32(0o444)
		volumes = append(volumes, corev1.Volume{
			Name: "sudo-service-stdin",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  stdinSecretName(sr),
					DefaultMode: &mode,
				},
			},
		})
	}

	// Stamp the controller-owned hardened securityContext onto requester init
	// containers (validateSpecExtras forbids them setting their own), so they
	// inherit the same profile as the executor container.
	initContainers := make([]corev1.Container, len(sr.Spec.InitContainers))
	for i, c := range sr.Spec.InitContainers {
		c.DeepCopyInto(&initContainers[i])
		initContainers[i].SecurityContext = hardenedContainerSecurityContext()
	}

	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    executorLabels(),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &one,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: executorLabels(),
				},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           saName,
					AutomountServiceAccountToken: automount,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
					},
					InitContainers: initContainers,
					Containers:     []corev1.Container{executor},
					Volumes:        volumes,
				},
			},
		},
	}

	// Cross-namespace ownerReferences are not honoured by Kubernetes GC (and the
	// executor VAP only requires the ownerRef for same-namespace cluster-admin
	// Jobs). So only set it in the controller namespace; cross-namespace Jobs are
	// reclaimed by TTLSecondsAfterFinished instead.
	if ns == ControllerNamespace {
		job.OwnerReferences = []metav1.OwnerReference{ownerRef(sr)}
	}
	return job
}

// executorCommand renders the container command. With no stdin it is the
// historical `sh -c <command>` (or the auto-approve argv). With stdin, the
// command is passed as a positional parameter — never interpolated into the
// script text, so there is no quoting to get wrong — and an outer shell
// redirects fd 0 from the mounted payload before exec'ing it.
func executorCommand(sr *SudoRequest) []string {
	if !hasSpecExtras(sr) {
		if tokens, ok := getAutoApproveParsedCommand(sr.Spec.Command, imageFor(sr)); ok {
			return tokens
		}
	}
	if sr.Spec.Stdin != "" {
		return []string{"/bin/sh", "-c", `exec /bin/sh -c "$1" < ` + stdinMountDir + "/stdin", "sudo-service", sr.Spec.Command}
	}
	return []string{"/bin/sh", "-c", sr.Spec.Command}
}

func hardenedContainerSecurityContext() *corev1.SecurityContext {
	allowPrivilegeEscalation := false
	readOnlyRootFS := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		ReadOnlyRootFilesystem:   &readOnlyRootFS,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// ensureStdinSecret creates the Secret holding spec.stdin, owned by the executor
// Job so it is garbage-collected when the Job's TTL elapses. No-op when the
// request has no stdin. Idempotent on requeue.
func (r *SudoRequestReconciler) ensureStdinSecret(ctx context.Context, sr *SudoRequest, job *batchv1.Job) error {
	if sr.Spec.Stdin == "" {
		return nil
	}
	tru := true
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stdinSecretName(sr),
			Namespace: job.Namespace,
			Labels:    map[string]string{"app": "sudo-service", "role": "stdin"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         "batch/v1",
				Kind:               "Job",
				Name:               job.Name,
				UID:                job.UID,
				Controller:         &tru,
				BlockOwnerDeletion: &tru,
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"stdin": []byte(sr.Spec.Stdin)},
	}
	if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create stdin secret: %w", err)
	}
	return nil
}

func executorLabels() map[string]string {
	return map[string]string{
		AppLabelKey:  ExecutorAppLabelValue,
		RoleLabelKey: ExecutorRoleLabelValue,
	}
}

// captureJobOutput reads the pod logs of the executor Job, stuffs them into a Secret
// owned by the SudoRequest, and returns the Secret name + exit code.
func (r *SudoRequestReconciler) captureJobOutput(ctx context.Context, sr *SudoRequest, job *batchv1.Job) (string, int32, error) {
	// Find the pod owned by the job (in the executor namespace, which may differ
	// from the controller namespace for cross-namespace requests).
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		return "", -1, fmt.Errorf("list job pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", -1, fmt.Errorf("no pods for job %s yet", job.Name)
	}
	pod := pods.Items[0]

	// Exit code.
	var exitCode int32 = -1
	if len(pod.Status.ContainerStatuses) > 0 {
		cs := pod.Status.ContainerStatuses[0]
		if cs.State.Terminated != nil {
			exitCode = cs.State.Terminated.ExitCode
		} else {
			return "", -1, fmt.Errorf("container not terminated")
		}
	}

	// Read pod logs via the typed clientset (controller-runtime client doesn't expose subresources).
	logs, err := getPodLogs(ctx, job.Namespace, pod.Name, "executor")
	if err != nil {
		return "", exitCode, fmt.Errorf("fetch logs: %w", err)
	}

	// Stuff logs into a Secret with ownerRef to the SudoRequest.
	secretName := outputSecretName(sr)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: ControllerNamespace,
			Labels: map[string]string{
				"app":  "sudo-service",
				"role": "output",
				// expiry label is read by the GC.
				"expires-at": fmt.Sprintf("%d", time.Now().Unix()+int64(ttlSecondsAfterApproval(sr))),
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef(sr)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"output": []byte(logs),
		},
	}

	if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", exitCode, fmt.Errorf("create output secret: %w", err)
	}
	return secretName, exitCode, nil
}

func jobName(sr *SudoRequest) string {
	// SudoRequest name is generateName-derived. Job name must be DNS-1123: lowercase, <=63 chars.
	// Use a stable prefix + UID-suffix for determinism.
	uid := strings.ReplaceAll(string(sr.UID), "-", "")
	if len(uid) > 12 {
		uid = uid[:12]
	}
	n := fmt.Sprintf("sudo-exec-%s", uid)
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

func stdinSecretName(sr *SudoRequest) string {
	uid := strings.ReplaceAll(string(sr.UID), "-", "")
	if len(uid) > 12 {
		uid = uid[:12]
	}
	n := fmt.Sprintf("sudo-stdin-%s", uid)
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

func outputSecretName(sr *SudoRequest) string {
	uid := strings.ReplaceAll(string(sr.UID), "-", "")
	if len(uid) > 12 {
		uid = uid[:12]
	}
	n := fmt.Sprintf("sudo-out-%s", uid)
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

// ttlSecondsAfterApproval returns the per-request TTL (capped at ExecutorJobTTL
// to keep us under the executor VAP's <= 3600 guard) or DefaultPostApproval if
// the requester didn't specify one. Used for both the Job's
// TTLSecondsAfterFinished and the output Secret's expires-at label so output
// retention exactly matches what the caller asked for.
func ttlSecondsAfterApproval(sr *SudoRequest) int32 {
	if sr.Spec.TTLSecondsAfterApproval == nil {
		return int32(DefaultPostApproval)
	}
	v := *sr.Spec.TTLSecondsAfterApproval
	if v < 0 {
		return 0
	}
	if v > int32(ExecutorJobTTL) {
		return int32(ExecutorJobTTL)
	}
	return v
}

func ownerRef(sr *SudoRequest) metav1.OwnerReference {
	tru := true
	return metav1.OwnerReference{
		APIVersion:         GroupName + "/" + GroupVersion,
		Kind:               "SudoRequest",
		Name:               sr.Name,
		UID:                sr.UID,
		Controller:         &tru,
		BlockOwnerDeletion: &tru,
	}
}
