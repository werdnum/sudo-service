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
	"fmt"
	"os"
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
	out, err := runKubectl(stdin, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func runKubectl(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// TestIntegrationRetryLineageAdmission proves that immutable retry lineage can
// only be minted by the chart's controller identity. Ordinary direct writers
// may still create their own first-generation requests, and the controller path
// remains valid. A non-default namespace exercises the identity templating
// rather than accidentally relying on the chart's default namespace.
func TestIntegrationRetryLineageAdmission(t *testing.T) {
	if _, err := ctrl.GetConfig(); err != nil {
		t.Skipf("no cluster available (set KUBECONFIG): %v", err)
	}

	const namespace = "sudo-retry-admission"
	const requester = "system:serviceaccount:sudo-retry-admission:requester"
	const controller = "system:serviceaccount:sudo-retry-admission:sudo-service-controller-sa"

	kubectl(t, "", "apply", "-f", "charts/sudo-service/templates/crd-sudorequest.yaml")
	kubectl(t, "", "wait", "--for=condition=Established",
		"crd/sudorequests.sudo.andrewgarrett.dev", "--timeout=60s")
	kubectl(t, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+namespace+"\n",
		"apply", "-f", "-")

	policy, err := os.ReadFile("charts/sudo-service/templates/validatingadmissionpolicy-requester.yaml")
	if err != nil {
		t.Fatalf("read requester admission policy: %v", err)
	}
	renderedPolicy := strings.ReplaceAll(string(policy),
		`{{ include "sudo-service.controllerUsername" . }}`, controller)
	if strings.Contains(renderedPolicy, "{{") {
		t.Fatal("requester admission policy contains an unrendered Helm expression")
	}
	kubectl(t, renderedPolicy, "apply", "-f", "-")
	kubectl(t, "", "apply", "-f",
		"charts/sudo-service/templates/validatingadmissionpolicybinding-requester.yaml")
	t.Cleanup(func() {
		_, _ = runKubectl("", "delete", "validatingadmissionpolicybinding",
			"sudo-service-requester-validation", "--ignore-not-found")
		_, _ = runKubectl("", "delete", "validatingadmissionpolicy",
			"sudo-service-requester-validation", "--ignore-not-found")
	})

	rbac := fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: retry-admission-writer
  namespace: %s
rules:
  - apiGroups: ["sudo.andrewgarrett.dev"]
    resources: ["sudorequests"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: retry-admission-requester
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: retry-admission-writer
subjects:
  - kind: User
    name: %s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: retry-admission-controller
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: retry-admission-writer
subjects:
  - kind: User
    name: %s
`, namespace, namespace, requester, namespace, controller)
	kubectl(t, rbac, "apply", "-f", "-")

	request := func(name, actor, specRequester, retryOf string) (string, error) {
		retryField := ""
		if retryOf != "" {
			retryField = "  retryOfUID: " + retryOf + "\n"
		}
		manifest := fmt.Sprintf(`apiVersion: sudo.andrewgarrett.dev/v1alpha1
kind: SudoRequest
metadata:
  name: %s
  namespace: %s
spec:
  requester: %s
%s  reason: admission test
  command: "true"
`, name, namespace, specRequester, retryField)
		return runKubectl(manifest, "--as="+actor, "create", "--dry-run=server", "-f", "-")
	}

	if out, err := request("ordinary", requester, requester, ""); err != nil {
		t.Fatalf("ordinary requester create was rejected: %v\n%s", err, out)
	}
	if out, err := request("forged-retry", requester, requester, "forged-predecessor"); err == nil ||
		!strings.Contains(out, "spec.retryOfUID is controller-owned") {
		t.Fatalf("direct writer forged retry lineage: err=%v\n%s", err, out)
	}
	if out, err := request("controller-retry", controller, requester, "real-predecessor"); err != nil {
		t.Fatalf("rendered controller identity could not create retry lineage: %v\n%s", err, out)
	}
}

// TestIntegrationExecutorPodAdmission proves both sides of the Pod admission
// boundary against a real apiserver: a namespace pod-writer cannot directly use
// the cluster-admin executor SA, while the real Job controller can still create
// a correctly-owned Pod for a Job that uses it.
func TestIntegrationExecutorPodAdmission(t *testing.T) {
	if _, err := ctrl.GetConfig(); err != nil {
		t.Skipf("no cluster available (set KUBECONFIG): %v", err)
	}

	kubectl(t, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+ControllerNamespace+"\n",
		"apply", "-f", "-")

	policy, err := os.ReadFile("charts/sudo-service/templates/validatingadmissionpolicy-executor-pod.yaml")
	if err != nil {
		t.Fatalf("read Pod admission policy: %v", err)
	}
	renderedPolicy := strings.ReplaceAll(string(policy), "{{ .Values.namespace }}", ControllerNamespace)
	if strings.Contains(renderedPolicy, "{{") {
		t.Fatal("Pod admission policy contains an unrendered Helm expression")
	}
	kubectl(t, renderedPolicy, "apply", "-f", "-")
	kubectl(t, "", "apply", "-f",
		"charts/sudo-service/templates/validatingadmissionpolicybinding-executor-pod.yaml")
	t.Cleanup(func() {
		_, _ = runKubectl("", "delete", "validatingadmissionpolicybinding",
			"sudo-service-executor-pod-restrictions", "--ignore-not-found")
		_, _ = runKubectl("", "delete", "validatingadmissionpolicy",
			"sudo-service-executor-pod-restrictions", "--ignore-not-found")
	})

	setup := `apiVersion: v1
kind: ServiceAccount
metadata:
  name: sudo-service-executor-sa
  namespace: sudo-service
---
apiVersion: v1
kind: ServiceAccount
metadata:
  # Namespace creation and the ServiceAccount controller are asynchronous. Make
  # the ordinary-Pod control case deterministic instead of racing creation of
  # the namespace's implicit default ServiceAccount.
  name: default
  namespace: sudo-service
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: executor-pod-admission-test-writer
  namespace: sudo-service
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["create"]
  # Lets the test submit blockOwnerDeletion=true on its forged Job owner. The
  # OwnerReferencesPermissionEnforcement plugin requires update on the owner's
  # finalizers subresource and runs before VAP evaluation; grant it here so the
  # test proves the VAP identity denial rather than an earlier authz denial.
  - apiGroups: ["batch"]
    resources: ["jobs/finalizers"]
    verbs: ["update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: executor-pod-admission-test-writer
  namespace: sudo-service
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: executor-pod-admission-test-writer
subjects:
  - kind: User
    name: executor-pod-attacker
`
	kubectl(t, setup, "apply", "-f", "-")
	t.Cleanup(func() {
		_, _ = runKubectl("", "delete", "rolebinding", "executor-pod-admission-test-writer",
			"-n", ControllerNamespace, "--ignore-not-found")
		_, _ = runKubectl("", "delete", "role", "executor-pod-admission-test-writer",
			"-n", ControllerNamespace, "--ignore-not-found")
	})

	// The ownerReference and labels are intentionally perfect forgeries. Admission
	// must still reject them because request.userInfo is not the Job controller.
	forgedPod := `apiVersion: v1
kind: Pod
metadata:
  name: forged-executor-pod
  namespace: sudo-service
  labels:
    app: sudo-service-executor
    role: executor
    batch.kubernetes.io/job-name: forged-job
    batch.kubernetes.io/controller-uid: 11111111-1111-1111-1111-111111111111
  ownerReferences:
    - apiVersion: batch/v1
      kind: Job
      name: forged-job
      uid: 11111111-1111-1111-1111-111111111111
      controller: true
      blockOwnerDeletion: true
spec:
  serviceAccountName: sudo-service-executor-sa
  restartPolicy: Never
  containers:
    - name: executor
      image: busybox:1.36
      command: ["true"]
`
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, createErr := runKubectl(forgedPod, "--as=executor-pod-attacker", "create",
			"--dry-run=server", "-f", "-")
		if createErr != nil && strings.Contains(out, "Only the Kubernetes Job controller") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("forged direct executor Pod was not rejected by the identity check: err=%v\n%s",
				createErr, out)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// A non-executor Pod is outside this policy and remains creatable by the same
	// namespace writer.
	ordinaryPod := `apiVersion: v1
kind: Pod
metadata:
  name: ordinary-pod
  namespace: sudo-service
spec:
  restartPolicy: Never
  containers:
    - name: ordinary
      image: busybox:1.36
      command: ["true"]
`
	if out, err := runKubectl(ordinaryPod, "--as=executor-pod-attacker", "create",
		"--dry-run=server", "-f", "-"); err != nil {
		t.Fatalf("ordinary Pod should not match executor policy: %v\n%s", err, out)
	}

	// Finally use a real Job. Its Pod is created under whichever of the two
	// upstream controller-manager identities this cluster uses, with authoritative
	// Job ownership and labels generated by Kubernetes itself.
	job := fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: executor-pod-admission-test
  namespace: %s
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: sudo-service-executor
        role: executor
    spec:
      serviceAccountName: sudo-service-executor-sa
      restartPolicy: Never
      containers:
        - name: executor
          image: busybox:1.36
          command: ["sh", "-c", "sleep 30"]
`, ControllerNamespace)
	kubectl(t, job, "apply", "-f", "-")
	t.Cleanup(func() {
		_, _ = runKubectl("", "delete", "job", "executor-pod-admission-test",
			"-n", ControllerNamespace, "--ignore-not-found", "--wait=false")
	})

	deadline = time.Now().Add(30 * time.Second)
	for {
		out, getErr := runKubectl("", "get", "pods", "-n", ControllerNamespace,
			"-l", "job-name=executor-pod-admission-test", "-o", "name")
		if getErr == nil && strings.TrimSpace(out) != "" {
			break
		}
		if time.Now().After(deadline) {
			describe, _ := runKubectl("", "describe", "job", "executor-pod-admission-test",
				"-n", ControllerNamespace)
			t.Fatalf("Job controller could not create admitted executor Pod: %v\n%s", getErr, describe)
		}
		time.Sleep(500 * time.Millisecond)
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
	if final.Status.OutputCaptureState != OutputCaptureComplete || final.Status.OutputDeliveryState != OutputDeliveryAvailable {
		t.Fatalf("unexpected output state: capture=%q delivery=%q reason=%q",
			final.Status.OutputCaptureState, final.Status.OutputDeliveryState, final.Status.OutputFailureReason)
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
