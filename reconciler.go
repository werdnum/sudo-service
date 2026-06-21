package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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

// SudoRequestReconciler drives the state machine:
//
//	(no status)  -> Pending  (mint token, send Pushbullet push)
//	Pending      -> Expired  (after 1h)
//	Approved     -> Executed (create Job, capture output, write Secret)
//	Approved     -> Failed   (job didn't finish or pod errored)
type SudoRequestReconciler struct {
	client.Client
	// APIReader is an uncached, direct-to-apiserver reader. The manager's cache
	// (and thus the embedded Client's reads) is scoped to ControllerNamespace, so
	// reads of executor Jobs/Pods that may live in another namespace
	// (spec.namespace) must go through this reader, not the cache.
	APIReader     client.Reader
	Scheme        *runtime.Scheme
	Pushover      *PushoverClient
	Summarizer    *Summarizer // optional; nil when AI summaries are disabled
	Broadcaster   *Broadcaster
	Recorder      record.EventRecorder
	PublicBaseURL string
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
	log := ctrl.LoggerFrom(ctx)

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
		})
		return ctrl.Result{}, nil
	}

	// Reject a spec whose widened pod fields fall outside the curated allowlist
	// (a hostPath volume, an init container setting its own securityContext, ...).
	// As with the syntax check, the HTTP API rejects these at submission; a
	// CRD-created one only reaches us here, so deny it before the approval push.
	if err := validateSpecExtras(sr); err != nil {
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
		})
		return ctrl.Result{}, nil
	}

	token, err := randomToken(32)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("mint token: %w", err)
	}
	hash := sha256Hex(token)

	expiry := metav1.NewTime(time.Now().Add(ApprovalTokenTTL * time.Second))

	// Render approval URL.
	u, err := url.Parse(r.PublicBaseURL)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("parse PublicBaseURL: %w", err)
	}
	u.Path = "/approve"
	q := u.Query()
	q.Set("id", string(sr.UID))
	q.Set("t", token)
	u.RawQuery = q.Encode()
	approvalURL := u.String()

	// Best-effort AI review aid. Generated BEFORE the push (and thus before the
	// adjacent status write) so that the instant the reviewer receives the
	// approval URL, the request is already Pending with its token hash stored.
	// Doing this slow call after the push would open a window where the
	// freshly-delivered link is unusable — VerifyApprovalToken requires Pending
	// + a stored hash, neither of which exists until the status update lands.
	// Optional and never load-bearing: a failure here logs and proceeds with no
	// summary, and a short timeout bounds it. (On a push retry the summary is
	// regenerated; that wasted call on the rare error path is worth the
	// correctness.)
	var summary string
	if r.Summarizer != nil {
		// Give the AI the ground-truth pod spec to review (not just the curated
		// summary), but redact literal env values — the summary goes to a
		// third-party endpoint, so credentials in spec.env must not leave the
		// cluster. The human push and approve page still show the raw values.
		aiContext := ""
		if hasSpecExtras(sr) {
			if tmpl, err := displayPodTemplate(sr, true); err == nil {
				aiContext = tmpl
			} else {
				aiContext = specExtrasText(sr, true)
			}
		}
		sumCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s, err := r.Summarizer.Summarize(sumCtx, sr.Spec.Command, imageFor(sr), sr.Spec.Reason, aiContext)
		cancel()
		if err != nil {
			log.Error(err, "AI command summary failed; continuing without one")
		} else {
			summary = s
		}
	}

	// Send the Pushover push first; if it fails, we don't want to mark Pending and lose the token.
	title := fmt.Sprintf("sudo: %s wants to run %s", sr.Spec.Requester, truncate(sr.Spec.Command, 80))
	body := fmt.Sprintf("reason: %s\ncommand: %s\nimage: %s",
		sr.Spec.Reason, sr.Spec.Command, imageFor(sr))
	// Redact literal env values: the push goes to the external Pushover service.
	// The OIDC-protected approve page still shows the raw values for review.
	if extras := specExtrasText(sr, true); extras != "" {
		body += "\n" + extras
	}
	reqID, err := r.Pushover.SendApproval(ctx, title, body, approvalURL)
	if err != nil {
		log.Error(err, "Pushover send failed; will retry")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	sr.Status.Phase = PhasePending
	sr.Status.ApprovalTokenHash = hash
	sr.Status.ApprovalTokenExpiresAt = &expiry
	sr.Status.PushoverRequestID = reqID
	sr.Status.Summary = summary

	if err := r.Status().Update(ctx, sr); err != nil {
		return ctrl.Result{}, fmt.Errorf("status update Pending: %w", err)
	}

	r.Recorder.Eventf(sr, corev1.EventTypeNormal, "Pending", "Awaiting human approval (pushover request=%s)", reqID)
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type:      "phase",
		Phase:     PhasePending,
		Requester: sr.Spec.Requester,
		Reason:    sr.Spec.Reason,
		Command:   sr.Spec.Command,
		CreatedAt: sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
	})

	// Wake up to enforce TTL.
	return ctrl.Result{RequeueAfter: time.Until(metav1.NewTime(time.Now().Add(PendingRequestTTL * time.Second)).Time)}, nil
}

func (r *SudoRequestReconciler) handlePending(ctx context.Context, sr *SudoRequest) (ctrl.Result, error) {
	// Expire after 1h.
	age := time.Since(sr.CreationTimestamp.Time)
	if age >= PendingRequestTTL*time.Second {
		sr.Status.Phase = PhaseExpired
		// Clear token so a stray push click can't approve.
		sr.Status.ApprovalTokenHash = ""
		sr.Status.ApprovalTokenExpiresAt = nil
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(sr, corev1.EventTypeWarning, "Expired", "Request expired without approval after %s", age)
		r.Broadcaster.Publish(string(sr.UID), Event{
			Type:      "phase",
			Phase:     PhaseExpired,
			Requester: sr.Spec.Requester,
			Reason:    sr.Spec.Reason,
			Command:   sr.Spec.Command,
			CreatedAt: sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
		})
		return ctrl.Result{}, nil
	}
	// Requeue at expiry.
	return ctrl.Result{RequeueAfter: PendingRequestTTL*time.Second - age + time.Second}, nil
}

// failApproved moves an Approved request to Failed with a reason, emitting the
// event + broadcast the same way the other terminal transitions do.
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
	sr.Status.Phase = PhaseFailed
	if err := r.Status().Update(ctx, sr); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(sr, corev1.EventTypeWarning, "Failed", "%s", reason)
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type:      "phase",
		Phase:     PhaseFailed,
		Requester: sr.Spec.Requester,
		Reason:    sr.Spec.Reason,
		Command:   sr.Spec.Command,
		CreatedAt: sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
	})
	return ctrl.Result{}, nil
}

func (r *SudoRequestReconciler) handleApproved(ctx context.Context, sr *SudoRequest) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// A cross-namespace executor Job carries no ownerRef, so it won't cascade when
	// the SudoRequest is deleted. Add the cleanup finalizer before creating any
	// Job (same-namespace Jobs cascade via their ownerRef and don't need it).
	if executorNamespace(sr) != ControllerNamespace && !controllerutil.ContainsFinalizer(sr, executorCleanupFinalizer) {
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
		sr.Status.ExecutorJobName = "sudo-exec-" + jobSuffix
		if sr.Spec.Stdin != "" {
			stdinSuffix, err := randomToken(8)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("mint stdin secret name: %w", err)
			}
			sr.Status.StdinSecretName = "sudo-stdin-" + stdinSuffix
		}
		if err := r.Status().Update(ctx, sr); err != nil {
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
				errors.Is(err, errForeignChildObject) {
				return r.failApproved(ctx, sr, fmt.Sprintf("executor Job rejected: %v", err))
			}
			return ctrl.Result{}, err
		}
		sr.Status.ExecutorJobUID = string(job.UID)
		if err := r.Status().Update(ctx, sr); err != nil {
			// Roll back the Job we just created. Otherwise the next reconcile
			// re-enters with ExecutorJobUID still empty, finds this un-recorded Job,
			// and (cross-namespace, where we can't authenticate it) fails the request
			// as "foreign" — a transient status-write error would spuriously fail a
			// legitimate request. Deleting it gives the retry a clean create.
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
		// Job has been GC'd before we observed completion. Treat as Failed
		// rather than re-creating (which would re-run the privileged command).
		log.Info("executor Job missing before completion; failing request to prevent replay",
			"jobName", sr.Status.ExecutorJobName)
		sr.Status.Phase = PhaseFailed
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(sr, corev1.EventTypeWarning, "Failed",
			"Executor Job %s disappeared before controller observed completion (likely ttlSecondsAfterApproval too short)",
			sr.Status.ExecutorJobName)
		r.Broadcaster.Publish(string(sr.UID), Event{
			Type:      "phase",
			Phase:     PhaseFailed,
			Requester: sr.Spec.Requester,
			Reason:    sr.Spec.Reason,
			Command:   sr.Spec.Command,
			CreatedAt: sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
		})
		return ctrl.Result{}, nil
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
	outputSecret, exitCode, err := r.captureJobOutput(ctx, sr, job)
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
		log.Error(err, "capture job output")
		sr.Status.Phase = PhaseFailed
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(sr, corev1.EventTypeWarning, "Failed", "Failed to capture output: %v", err)
		r.Broadcaster.Publish(string(sr.UID), Event{
			Type:      "phase",
			Phase:     PhaseFailed,
			Requester: sr.Spec.Requester,
			Reason:    sr.Spec.Reason,
			Command:   sr.Spec.Command,
			CreatedAt: sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
		})
		return ctrl.Result{}, nil
	}

	// If the executor finished but the Job hasn't (an injected sidecar is keeping
	// the pod alive), delete the Job so the pod is torn down — it would otherwise
	// never finish, so neither its TTL nor anything else would reclaim it. A
	// transient delete failure must not be swallowed: the request would go terminal
	// (never reconciled again), leaving the privileged pod and its mounted data
	// alive, so return the error and retry before finalizing.
	if !jobFinished {
		if err := r.stopJob(ctx, job); err != nil {
			return ctrl.Result{}, fmt.Errorf("stop job %s after executor completion (injected sidecar?): %w", job.Name, err)
		}
	}

	sr.Status.ExitCode = &exitCode
	sr.Status.OutputSecretRef = outputSecret
	if exitCode == 0 {
		sr.Status.Phase = PhaseExecuted
	} else {
		sr.Status.Phase = PhaseFailed
	}
	if err := r.Status().Update(ctx, sr); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(sr, corev1.EventTypeNormal, "Executed",
		"Command finished exit=%d output=%s (len%shash%s)",
		exitCode, outputSecret,
		// Length and hash only; output bytes are never logged.
		"<redacted>", "<redacted>")
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type:            "phase",
		Phase:           sr.Status.Phase,
		ExitCode:        &exitCode,
		OutputSecretRef: outputSecret,
		Requester:       sr.Spec.Requester,
		Reason:          sr.Spec.Reason,
		Command:         sr.Spec.Command,
		CreatedAt:       sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
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
	sr.Status.Phase = PhaseApproved
	sr.Status.ApprovedBy = approvedBy
	sr.Status.ApprovedAt = &now
	// Burn the approval token.
	sr.Status.ApprovalTokenHash = ""
	sr.Status.ApprovalTokenExpiresAt = nil
	if err := r.Status().Update(ctx, sr); err != nil {
		return err
	}
	r.Recorder.Eventf(sr, corev1.EventTypeNormal, "Approved", "Approved by %s", approvedBy)
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type:       "phase",
		Phase:      PhaseApproved,
		ApprovedBy: approvedBy,
		Requester:  sr.Spec.Requester,
		Reason:     sr.Spec.Reason,
		Command:    sr.Spec.Command,
		CreatedAt:  sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
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
	sr.Status.Phase = PhaseDenied
	sr.Status.DeniedBy = deniedBy
	sr.Status.DeniedAt = &now
	sr.Status.DenialReason = reason
	sr.Status.ApprovalTokenHash = ""
	sr.Status.ApprovalTokenExpiresAt = nil
	if err := r.Status().Update(ctx, sr); err != nil {
		return err
	}
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
		return nil, fmt.Errorf("approval token expired")
	}
	if !constantTimeEqual(sha256Hex(token), sr.Status.ApprovalTokenHash) {
		return nil, fmt.Errorf("invalid approval token")
	}
	return sr, nil
}

func (r *SudoRequestReconciler) findByUID(ctx context.Context, uid types.UID) (*SudoRequest, error) {
	var list SudoRequestList
	if err := r.List(ctx, &list, client.InNamespace(ControllerNamespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].UID == uid {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("not found: %s", uid)
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
	if sr.Spec.Image != "" {
		return sr.Spec.Image
	}
	return DefaultExecutorImage
}
