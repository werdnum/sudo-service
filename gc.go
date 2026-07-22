package main

import (
	"context"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// GarbageCollector deletes controller Secrets past their expires-at label and finished Jobs
// past TTLSecondsAfterFinished (the API server handles Jobs natively, but we double-check).
//
// Also expires Pending SudoRequests whose CreationTimestamp is older than PendingRequestTTL
// in case the per-object requeue is dropped (controller restart, etc.). When that GC path
// fires, SSE subscribers must learn about the transition the same way they would from the
// normal reconciler path — the Broadcaster reference is what lets them.
type GarbageCollector struct {
	client.Client
	Broadcaster *Broadcaster
}

var _ manager.Runnable = (*GarbageCollector)(nil)

func (g *GarbageCollector) Start(ctx context.Context) error {
	log := ctrl.LoggerFrom(ctx).WithName("gc")
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := g.sweepSecrets(ctx); err != nil {
				log.Error(err, "sweep secrets")
			}
			if err := g.expirePending(ctx); err != nil {
				log.Error(err, "expire pending")
			}
		}
	}
}

func (g *GarbageCollector) sweepSecrets(ctx context.Context) error {
	var secs corev1.SecretList
	if err := g.List(ctx, &secs,
		client.InNamespace(ControllerNamespace),
		client.MatchingLabels{"app": "sudo-service"},
	); err != nil {
		return err
	}
	now := time.Now().Unix()
	for i := range secs.Items {
		s := &secs.Items[i]
		raw := s.Labels["expires-at"]
		if raw == "" {
			continue
		}
		exp, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		if now >= exp {
			_ = g.Delete(ctx, s)
		}
	}
	return nil
}

func (g *GarbageCollector) expirePending(ctx context.Context) error {
	var list SudoRequestList
	if err := g.List(ctx, &list, client.InNamespace(ControllerNamespace)); err != nil {
		return err
	}
	now := time.Now()
	for i := range list.Items {
		sr := &list.Items[i]
		if sr.Status.Phase != PhasePending {
			continue
		}
		if now.Sub(sr.CreationTimestamp.Time) < PendingRequestTTL*time.Second {
			continue
		}
		sr.Status.Phase = PhaseExpired
		tokenSecretName := sr.Status.ApprovalTokenSecretName
		sr.Status.ApprovalTokenHash = ""
		sr.Status.ApprovalTokenExpiresAt = nil
		sr.Status.ApprovalTokenSecretName = ""
		if err := g.Status().Update(ctx, sr); err != nil {
			continue
		}
		if tokenSecretName != "" {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: sr.Namespace, Name: tokenSecretName}}
			_ = g.Delete(ctx, secret)
		}
		// Same fan-out as handlePending's expiry path, so any SSE client
		// connected to /requests/{uid}/events sees the terminal transition
		// even when this GC path is the one that fires (e.g. after a
		// controller restart dropped the per-object requeue).
		if g.Broadcaster != nil {
			g.Broadcaster.Publish(string(sr.UID), Event{
				Type:       "phase",
				Phase:      PhaseExpired,
				Requester:  sr.Spec.Requester,
				Reason:     sr.Spec.Reason,
				Command:    sr.Spec.Command,
				CreatedAt:  sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"),
				RetryOfUID: sr.Spec.RetryOfUID,
			})
		}
	}
	return nil
}
