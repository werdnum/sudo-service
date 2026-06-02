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

// getExecutorJob returns the executor Job named in status.executorJobName, or
// (nil, nil) if it has been GC'd. Used by handleApproved to detect the
// "Job ran and was deleted before we saw it complete" case so we don't replay
// the privileged command.
func (r *SudoRequestReconciler) getExecutorJob(ctx context.Context, sr *SudoRequest) (*batchv1.Job, error) {
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: ControllerNamespace, Name: sr.Status.ExecutorJobName}, &job)
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
	name := jobName(sr)
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: ControllerNamespace, Name: name}, &job)
	if err == nil {
		return &job, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	// Build job spec.
	one := int32(0)
	ttl := ttlSecondsAfterApproval(sr)
	runAsNonRoot := true
	runAsUser := int64(1000)
	readOnlyRootFS := true
	allowPrivilegeEscalation := false

	job = batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ControllerNamespace,
			Labels: map[string]string{
				"app":  "sudo-service",
				"role": "executor",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef(sr)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &one,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":  "sudo-service",
						"role": "executor",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: ExecutorSAName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
					},
					Containers: []corev1.Container{{
						Name:  "executor",
						Image: imageFor(sr),
						Command: func() []string {
							tokens, ok := getAutoApproveParsedCommand(sr.Spec.Command, imageFor(sr))
							if ok {
								return tokens
							}
							return []string{"sh", "-c", sr.Spec.Command}
						}(),
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							ReadOnlyRootFilesystem:   &readOnlyRootFS,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
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
					}},
				},
			},
		},
	}

	if err := r.Create(ctx, &job); err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	return &job, nil
}

// captureJobOutput reads the pod logs of the executor Job, stuffs them into a Secret
// owned by the SudoRequest, and returns the Secret name + exit code.
func (r *SudoRequestReconciler) captureJobOutput(ctx context.Context, sr *SudoRequest, job *batchv1.Job) (string, int32, error) {
	// Find the pod owned by the job.
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(ControllerNamespace),
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
	logs, err := getPodLogs(ctx, ControllerNamespace, pod.Name, "executor")
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
