//go:build integration

// Package main integration test. Unlike the unit tests (which model the manager
// cache with a fake client), this stands up the REAL controller-runtime manager —
// real informer cache, real workqueue, real optimistic-concurrency conflicts —
// against a real Kubernetes apiserver, and runs an approved request end-to-end:
// a real executor Job, a real pod, real captured logs.
//
// It needs a cluster (KUBECONFIG). CI provisions one with k3d/kind; locally:
//
//	k3d cluster create sudo-it
//	export KUBECONFIG=$(k3d kubeconfig write sudo-it)
//	go test -tags integration -run TestIntegration -v .
//
// The build tag keeps it out of the default `go test ./...`.
package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func kubectl(t *testing.T, stdin string, args ...string) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
}

// TestIntegrationApprovedRequestExecutes drives an approved SudoRequest through
// the real manager to Executed and verifies the captured output. The request is
// created and flipped to Approved directly (before the manager starts), so the
// human-approval / Pushover path is bypassed — this exercises the post-approval
// reconcile that issue #20 broke.
func TestIntegrationApprovedRequestExecutes(t *testing.T) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		t.Skipf("no cluster available (set KUBECONFIG): %v", err)
	}

	// Install the CRD and the controller namespace the manager cache is scoped to.
	kubectl(t, "", "apply", "-f", "charts/sudo-service/templates/crd-sudorequest.yaml")
	kubectl(t, "", "wait", "--for=condition=Established",
		"crd/sudorequests.sudo.andrewgarrett.dev", "--timeout=60s")
	kubectl(t, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+ControllerNamespace+"\n",
		"apply", "-f", "-")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Direct (uncached) client to seed the object before the manager runs.
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	const marker = "hello-from-integration-test"
	noClusterAdmin := false
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "it-", Namespace: ControllerNamespace},
		Spec: SudoRequestSpec{
			Requester: "integration",
			Reason:    "integration test",
			Command:   "echo " + marker,
			// Small, fast-pulling image with a POSIX shell; the executor runs
			// `/bin/sh -c <command>`.
			Image: "busybox:1.36",
			// Run under the namespace default SA (no cluster-admin), so the test
			// needs no executor SA/RBAC; echo touches no API.
			Privileges: SudoRequestPrivileges{ClusterAdmin: &noClusterAdmin},
		},
	}
	if err := c.Create(ctx, sr); err != nil {
		t.Fatalf("create SudoRequest: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), sr) })

	now := metav1.NewTime(time.Now())
	sr.Status.Phase = PhaseApproved
	sr.Status.ApprovedBy = "integration"
	sr.Status.ApprovedAt = &now
	if err := c.Status().Update(ctx, sr); err != nil {
		t.Fatalf("approve SudoRequest: %v", err)
	}

	// Build the manager exactly like main.go (namespace-scoped cache, metrics off).
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         scheme,
		Cache:          cache.Options{DefaultNamespaces: map[string]cache.Config{ControllerNamespace: {}}},
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	reconciler := &SudoRequestReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		// Never called on the Approved path; non-nil for safety.
		Pushover:    NewPushoverClient("", ""),
		Broadcaster: NewBroadcaster(),
		Recorder:    mgr.GetEventRecorderFor("sudo-service-it"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler: %v", err)
	}
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager stopped: %v", err)
		}
	}()

	key := client.ObjectKeyFromObject(sr)
	var final SudoRequest
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if err := c.Get(ctx, key, &final); err != nil {
			t.Fatalf("get: %v", err)
		}
		if final.Status.Phase == PhaseExecuted || final.Status.Phase == PhaseFailed {
			break
		}
		time.Sleep(2 * time.Second)
	}

	if final.Status.Phase != PhaseExecuted {
		// Surface diagnostics for a failed run.
		dumpDiagnostics(t, c, &final)
		t.Fatalf("phase=%q (want Executed), failureReason=%q", final.Status.Phase, final.Status.FailureReason)
	}
	if final.Status.ExitCode == nil || *final.Status.ExitCode != 0 {
		t.Fatalf("exitCode=%v (want 0)", final.Status.ExitCode)
	}
	if final.Status.OutputSecretRef == "" {
		t.Fatal("no output secret recorded")
	}

	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ControllerNamespace, Name: final.Status.OutputSecretRef}, &sec); err != nil {
		t.Fatalf("get output secret: %v", err)
	}
	if got := string(sec.Data["output"]); !strings.Contains(got, marker) {
		t.Fatalf("output %q does not contain %q", got, marker)
	}
}

func dumpDiagnostics(t *testing.T, c client.Client, sr *SudoRequest) {
	t.Helper()
	ctx := context.Background()
	if sr.Status.ExecutorJobName != "" {
		var pods corev1.PodList
		_ = c.List(ctx, &pods, client.InNamespace(executorNamespace(sr)),
			client.MatchingLabels{"job-name": sr.Status.ExecutorJobName})
		for i := range pods.Items {
			p := &pods.Items[i]
			t.Logf("pod %s phase=%s reason=%s", p.Name, p.Status.Phase, p.Status.Reason)
			for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
				if cs.State.Waiting != nil {
					t.Logf("  container %s waiting: %s %s", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
				}
				if cs.State.Terminated != nil {
					t.Logf("  container %s terminated: exit=%d reason=%s", cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
				}
			}
		}
	}
}
