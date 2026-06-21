package main

import (
	"bytes"
	"context"
	"errors"
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

// errForeignChildObject means a Job/Secret already exists at the name we'd use but
// can't be confirmed as controller-created. handleApproved treats it as permanent
// (fail the request) rather than adopting a possibly attacker-planted object.
var errForeignChildObject = errors.New("pre-existing child object is not controller-created; refusing to adopt it")

// findOrCreateJob returns the existing executor Job for the SudoRequest, or creates one.
// The Job is named by the controller-minted status.ExecutorJobName. A Get that finds
// one on requeue would normally be our own prior create — but in a target namespace
// the name could in principle be learned (status read) and pre-created. We can't
// authenticate an arbitrary pod spec by inspection, so we only adopt a pre-existing
// Job in the controller namespace, where the executor VAP guarantees only the
// controller SA can create executor Jobs. Cross-namespace, a pre-existing Job is
// failed closed.
func (r *SudoRequestReconciler) findOrCreateJob(ctx context.Context, sr *SudoRequest) (*batchv1.Job, error) {
	ns := executorNamespace(sr)
	name := sr.Status.ExecutorJobName
	var job batchv1.Job
	// Uncached: the Job may be in spec.namespace, which the cache doesn't watch.
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &job)
	if err == nil {
		if ns != ControllerNamespace {
			return nil, errForeignChildObject
		}
		// Controller namespace: the executor VAP guarantees this Job is ours.
		// On requeue make sure its stdin payload exists too (owned by the Job).
		if err := r.ensureStdinSecret(ctx, sr, &job); err != nil {
			return nil, err
		}
		return &job, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	extras, err := decodePodExtras(sr)
	if err != nil {
		// validateSpecExtras already accepted this spec, so a decode failure here
		// is unexpected; surface it rather than build a malformed pod.
		return nil, fmt.Errorf("decode pod extras: %w", err)
	}
	job = r.buildExecutorJob(sr, ns, name, extras)
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
func (r *SudoRequestReconciler) buildExecutorJob(sr *SudoRequest, ns, name string, extras *podExtras) batchv1.Job {
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
		Env:     extras.Env,
		EnvFrom: extras.EnvFrom,
		// Copy so the stdin-mount append below never mutates the spec's slice.
		VolumeMounts:    append([]corev1.VolumeMount(nil), extras.VolumeMounts...),
		SecurityContext: hardenedContainerSecurityContext(),
		Resources:       standardResources(),
	}

	volumes := make([]corev1.Volume, len(extras.Volumes))
	for i := range extras.Volumes {
		extras.Volumes[i].DeepCopyInto(&volumes[i])
		boundEmptyDirSize(&volumes[i])
	}
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
					SecretName:  sr.Status.StdinSecretName,
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
	initContainers := make([]corev1.Container, len(extras.InitContainers))
	for i := range extras.InitContainers {
		extras.InitContainers[i].DeepCopyInto(&initContainers[i])
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

// DefaultEmptyDirSizeLimit caps emptyDir scratch space when the requester didn't
// set their own sizeLimit, so a command can't fill node disk in a namespace
// without an ephemeral-storage quota. A requester may override it with a larger
// (or smaller) sizeLimit, which is rendered on the approve page for the human.
var DefaultEmptyDirSizeLimit = resource.MustParse("1Gi")

// boundEmptyDirSize stamps the default sizeLimit onto an emptyDir volume that
// doesn't already declare one. No-op for non-emptyDir volumes and for emptyDir
// volumes the requester already bounded.
func boundEmptyDirSize(v *corev1.Volume) {
	if v.EmptyDir == nil || v.EmptyDir.SizeLimit != nil {
		return
	}
	size := DefaultEmptyDirSizeLimit.DeepCopy()
	v.EmptyDir.SizeLimit = &size
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
// request has no stdin.
//
// The Secret name is sr.Status.StdinSecretName — a random token minted into
// status before the Job exists. The name is also unguessable, but we don't rely
// on that: on AlreadyExists we verify the existing Secret is owned by *this* Job
// and carries the approved bytes, failing closed otherwise. So even if the name
// leaked (e.g. a requester with status read) and was pre-created, the Job never
// mounts an unapproved payload.
func (r *SudoRequestReconciler) ensureStdinSecret(ctx context.Context, sr *SudoRequest, job *batchv1.Job) error {
	if sr.Spec.Stdin == "" {
		return nil
	}
	tru := true
	fls := false
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sr.Status.StdinSecretName,
			Namespace: job.Namespace,
			Labels:    map[string]string{"app": "sudo-service", "role": "stdin"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       job.Name,
				UID:        job.UID,
				Controller: &tru,
				// Not BlockOwnerDeletion (see ownerRef): avoids needing update on
				// jobs/finalizers under OwnerReferencesPermissionEnforcement.
				BlockOwnerDeletion: &fls,
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
	// Already exists: confirm it's the Secret we own with the approved content.
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
		return errForeignChildObject
	}
	return nil
}

// getJobPod returns the executor Job's pod, read uncached because the pod may be
// in spec.namespace which the cache doesn't watch. Returns (nil, nil) when no pod
// exists yet. The job-name label alone is not authoritative — in a tenant
// namespace anyone who can create Pods could attach it — so we additionally
// require the pod to be controlled by this Job (ownerRef UID), preventing a
// spoofed pod from being read for logs/exit code or counted as progress.
func (r *SudoRequestReconciler) getJobPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.APIReader.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		return nil, fmt.Errorf("list job pods: %w", err)
	}
	for i := range pods.Items {
		if metav1.IsControlledBy(&pods.Items[i], job) {
			return &pods.Items[i], nil
		}
	}
	return nil, nil
}

// executorStartTimedOut reports whether the executor Job's pod has failed to make
// progress within ExecutorStartDeadline, so a stuck pod (unsatisfiable mount,
// unschedulable, image won't pull, or an init container that never exits) doesn't
// leave the request in Approved forever.
//
// Once the executor container itself is Running/Terminated the pod has started and
// the deadline no longer applies. Otherwise the clock runs from the executor's
// start window: Job creation while any init container is still
// running/pending — so a never-exiting init is caught at the deadline — and the
// last init's finish time once all inits have completed, giving the executor its
// own pull/create budget after a legitimately long init.
func (r *SudoRequestReconciler) executorStartTimedOut(ctx context.Context, job *batchv1.Job) (bool, string, error) {
	pod, err := r.getJobPod(ctx, job)
	if err != nil {
		return false, "", err
	}
	if pod == nil {
		return time.Since(job.CreationTimestamp.Time) > ExecutorStartDeadline*time.Second, "no pod scheduled", nil
	}
	if executorStarted(pod) {
		return false, "", nil
	}
	ref := executorWaitStart(pod, job)
	return time.Since(ref) > ExecutorStartDeadline*time.Second, podWaitReason(pod), nil
}

// executorStarted reports whether the executor container has reached
// Running/Terminated. A running *init* container does NOT count — otherwise an
// init that never exits would keep the pod "progressing" and defeat the deadline.
func executorStarted(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "executor" && (cs.State.Running != nil || cs.State.Terminated != nil) {
			return true
		}
	}
	return false
}

// podWaitReason returns the first waiting container's reason/message, for the
// timeout diagnostic (e.g. a missing Secret behind ContainerCreating).
func podWaitReason(pod *corev1.Pod) string {
	reason := "pod pending"
	allStatuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, cs := range allStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			reason = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				reason += ": " + cs.State.Waiting.Message
			}
		}
	}
	return reason
}

// executorWaitStart returns the moment the executor container's start window
// began: the latest init container finish time when all inits have completed,
// otherwise Job creation (the first init — or the executor when there are none —
// hasn't started yet, so the original deadline applies).
func executorWaitStart(pod *corev1.Pod, job *batchv1.Job) time.Time {
	ref := job.CreationTimestamp.Time
	if len(pod.Status.InitContainerStatuses) == 0 {
		return ref
	}
	latest := time.Time{}
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Terminated == nil {
			return ref // an init hasn't finished yet; executor window not open.
		}
		if cs.State.Terminated.FinishedAt.Time.After(latest) {
			latest = cs.State.Terminated.FinishedAt.Time
		}
	}
	if latest.IsZero() {
		return ref
	}
	return latest
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

// stopJob deletes the executor Job with background propagation, terminating its
// pod and cascading its owned stdin Secret. Used when we fail a request whose pod
// never started: without this the Job (which has no activeDeadlineSeconds, and no
// ownerRef when cross-namespace) would keep its pod and could still run the
// privileged command after the request is recorded Failed, and would leak in the
// target namespace. Best-effort: a NotFound (already gone) is fine.
func (r *SudoRequestReconciler) stopJob(ctx context.Context, job *batchv1.Job) error {
	policy := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return err
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
	// from the controller namespace for cross-namespace requests). Uncached: the
	// cache doesn't watch spec.namespace.
	pod, err := r.getJobPod(ctx, job)
	if err != nil {
		return "", -1, err
	}
	if pod == nil {
		return "", -1, fmt.Errorf("no pods for job %s yet", job.Name)
	}

	// Pick which container's logs and exit code to report. Normally it's the
	// executor; but if an init container failed, the executor never starts, so we
	// report the failing init container's logs/exit code — that's where the
	// requester's setup step actually broke.
	logContainer, exitCode, err := containerToReport(pod)
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
// SudoRequest UID. Used by jobName and outputSecretName. (The stdin Secret name
// is deliberately NOT built here — it is randomly minted into status so a
// requester can't pre-create it; see SudoRequestStatus.StdinSecretName.)
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
	fls := false
	return metav1.OwnerReference{
		APIVersion: GroupName + "/" + GroupVersion,
		Kind:       "SudoRequest",
		Name:       sr.Name,
		UID:        sr.UID,
		Controller: &tru,
		// Not BlockOwnerDeletion: we only want cascade GC, not delete-ordering.
		// Setting it true would require update on sudorequests/finalizers under the
		// OwnerReferencesPermissionEnforcement admission plugin, which we don't grant.
		BlockOwnerDeletion: &fls,
	}
}
