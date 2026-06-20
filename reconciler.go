package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
)

// SudoRequestReconciler drives the state machine:
//
//	(no status)  -> Pending  (mint token, send Pushbullet push)
//	Pending      -> Expired  (after 1h)
//	Approved     -> Executed (create Job, capture output, write Secret)
//	Approved     -> Failed   (job didn't finish or pod errored)
type SudoRequestReconciler struct {
	client.Client
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

	if _, ok := getAutoApproveParsedCommand(sr.Spec.Command, imageFor(sr)); ok {
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
		sumCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		s, err := r.Summarizer.Summarize(sumCtx, sr.Spec.Command, imageFor(sr), sr.Spec.Reason)
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

func (r *SudoRequestReconciler) handleApproved(ctx context.Context, sr *SudoRequest) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

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
	if sr.Status.ExecutorJobName == "" {
		job, err := r.findOrCreateJob(ctx, sr)
		if err != nil {
			return ctrl.Result{}, err
		}
		sr.Status.ExecutorJobName = job.Name
		if err := r.Status().Update(ctx, sr); err != nil {
			return ctrl.Result{}, fmt.Errorf("record executorJobName: %w", err)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	job, err := r.getExecutorJob(ctx, sr)
	if err != nil {
		return ctrl.Result{}, err
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

	// If job is still running, requeue.
	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Job finished. Collect output and finalize.
	outputSecret, exitCode, err := r.captureJobOutput(ctx, sr, job)
	if err != nil {
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
