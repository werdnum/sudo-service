package main

import (
	"bytes"
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

// stdinVolumeName is the controller-owned volume/mount name for the stdin
// payload. It is reserved: validateSpecExtras rejects a request that reuses the
// name or mounts at stdinMountDir, so the append in buildExecutorJob can't
// collide with a requester-supplied volume.
const stdinVolumeName = "sudo-service-stdin"

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

// autoApproveTokens returns the parsed auto-approve argv for the request, or
// (nil, false) if it is not auto-approvable. This is the single source of truth
// for that decision: both the reconciler's auto-approve gate and executorCommand's
// argv selection go through it, so they can't drift. A request that uses any
// widened pod field or privilege toggle is never auto-approvable — the allowlist
// only reasons about command+image.
func autoApproveTokens(sr *SudoRequest) ([]string, bool) {
	if hasSpecExtras(sr) {
		return nil, false
	}
	return getAutoApproveParsedCommand(sr.Spec.Command, imageFor(sr))
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
	// Uncached: the Job may be in spec.namespace, which the cache doesn't watch.
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: executorNamespace(sr), Name: sr.Status.ExecutorJobName}, &job)
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
	// Uncached: the Job may be in spec.namespace, which the cache doesn't watch.
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &job)
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
	// Output retention is the requester's ttlSecondsAfterApproval, but the Job
	// itself must outlive its completion long enough for the reconciler to capture
	// the pod logs — hence the floor. (See ExecutorJobTTLFloor.)
	jobTTL := ttlSecondsAfterApproval(sr)
	if jobTTL < int32(ExecutorJobTTLFloor) {
		jobTTL = int32(ExecutorJobTTLFloor)
	}
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
		Resources:       standardResources(),
	}

	volumes := append([]corev1.Volume(nil), sr.Spec.Volumes...)
	if sr.Spec.Stdin != "" {
		executor.VolumeMounts = append(executor.VolumeMounts, corev1.VolumeMount{
			Name:      stdinVolumeName,
			MountPath: stdinMountDir,
			ReadOnly:  true,
		})
		mode := int32(0o444)
		volumes = append(volumes, corev1.Volume{
			Name: stdinVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  stdinSecretName(sr),
					DefaultMode: &mode,
				},
			},
		})
	}

	// Stamp the controller-owned hardened securityContext and resource bounds onto
	// requester init containers (validateSpecExtras forbids them setting their own
	// securityContext), so they inherit the same locked-down, bounded profile as
	// the executor container — an init container can't run unbounded in a
	// namespace without a LimitRange.
	initContainers := make([]corev1.Container, len(sr.Spec.InitContainers))
	for i, c := range sr.Spec.InitContainers {
		c.DeepCopyInto(&initContainers[i])
		initContainers[i].SecurityContext = hardenedContainerSecurityContext()
		initContainers[i].Resources = standardResources()
	}

	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    executorLabels(),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &one,
			TTLSecondsAfterFinished: &jobTTL,
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
	if tokens, ok := autoApproveTokens(sr); ok {
		return tokens
	}
	if sr.Spec.Stdin != "" {
		return []string{"/bin/sh", "-c", `exec /bin/sh -c "$1" < ` + stdinMountDir + "/stdin", "sudo-service", sr.Spec.Command}
	}
	return []string{"/bin/sh", "-c", sr.Spec.Command}
}

// standardResources is the fixed request/limit profile applied to the executor
// container and every requester init container, so requester-supplied (or
// omitted) resources can't run unbounded.
func standardResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
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
	err := r.Create(ctx, sec)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create stdin secret: %w", err)
	}

	// The Secret name is derived from the SudoRequest UID, which the requester
	// learns at create time. In a target namespace where they can create Secrets,
	// they could pre-create it with different content so the Job mounts an
	// unapproved payload. So on AlreadyExists, fail closed unless the existing
	// Secret is the one we own (ownerRef to *this* Job) with the approved bytes.
	var existing corev1.Secret
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: sec.Name}, &existing); err != nil {
		return fmt.Errorf("get existing stdin secret: %w", err)
	}
	ownedByJob := false
	for _, o := range existing.OwnerReferences {
		if o.UID == job.UID && o.Kind == "Job" {
			ownedByJob = true
			break
		}
	}
	if !ownedByJob || !bytes.Equal(existing.Data["stdin"], sec.Data["stdin"]) {
		return fmt.Errorf("stdin secret %s/%s already exists and is not the controller-owned approved payload; refusing to mount it", job.Namespace, sec.Name)
	}
	return nil
}

// containerToReport returns the container whose logs and exit code best explain
// the outcome: the executor if it terminated, otherwise a failed init container
// (whose nonzero exit prevented the executor from starting). It returns an error
// if nothing has terminated yet (the caller should requeue).
func containerToReport(pod *corev1.Pod) (container string, exitCode int32, err error) {
	for i := range pod.Status.ContainerStatuses {
		cs := pod.Status.ContainerStatuses[i]
		if cs.Name == "executor" && cs.State.Terminated != nil {
			return "executor", cs.State.Terminated.ExitCode, nil
		}
	}
	// Executor never terminated — look for a failed init container.
	for i := range pod.Status.InitContainerStatuses {
		cs := pod.Status.InitContainerStatuses[i]
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return cs.Name, cs.State.Terminated.ExitCode, nil
		}
	}
	return "", -1, fmt.Errorf("no terminated container to report yet")
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
	// from the controller namespace for cross-namespace requests). Uncached: the
	// cache doesn't watch spec.namespace.
	var pods corev1.PodList
	if err := r.APIReader.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		return "", -1, fmt.Errorf("list job pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", -1, fmt.Errorf("no pods for job %s yet", job.Name)
	}
	pod := pods.Items[0]

	// Pick which container's logs and exit code to report. Normally it's the
	// executor; but if an init container failed, the executor never starts, so we
	// report the failing init container's logs/exit code — that's where the
	// requester's setup step actually broke.
	logContainer, exitCode, err := containerToReport(&pod)
	if err != nil {
		return "", -1, err
	}

	// Read pod logs via the typed clientset (controller-runtime client doesn't expose subresources).
	logs, err := getPodLogs(ctx, job.Namespace, pod.Name, logContainer)
	if err != nil {
		return "", exitCode, fmt.Errorf("fetch logs from %s: %w", logContainer, err)
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

// resourceName builds a deterministic DNS-1123 name (lowercase, <=63 chars) for a
// per-request child resource: a stable prefix plus a 12-char slice of the
// SudoRequest UID. jobName, stdinSecretName and outputSecretName must all agree
// on this scheme so the Job and the Secrets it references derive the same suffix
// from the same UID — hence the single helper.
func resourceName(sr *SudoRequest, prefix string) string {
	uid := strings.ReplaceAll(string(sr.UID), "-", "")
	if len(uid) > 12 {
		uid = uid[:12]
	}
	n := prefix + uid
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

func jobName(sr *SudoRequest) string          { return resourceName(sr, "sudo-exec-") }
func stdinSecretName(sr *SudoRequest) string  { return resourceName(sr, "sudo-stdin-") }
func outputSecretName(sr *SudoRequest) string { return resourceName(sr, "sudo-out-") }

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
