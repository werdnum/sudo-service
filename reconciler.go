package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// executorCleanupFinalizer guards deletion of a SudoRequest whose executor Job
// runs in another namespace. Cross-namespace Jobs carry no ownerRef (cross-
// namespace GC is not honoured by Kubernetes), so deleting the SudoRequest would
// not cascade them; the finalizer lets the controller stop the Job first.
const executorCleanupFinalizer = "sudo.andrewgarrett.dev/executor-cleanup"

var errApprovalTokenExpired = errors.New("approval token expired")

// SudoRequestReconciler drives the state machine:
//
//	(no status)  -> Pending  (persist approvable state)
//	Pending      -> Pending  (generate optional review aid, then notify)
//	Pending      -> Expired  (after 1h)
//	Approved     -> Executed (create Job, capture output, write Secret)
//	Approved     -> Failed   (job didn't finish or pod errored)
type SudoRequestReconciler struct {
	ControllerNamespace string
	client.Client
	// APIReader is an uncached, direct-to-apiserver reader. The manager's cache
	// (and thus the embedded Client's reads) is scoped to controllerNamespace(), so
	// reads of executor Jobs/Pods that may live in another namespace
	// (spec.namespace) must go through this reader, not the cache.
	APIReader     client.Reader
	Scheme        *runtime.Scheme
	Pushover      *PushoverClient
	Summarizer    *Summarizer // optional; nil when AI assessments are disabled
	Broadcaster   *Broadcaster
	Recorder      record.EventRecorder
	PublicBaseURL string
	// PodLogs is injectable for tests; production falls back to streamPodLogs.
	PodLogs func(context.Context, string, string, string) (io.ReadCloser, error)
}

func (r *SudoRequestReconciler) controllerNamespace() string {
	return configuredControllerNamespace(r.ControllerNamespace)
}

func (r *SudoRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&SudoRequest{}).
		Owns(&corev1.Secret{}).
		// Reconcile a few requests in parallel. controller-runtime still
		// serializes reconciles of the same object via the workqueue, so this
		// stays replay-safe; it only lets independent requests proceed
		// concurrently. The motivating case is the optional AI summary: that
		// best-effort call can block its reconcile for up to its timeout, and
		// with a single worker one slow request would head-of-line-block every
		// other transition (e.g. an approved request waiting for its executor
		// Job). A small worker pool keeps those independent transitions moving.
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(r)
}

func (r *SudoRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("sudorequest", req.NamespacedName)

	var sr SudoRequest
	if err := r.Get(ctx, req.NamespacedName, &sr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Being deleted: stop a still-running cross-namespace executor Job before
	// releasing the object, then drop the finalizer. Without this the Job's pod
	// keeps running with its mounted Secrets/PVCs after the request is gone.
	if !sr.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&sr, executorCleanupFinalizer) {
			if sr.Status.ExecutorJobName != "" {
				job, err := r.getExecutorJob(ctx, &sr)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("read executor job during finalize: %w", err)
				}
				if job != nil {
					if err := r.stopJob(ctx, job); err != nil {
						return ctrl.Result{}, fmt.Errorf("stop executor job during finalize: %w", err)
					}
				}
			}
			controllerutil.RemoveFinalizer(&sr, executorCleanupFinalizer)
			if err := r.Update(ctx, &sr); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	switch sr.Status.Phase {
	case "":
		return r.handleNew(ctx, &sr)
	case PhasePending:
		return r.handlePending(ctx, &sr)
	case PhaseApproved:
		return r.handleApproved(ctx, &sr)
	case PhaseExecuted, PhaseFailed, PhaseDenied, PhaseExpired:
		// Terminal phases. Nothing more to do; output Secret has its own TTL GC.
		log.V(1).Info("terminal phase", "phase", sr.Status.Phase)
		return ctrl.Result{}, nil
	default:
		log.Info("unknown phase, treating as pending", "phase", sr.Status.Phase)
		return ctrl.Result{}, nil
	}
}

func (r *SudoRequestReconciler) handleNew(ctx context.Context, sr *SudoRequest) (ctrl.Result, error) {
	profile, resolvedImage, warnings, profileErr := resolveAndPreflight(sr)
	if profileErr == nil {
		sr.Status.ResolvedImage = resolvedImage
		sr.Status.PreflightWarnings = warnings
		if profile != nil {
			sr.Status.ResolvedProfile = profile.Name
		}
	}

	// Reject syntactically-broken commands before they cost the reviewer a
	// round-trip. The HTTP API rejects these at submission, but a request
	// created directly against the CRD bypasses that path and only reaches us
	// here. Fail it now rather than push an unrunnable command to the human.
	if err := validateCommandSyntax(sr.Spec.Command); err != nil {
		now := metav1.NewTime(time.Now())
		sr.Status.Phase = PhaseDenied
		sr.Status.DeniedBy = "syntax-check"
		sr.Status.DeniedAt = &now
		sr.Status.DenialReason = err.Error()
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, fmt.Errorf("status update Denied for invalid syntax: %w", err)
		}
		r.Recorder.Eventf(sr, corev1.EventTypeWarning, "Denied", "Rejected: %v", err)
		r.Broadcaster.Publish(string(sr.UID), Event{
			Type:         "phase",
			Phase:        PhaseDenied,
			DeniedBy:     "syntax-check",
			DenialReason: err.Error(),
			Requester:    sr.Spec.Requester,
			Reason:       sr.Spec.Reason,
			Command:      sr.Spec.Command,
			CreatedAt:    sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
			RetryOfUID:   sr.Spec.RetryOfUID,
		})
		return ctrl.Result{}, nil
	}

	// Reject a spec whose widened pod fields fall outside the curated allowlist
	// (a hostPath volume, an init container setting its own securityContext, ...).
	// As with the syntax check, the HTTP API rejects these at submission; a
	// CRD-created one only reaches us here, so deny it before the approval push.
	if err := firstError(profileErr, validateSpecExtras(sr, r.controllerNamespace())); err != nil {
		now := metav1.NewTime(time.Now())
		sr.Status.Phase = PhaseDenied
		sr.Status.DeniedBy = "spec-validation"
		sr.Status.DeniedAt = &now
		sr.Status.DenialReason = err.Error()
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, fmt.Errorf("status update Denied for invalid spec: %w", err)
		}
		r.Recorder.Eventf(sr, corev1.EventTypeWarning, "Denied", "Rejected: %v", err)
		r.Broadcaster.Publish(string(sr.UID), Event{
			Type:         "phase",
			Phase:        PhaseDenied,
			DeniedBy:     "spec-validation",
			DenialReason: err.Error(),
			Requester:    sr.Spec.Requester,
			Reason:       sr.Spec.Reason,
			Command:      sr.Spec.Command,
			CreatedAt:    sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
			RetryOfUID:   sr.Spec.RetryOfUID,
		})
		return ctrl.Result{}, nil
	}

	// Auto-approve only reasons about command+image, so requests that use the
	// widened pod fields or privilege toggles always require a human
	// (autoApproveTokens enforces that, shared with executorCommand).
	if _, ok := autoApproveTokens(sr); ok {
		now := metav1.NewTime(time.Now())
		sr.Status.Phase = PhaseApproved
		sr.Status.ApprovedBy = "auto-approve"
		sr.Status.ApprovedAt = &now
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, fmt.Errorf("status update Approved for auto-approve: %w", err)
		}
		r.Recorder.Eventf(sr, corev1.EventTypeNormal, "Approved", "Approved by auto-approve")
		r.Broadcaster.Publish(string(sr.UID), Event{
			Type:       "phase",
			Phase:      PhaseApproved,
			ApprovedBy: "auto-approve",
			Requester:  sr.Spec.Requester,
			Reason:     sr.Spec.Reason,
			Command:    sr.Spec.Command,
			CreatedAt:  sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
			RetryOfUID: sr.Spec.RetryOfUID,
		})
		return ctrl.Result{}, nil
	}

	token, err := randomToken(32)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("mint token: %w", err)
	}
	hash := sha256Hex(token)

	expiry := metav1.NewTime(time.Now().Add(ApprovalTokenTTL * time.Second))

	// Store the plaintext token before status references it. If the subsequent
	// status update fails this Secret is harmless (and owner-GC'd): no external
	// notification has been sent and no request is approvable with its token.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "sudo-approval-token-",
			Namespace:    sr.Namespace,
			Labels: map[string]string{
				"app":        "sudo-service",
				"role":       "approval-token",
				"expires-at": fmt.Sprintf("%d", expiry.Unix()),
			},
		},
		Data: map[string][]byte{"token": []byte(token)},
	}
	if err := controllerutil.SetControllerReference(sr, secret, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set approval-token Secret owner: %w", err)
	}
	if err := r.Create(ctx, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("create approval-token Secret: %w", err)
	}

	sr.Status.Phase = PhasePending
	sr.Status.ApprovalTokenHash = hash
	sr.Status.ApprovalTokenExpiresAt = &expiry
	sr.Status.ApprovalTokenSecretName = secret.Name
	sr.Status.NotificationState = NotificationPending
	if r.Summarizer != nil {
		sr.Status.PermissionAssessmentState = PermissionAssessmentPending
	}

	if err := r.Status().Update(ctx, sr); err != nil {
		// The update result is ambiguous: the apiserver may have committed the
		// status even when the client observed a timeout. Keep the referenced
		// Secret rather than risk stranding a durable Pending request. Its owner
		// reference and expires-at label bound cleanup if the write truly failed.
		return ctrl.Result{}, fmt.Errorf("status update Pending: %w", err)
	}

	r.Recorder.Eventf(sr, corev1.EventTypeNormal, "Pending", "Awaiting human approval; notification queued")
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type:       "phase",
		Phase:      PhasePending,
		Requester:  sr.Spec.Requester,
		Reason:     sr.Spec.Reason,
		Command:    sr.Spec.Command,
		CreatedAt:  sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
		RetryOfUID: sr.Spec.RetryOfUID,
	})

	// A separate reconcile delivers the notification. At this point both the
	// Pending phase and the exact token referenced by the link are durable.
	return ctrl.Result{Requeue: true}, nil
}

func (r *SudoRequestReconciler) handlePending(ctx context.Context, sr *SudoRequest) (ctrl.Result, error) {
	// Expire after 1h.
	age := time.Since(sr.CreationTimestamp.Time)
	if age >= PendingRequestTTL*time.Second {
		sr.Status.Phase = PhaseExpired
		// Clear token so a stray push click can't approve.
		tokenSecretName := sr.Status.ApprovalTokenSecretName
		sr.Status.ApprovalTokenHash = ""
		sr.Status.ApprovalTokenExpiresAt = nil
		sr.Status.ApprovalTokenSecretName = ""
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, err
		}
		r.deleteApprovalTokenSecret(ctx, sr.Namespace, tokenSecretName)
		r.Recorder.Eventf(sr, corev1.EventTypeWarning, "Expired", "Request expired without approval after %s", age)
		r.Broadcaster.Publish(string(sr.UID), Event{
			Type:       "phase",
			Phase:      PhaseExpired,
			Requester:  sr.Spec.Requester,
			Reason:     sr.Spec.Reason,
			Command:    sr.Spec.Command,
			CreatedAt:  sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
			RetryOfUID: sr.Spec.RetryOfUID,
		})
		return ctrl.Result{}, nil
	}

	// Pending was persisted before optional model work, so the approval page is
	// usable even while generation is slow. Generate at most once before sending
	// the notification; failure is recorded but does not block delivery.
	if sr.Status.PermissionAssessmentState == PermissionAssessmentPending {
		assessment, err := r.generatePermissionAssessment(ctx, sr)
		state := PermissionAssessmentGenerated
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "AI permission assessment failed; continuing without one")
			state = PermissionAssessmentFailed
		}
		if updateErr := r.updateStatus(ctx, sr, func(current *SudoRequest) {
			current.Status.PermissionAssessment = assessment
			current.Status.PermissionAssessmentState = state
		}); updateErr != nil {
			return ctrl.Result{}, fmt.Errorf("record permission assessment: %w", updateErr)
		}
		sr.Status.PermissionAssessment = assessment
		sr.Status.PermissionAssessmentState = state
	}

	// Empty notification state means an older controller already delivered the notification before
	// writing Pending. Only the explicit Pending marker opts into this lifecycle.
	if sr.Status.NotificationState == NotificationPending {
		if sr.Status.NotificationNextAttemptAt != nil {
			if delay := time.Until(sr.Status.NotificationNextAttemptAt.Time); delay > 0 {
				return ctrl.Result{RequeueAfter: delay}, nil
			}
		}
		return r.deliverApprovalNotification(ctx, sr)
	}
	// Requeue at expiry.
	return ctrl.Result{RequeueAfter: PendingRequestTTL*time.Second - age + time.Second}, nil
}

func (r *SudoRequestReconciler) generatePermissionAssessment(ctx context.Context, sr *SudoRequest) (*PermissionAssessment, error) {
	if r.Summarizer == nil {
		return nil, nil
	}
	// Give the model the ground-truth Pod spec, but redact literal env values
	// because this context leaves the cluster. Human review surfaces keep raw
	// values as ground truth.
	aiContext := ""
	if hasSpecExtras(sr) {
		if tmpl, err := displayPodTemplate(sr, true); err == nil {
			aiContext = tmpl
		} else {
			aiContext = specExtrasText(sr, true)
		}
	}
	summaryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return r.Summarizer.Summarize(summaryCtx, sr.Spec.Command, imageFor(sr), sr.Spec.Reason, aiContext)
}

func (r *SudoRequestReconciler) deliverApprovalNotification(ctx context.Context, sr *SudoRequest) (ctrl.Result, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: sr.Namespace, Name: sr.Status.ApprovalTokenSecretName}
	if key.Name == "" {
		return ctrl.Result{}, fmt.Errorf("pending notification has no approval-token Secret")
	}
	if err := r.Get(ctx, key, &secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("read approval-token Secret: %w", err)
	}
	token := string(secret.Data["token"])
	if token == "" || !constantTimeEqual(sha256Hex(token), sr.Status.ApprovalTokenHash) {
		return ctrl.Result{}, fmt.Errorf("approval-token Secret does not match persisted hash")
	}

	u, err := url.Parse(r.PublicBaseURL)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("parse PublicBaseURL: %w", err)
	}
	u.Path = "/approve"
	q := u.Query()
	q.Set("id", string(sr.UID))
	q.Set("t", token)
	u.RawQuery = q.Encode()

	title := fmt.Sprintf("sudo: permission requested by %s", sr.Spec.Requester)
	body := ""
	if sr.Status.PermissionAssessment != nil {
		body = "Permission requested: " + sr.Status.PermissionAssessment.Request + "\n"
		if len(sr.Status.PermissionAssessment.Effects) > 0 {
			body += "Effects: " + formatPermissionEffects(sr.Status.PermissionAssessment.Effects) + "\n"
		}
	} else if sr.Status.Summary != "" {
		// Rolling-upgrade fallback for Pending records written by an older
		// controller. New records never write the free-form summary field.
		body = "Review aid: " + sr.Status.Summary + "\n"
	}
	body += fmt.Sprintf("reason: %s\ncommand: %s\nimage: %s", sr.Spec.Reason, sr.Spec.Command, imageFor(sr))
	if profile := profileFor(sr); profile != "" {
		body += "\nprofile: " + profile
	}
	for _, warning := range sr.Status.PreflightWarnings {
		body += "\npreflight warning: " + warning
	}
	if sr.Spec.RetryOfUID != "" {
		body += "\nretry of: " + sr.Spec.RetryOfUID
	}
	if actor := submittedByFor(sr); actor != sr.Spec.Requester {
		body += "\nresubmitted by: " + actor
	}
	if extras := specExtrasText(sr, true, r.controllerNamespace()); extras != "" {
		body += "\n" + extras
	}

	attemptedAt := metav1.NewTime(time.Now())
	reqID, sendErr := r.Pushover.SendApproval(ctx, title, body, u.String())
	if sendErr != nil {
		nextAttemptAt := metav1.NewTime(attemptedAt.Add(30 * time.Second))
		if err := r.updateStatus(ctx, sr, func(current *SudoRequest) {
			current.Status.NotificationAttempts++
			current.Status.NotificationLastAttemptAt = &attemptedAt
			current.Status.NotificationNextAttemptAt = &nextAttemptAt
			current.Status.NotificationLastError = truncate(sendErr.Error(), 512)
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("record Pushover failure: %w", err)
		}
		ctrl.LoggerFrom(ctx).Error(sendErr, "Pushover send failed; will retry the persisted link")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	deliveredAt := metav1.NewTime(time.Now())
	if err := r.updateStatus(ctx, sr, func(current *SudoRequest) {
		current.Status.NotificationAttempts++
		current.Status.NotificationLastAttemptAt = &attemptedAt
		current.Status.NotificationNextAttemptAt = nil
		current.Status.NotificationDeliveredAt = &deliveredAt
		current.Status.NotificationLastError = ""
		current.Status.NotificationState = NotificationDelivered
		current.Status.PushoverRequestID = reqID
	}); err != nil {
		// A retry reuses this exact persisted token. Pushover has no idempotency
		// key, so a status-write failure can duplicate a message, but never with a
		// dead predecessor link.
		return ctrl.Result{}, fmt.Errorf("record Pushover delivery: %w", err)
	}
	r.Recorder.Eventf(sr, corev1.EventTypeNormal, "Notified", "Approval notification delivered (pushover request=%s)", reqID)
	return ctrl.Result{RequeueAfter: PendingRequestTTL*time.Second - time.Since(sr.CreationTimestamp.Time) + time.Second}, nil
}

// updateStatus applies mutate to the request's status and persists it, retrying
// on optimistic-concurrency conflicts.
//
// The manager cache lags the controller's own writes, and the Approved path does
// several status writes back-to-back with immediate requeues (mint names, record
// Job UID). A reconcile can therefore read a stale resourceVersion from the cache
// and collide on its next Status().Update with "the object has been modified".
// Surfacing that as a reconcile error is actively harmful on the Job path: it
// rolls back (deletes) the freshly-created executor Job and ultimately fails the
// just-approved request (see issue #20). Instead we refetch the object uncached —
// straight from the apiserver, bypassing the lagging cache — re-apply the
// mutation, and retry. sr is updated in place to the persisted object so the
// caller's later logic sees the current state. mutate must be idempotent: it is
// re-run against the refetched object on every retry.
func (r *SudoRequestReconciler) updateStatus(ctx context.Context, sr *SudoRequest, mutate func(*SudoRequest)) error {
	mutate(sr)
	err := r.Status().Update(ctx, sr)
	if err == nil || !apierrors.IsConflict(err) {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest SudoRequest
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(sr), &latest); err != nil {
			return err
		}
		mutate(&latest)
		if err := r.Status().Update(ctx, &latest); err != nil {
			return err
		}
		latest.DeepCopyInto(sr)
		return nil
	})
}

// markFailed records the terminal Failed phase with a human-readable reason,
// emitting the event + broadcast the same way the other terminal transitions do.
// The reason is persisted to status.failureReason so the requester-facing HTTP
// status can explain a Failed request that has no exitCode/output (issue #20).
func (r *SudoRequestReconciler) markFailed(ctx context.Context, sr *SudoRequest, reason string) (ctrl.Result, error) {
	if err := r.updateStatus(ctx, sr, func(s *SudoRequest) {
		s.Status.Phase = PhaseFailed
		s.Status.FailureReason = reason
	}); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(sr, corev1.EventTypeWarning, "Failed", "%s", reason)
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type:          "phase",
		Phase:         PhaseFailed,
		FailureReason: reason,
		Requester:     sr.Spec.Requester,
		Reason:        sr.Spec.Reason,
		Command:       sr.Spec.Command,
		CreatedAt:     sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
		RetryOfUID:    sr.Spec.RetryOfUID,
	})
	return ctrl.Result{}, nil
}

// failApproved moves an Approved request to Failed with a reason.
//
// It first best-effort stops any executor Job sitting at the minted name. That
// Job may be one we must not leave running: a foreign/pre-created Job, a Job that
// replaced ours (UID mismatch), our own Job that ended up mounting a foreign
// stdin Secret, or a stuck pod past its start deadline. None of these have an
// ownerRef/activeDeadlineSeconds to stop them, so without this they'd keep
// running (an unapproved or unreviewed workload) after we record Failed.
func (r *SudoRequestReconciler) failApproved(ctx context.Context, sr *SudoRequest, reason string) (ctrl.Result, error) {
	if sr.Status.ExecutorJobName != "" {
		job, err := r.getExecutorJob(ctx, sr)
		if err != nil {
			return ctrl.Result{}, err
		}
		if job != nil {
			// Stopping the Job is a precondition for the terminal transition, not
			// best-effort: once we record Failed there's no further reconcile, so a
			// transient delete error must requeue rather than leave the (possibly
			// foreign/unapproved or stuck) workload running.
			if err := r.stopJob(ctx, job); err != nil {
				return ctrl.Result{}, fmt.Errorf("stop executor job before failing: %w", err)
			}
		}
	}
	return r.markFailed(ctx, sr, reason)
}

func (r *SudoRequestReconciler) handleApproved(ctx context.Context, sr *SudoRequest) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Refetch uncached before making any decisions. The manager cache lags the
	// controller's own writes, and the Approved path below does several status
	// writes back-to-back with immediate requeues (finalizer, mint names, record
	// Job UID). Reading the gating fields (ExecutorJobName/ExecutorJobUID) from a
	// stale cache would drive the wrong branch — re-minting a name, or re-entering
	// findOrCreateJob for a Job that already exists (which, cross-namespace, fails
	// closed as "foreign"). Reading straight from the apiserver makes every
	// decision below act on the current object (issue #20).
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(sr), sr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if sr.Status.Phase != PhaseApproved {
		// Lost a race with another writer (or a manual edit) since the cached read
		// that routed us here; re-reconcile from the now-current phase.
		return ctrl.Result{Requeue: true}, nil
	}

	// A cross-namespace executor Job carries no ownerRef, so it won't cascade when
	// the SudoRequest is deleted. Add the cleanup finalizer before creating any
	// Job (same-namespace Jobs cascade via their ownerRef and don't need it).
	if executorNamespace(sr, r.controllerNamespace()) != r.controllerNamespace() && !controllerutil.ContainsFinalizer(sr, executorCleanupFinalizer) {
		controllerutil.AddFinalizer(sr, executorCleanupFinalizer)
		if err := r.Update(ctx, sr); err != nil {
			return ctrl.Result{}, fmt.Errorf("add executor-cleanup finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Replay-safe Job handling:
	//
	// First reconcile under Approved: status.executorJobName is empty —
	// create (or pick up an in-flight) Job and stamp the name into status
	// BEFORE doing anything else. This way, if the controller crashes or
	// the Status update for Executed/Failed never lands, the next reconcile
	// won't recreate the Job.
	//
	// Subsequent reconciles: status.executorJobName is set. Get the Job by
	// that name; if it's gone (e.g. ttlSecondsAfterApproval=0 + kube-
	// controller-manager already GC'd it), we have to assume it already ran
	// — fail the request rather than re-create and replay the privileged
	// command. We have no output to capture in that case.
	// Mint the unguessable child-resource names BEFORE creating anything (the Job
	// is named ExecutorJobName, and references StdinSecretName). Persisting them
	// first makes creation replay-safe and means a requester who can create Jobs/
	// Secrets in the target namespace can't pre-create predictable sudo-exec-<uid>
	// / sudo-stdin-<uid> objects for the controller to adopt.
	if sr.Status.ExecutorJobName == "" {
		jobSuffix, err := randomToken(8)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("mint executor job name: %w", err)
		}
		jobName := "sudo-exec-" + jobSuffix
		var stdinName string
		if sr.Spec.Stdin != "" {
			stdinSuffix, err := randomToken(8)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("mint stdin secret name: %w", err)
			}
			stdinName = "sudo-stdin-" + stdinSuffix
		}
		if err := r.updateStatus(ctx, sr, func(s *SudoRequest) {
			// Idempotent: keep an already-minted name (a prior reconcile whose write
			// we raced) rather than clobbering it with a fresh suffix.
			if s.Status.ExecutorJobName != "" {
				return
			}
			s.Status.ExecutorJobName = jobName
			s.Status.StdinSecretName = stdinName
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("record minted resource names: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Create the Job once (at the minted name) and record its UID. The UID lets
	// later reconciles tell "not created yet" from "created then GC'd".
	if sr.Status.ExecutorJobUID == "" {
		job, err := r.findOrCreateJob(ctx, sr)
		if err != nil {
			// A permanent rejection (the apiserver refused the Job spec, RBAC
			// forbids it, or the target namespace doesn't exist) will never succeed
			// on retry. Fail the request instead of looping forever in Approved.
			// Validation catches the known cases up front; this is the backstop for
			// anything that slips through.
			if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsNotFound(err) ||
				errors.Is(err, errForeignChildObject) || errors.Is(err, errDisallowedSecret) {
				return r.failApproved(ctx, sr, fmt.Sprintf("executor Job rejected: %v", err))
			}
			return ctrl.Result{}, err
		}
		uid := string(job.UID)
		if err := r.updateStatus(ctx, sr, func(s *SudoRequest) {
			if s.Status.ExecutorJobUID != "" {
				return
			}
			s.Status.ExecutorJobUID = uid
		}); err != nil {
			// The status write failed even after refetch+retry — this is no longer a
			// transient cache-lag conflict. Roll back the Job we just created.
			// Otherwise the next reconcile re-enters with ExecutorJobUID still empty,
			// finds this un-recorded Job, and (cross-namespace, where we can't
			// authenticate it) fails the request as "foreign". Deleting it gives the
			// retry a clean create.
			if delErr := r.stopJob(ctx, job); delErr != nil {
				log.Error(delErr, "failed to roll back executor Job after UID-record failure", "job", job.Name)
			}
			return ctrl.Result{}, fmt.Errorf("record executorJobUID: %w", err)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	job, err := r.getExecutorJob(ctx, sr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job != nil && string(job.UID) != sr.Status.ExecutorJobUID {
		// A different Job now occupies our name (ours was deleted and replaced).
		// Don't read its logs/exit code as if it were the approved workload.
		return r.failApproved(ctx, sr, "executor Job was replaced by a different object; refusing to use it")
	}
	if job == nil {
		// If we already captured the result (the sidecar two-phase below records
		// outputSecret+exitCode before tearing the Job down), the Job being gone now
		// is our own teardown, not a lost execution — finalize from the recorded
		// result instead of failing.
		if sr.Status.ExitCode != nil && (sr.Status.OutputSecretRef != "" || sr.Status.OutputCaptureState != "") {
			return r.finalizeFromRecordedResult(ctx, sr)
		}
		// Job has been GC'd before we observed completion. Treat as Failed
		// rather than re-creating (which would re-run the privileged command).
		log.Info("executor Job missing before completion; failing request to prevent replay",
			"jobName", sr.Status.ExecutorJobName)
		return r.markFailed(ctx, sr, fmt.Sprintf(
			"Executor Job %s disappeared before controller observed completion (likely ttlSecondsAfterApproval too short)",
			sr.Status.ExecutorJobName))
	}

	// Completion is signalled by the executor container terminating, OR the Job's
	// counts advancing. We check the executor container directly because the Job's
	// Succeeded/Failed never advance if a mutating webhook injected a sidecar that
	// keeps the pod running after the executor exits — which would otherwise hang
	// the request forever.
	pod, err := r.getJobPod(ctx, job)
	if err != nil {
		return ctrl.Result{}, err
	}
	jobFinished := job.Status.Succeeded > 0 || job.Status.Failed > 0
	executorDone := pod != nil && executorContainerTerminated(pod)
	if !jobFinished && !executorDone {
		// Still running — requeue, but guard against a pod that never starts. An
		// unsatisfiable mount (a Secret/ConfigMap/PVC that doesn't exist in the
		// target namespace), an unschedulable pod, an image that won't pull, etc.
		// leaves the pod in ContainerCreating without ever incrementing the Job's
		// succeeded/failed counts, so without this the request would sit in Approved
		// forever.
		timedOut, reason, err := r.executorStartTimedOut(ctx, job)
		if err != nil {
			return ctrl.Result{}, err
		}
		if timedOut {
			// failApproved stops the Job (it has no activeDeadlineSeconds and, cross
			// namespace, no ownerRef, so leaving it would let the pod start and run
			// the privileged command after we record Failed).
			return r.failApproved(ctx, sr, fmt.Sprintf("executor pod did not start within %ds: %s", ExecutorStartDeadline, reason))
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Job finished (or the executor terminated while an injected sidecar keeps the
	// Job from finishing). Collect output and finalize.
	result, err := r.captureJobOutput(ctx, sr, job)
	if err != nil {
		// In the sidecar scenario the Job never finishes on its own, so it has no
		// TTL to fall back on and a terminal SudoRequest is never reconciled again.
		// Going terminal here would orphan the pod (and its mounted Secrets/PVCs)
		// forever. The sidecar keeps the pod — and its logs — alive, so the capture
		// is retriable; requeue instead of recording a terminal Failed on what is
		// almost always a transient apiserver hiccup.
		if !jobFinished {
			return ctrl.Result{}, fmt.Errorf("capture output while sidecar holds job %s open: %w", job.Name, err)
		}
		log.Error(err, "determine executor result")
		return r.markFailed(ctx, sr, fmt.Sprintf("Failed to determine executor result: %v", err))
	}

	// If the executor finished but the Job hasn't (an injected sidecar is keeping
	// the pod alive), we must tear the Job down — it would otherwise never finish, so
	// neither its TTL nor anything else would reclaim the pod. Two phases, so a
	// transient error neither loses the output nor strands the pod:
	//
	//  1. Persist outputSecret+exitCode while still in Approved. The pod (and its
	//     logs) only live while the Job exists, so deleting first and then failing the
	//     terminal status write would lose the captured result; recording it first
	//     keeps it durable (and the missing-Job path above recovers from it).
	//  2. Once the result is durable, delete the Job and finalize. The delete returns
	//     its error to retry — the request is still Approved with the result recorded,
	//     so retrying can't lose output, and we never go terminal with the pod alive.
	if !jobFinished {
		if sr.Status.ExitCode == nil {
			ec := result.ExitCode
			if err := r.updateStatus(ctx, sr, func(s *SudoRequest) {
				applyCapturedOutputStatus(s, result)
				s.Status.ExitCode = &ec
			}); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		if err := r.stopJob(ctx, job); err != nil {
			return ctrl.Result{}, fmt.Errorf("stop job %s after executor completion (injected sidecar?): %w", job.Name, err)
		}
	}

	applyCapturedOutputStatus(sr, result)
	return r.finalizeFromRecordedResult(ctx, sr)
}

func applyCapturedOutputStatus(sr *SudoRequest, result capturedOutput) {
	ec := result.ExitCode
	sr.Status.ExitCode = &ec
	sr.Status.OutputSecretRef = result.SecretRef
	sr.Status.OutputCaptureState = result.CaptureState
	sr.Status.OutputDeliveryState = result.DeliveryState
	sr.Status.OutputFailureReason = result.FailureReason
	sr.Status.OutputTotalBytes = result.TotalBytes
	sr.Status.OutputRetainedBytes = result.RetainedBytes
	sr.Status.OutputSHA256 = result.SHA256
}

// finalizeFromRecordedResult writes the terminal phase from a result already present
// in status (outputSecret + exitCode). The normal completion path sets those fields
// then calls this; the missing-Job path calls it to recover when the sidecar two-phase
// had already persisted the result before the Job was torn down (so the Job being gone
// is our own teardown, not a lost execution).
func (r *SudoRequestReconciler) finalizeFromRecordedResult(ctx context.Context, sr *SudoRequest) (ctrl.Result, error) {
	exitCode := *sr.Status.ExitCode
	outputSecret := sr.Status.OutputSecretRef
	outputCaptureState := sr.Status.OutputCaptureState
	outputDeliveryState := sr.Status.OutputDeliveryState
	outputFailureReason := sr.Status.OutputFailureReason
	outputTotalBytes := sr.Status.OutputTotalBytes
	outputRetainedBytes := sr.Status.OutputRetainedBytes
	outputSHA256 := sr.Status.OutputSHA256
	if err := r.updateStatus(ctx, sr, func(s *SudoRequest) {
		// Re-apply the result too, not just the phase: in the normal completion path
		// the caller set ExitCode/OutputSecretRef in memory but never persisted them,
		// so a conflict-driven refetch would otherwise drop them.
		ec := exitCode
		s.Status.ExitCode = &ec
		s.Status.OutputSecretRef = outputSecret
		s.Status.OutputCaptureState = outputCaptureState
		s.Status.OutputDeliveryState = outputDeliveryState
		s.Status.OutputFailureReason = outputFailureReason
		s.Status.OutputTotalBytes = outputTotalBytes
		s.Status.OutputRetainedBytes = outputRetainedBytes
		s.Status.OutputSHA256 = outputSHA256
		if exitCode == 0 {
			s.Status.Phase = PhaseExecuted
		} else {
			s.Status.Phase = PhaseFailed
		}
	}); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(sr, corev1.EventTypeNormal, "Executed",
		"Command finished exit=%d output=%s (len%shash%s)",
		exitCode, outputSecret,
		// Length and hash only; output bytes are never logged.
		"<redacted>", "<redacted>")
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type:                "phase",
		Phase:               sr.Status.Phase,
		ExitCode:            &exitCode,
		OutputSecretRef:     outputSecret,
		OutputCaptureState:  sr.Status.OutputCaptureState,
		OutputDeliveryState: sr.Status.OutputDeliveryState,
		OutputFailureReason: sr.Status.OutputFailureReason,
		OutputTotalBytes:    sr.Status.OutputTotalBytes,
		OutputRetainedBytes: sr.Status.OutputRetainedBytes,
		OutputSHA256:        sr.Status.OutputSHA256,
		Requester:           sr.Spec.Requester,
		Reason:              sr.Spec.Reason,
		Command:             sr.Spec.Command,
		CreatedAt:           sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
		RetryOfUID:          sr.Spec.RetryOfUID,
	})
	return ctrl.Result{}, nil
}

// Approve flips Pending -> Approved and stamps approvedBy/approvedAt.
// Called from the HTTP approve handler after JWT verification.
func (r *SudoRequestReconciler) Approve(ctx context.Context, uid types.UID, approvedBy string) error {
	sr, err := r.findByUID(ctx, uid)
	if err != nil {
		return err
	}
	if sr.Status.Phase != PhasePending {
		return fmt.Errorf("request not pending (phase=%s)", sr.Status.Phase)
	}
	now := metav1.NewTime(time.Now())
	tokenSecretName := sr.Status.ApprovalTokenSecretName
	sr.Status.Phase = PhaseApproved
	sr.Status.ApprovedBy = approvedBy
	sr.Status.ApprovedAt = &now
	// Burn the approval token.
	sr.Status.ApprovalTokenHash = ""
	sr.Status.ApprovalTokenExpiresAt = nil
	sr.Status.ApprovalTokenSecretName = ""
	if err := r.Status().Update(ctx, sr); err != nil {
		return err
	}
	r.deleteApprovalTokenSecret(ctx, sr.Namespace, tokenSecretName)
	r.Recorder.Eventf(sr, corev1.EventTypeNormal, "Approved", "Approved by %s", approvedBy)
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type:       "phase",
		Phase:      PhaseApproved,
		ApprovedBy: approvedBy,
		Requester:  sr.Spec.Requester,
		Reason:     sr.Spec.Reason,
		Command:    sr.Spec.Command,
		CreatedAt:  sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
		RetryOfUID: sr.Spec.RetryOfUID,
	})
	return nil
}

// Deny flips Pending -> Denied.
func (r *SudoRequestReconciler) Deny(ctx context.Context, uid types.UID, deniedBy, reason string) error {
	sr, err := r.findByUID(ctx, uid)
	if err != nil {
		return err
	}
	if sr.Status.Phase != PhasePending {
		return fmt.Errorf("request not pending (phase=%s)", sr.Status.Phase)
	}
	now := metav1.NewTime(time.Now())
	tokenSecretName := sr.Status.ApprovalTokenSecretName
	sr.Status.Phase = PhaseDenied
	sr.Status.DeniedBy = deniedBy
	sr.Status.DeniedAt = &now
	sr.Status.DenialReason = reason
	sr.Status.ApprovalTokenHash = ""
	sr.Status.ApprovalTokenExpiresAt = nil
	sr.Status.ApprovalTokenSecretName = ""
	if err := r.Status().Update(ctx, sr); err != nil {
		return err
	}
	r.deleteApprovalTokenSecret(ctx, sr.Namespace, tokenSecretName)
	r.Recorder.Eventf(sr, corev1.EventTypeWarning, "Denied", "Denied by %s: %s", deniedBy, reason)
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type:         "phase",
		Phase:        PhaseDenied,
		DeniedBy:     deniedBy,
		DenialReason: reason,
		Requester:    sr.Spec.Requester,
		Reason:       sr.Spec.Reason,
		Command:      sr.Spec.Command,
		CreatedAt:    sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
		RetryOfUID:   sr.Spec.RetryOfUID,
	})
	return nil
}

// VerifyApprovalToken returns the SudoRequest if (a) it's Pending, (b) the token's hash matches,
// and (c) the token has not expired.
func (r *SudoRequestReconciler) VerifyApprovalToken(ctx context.Context, uid types.UID, token string) (*SudoRequest, error) {
	sr, err := r.findByUID(ctx, uid)
	if err != nil {
		return nil, err
	}
	if sr.Status.Phase != PhasePending {
		return nil, fmt.Errorf("not pending (phase=%s)", sr.Status.Phase)
	}
	if sr.Status.ApprovalTokenExpiresAt == nil || time.Now().After(sr.Status.ApprovalTokenExpiresAt.Time) {
		return nil, errApprovalTokenExpired
	}
	if !constantTimeEqual(sha256Hex(token), sr.Status.ApprovalTokenHash) {
		return nil, fmt.Errorf("invalid approval token")
	}
	return sr, nil
}

func (r *SudoRequestReconciler) findByUID(ctx context.Context, uid types.UID) (*SudoRequest, error) {
	var list SudoRequestList
	if err := r.List(ctx, &list, client.InNamespace(r.controllerNamespace())); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].UID == uid {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("not found: %s", uid)
}

func (r *SudoRequestReconciler) deleteApprovalTokenSecret(ctx context.Context, namespace, name string) {
	if name == "" {
		return
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).Error(err, "delete spent approval-token Secret", "secret", name)
	}
}

// --- helpers ---

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var d byte
	for i := 0; i < len(a); i++ {
		d |= a[i] ^ b[i]
	}
	return d == 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func imageFor(sr *SudoRequest) string {
	if sr.Status.ResolvedImage != "" {
		return sr.Status.ResolvedImage
	}
	if sr.Spec.Image != "" {
		return sr.Spec.Image
	}
	if profile, _, err := resolveExecutorProfile(sr); err == nil && profile != nil {
		return profile.Image
	}
	return DefaultExecutorImage
}

func profileFor(sr *SudoRequest) string {
	if sr.Status.ResolvedProfile != "" {
		return sr.Status.ResolvedProfile
	}
	if sr.Spec.Image != "" {
		return ""
	}
	if sr.Spec.Profile != "" {
		return sr.Spec.Profile
	}
	return DefaultExecutorProfile
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
