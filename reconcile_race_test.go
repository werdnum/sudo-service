package main

import (
	"context"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// raceTestScheme is testScheme (reconciler_test.go) plus batchv1 for executor Jobs.
func raceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := testScheme(t)
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("add batchv1: %v", err)
	}
	return s
}

// laggyCache makes SudoRequest reads lag one generation behind the apiserver,
// the way the controller-runtime manager cache lags its own writes. Every Get of
// a SudoRequest returns the snapshot captured at the *previous* Get, so a write
// followed by an immediate re-reconcile (the Approved path requeues after every
// status write) reads a pre-write object — exactly the staleness that caused
// issue #20: the reconcile re-entered the "create Job + record UID" branch after
// the UID had already been recorded, conflicted on the stale resourceVersion, and
// the rollback deleted the live Job, failing the just-approved request.
//
// All other reads (and every write) pass straight through to the apiserver, which
// is the fake client and enforces optimistic-concurrency conflicts on stale
// writes — so no conflict has to be injected; the staleness produces them
// naturally, just as in production.
type laggyCache struct {
	mu   sync.Mutex
	prev map[string]*SudoRequest
}

func (l *laggyCache) get(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	sr, ok := obj.(*SudoRequest)
	if !ok {
		return c.Get(ctx, key, obj, opts...)
	}
	var cur SudoRequest
	if err := c.Get(ctx, key, &cur); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	k := key.String()
	if p := l.prev[k]; p != nil {
		p.DeepCopyInto(sr) // serve the stale, previous-generation snapshot
	} else {
		cur.DeepCopyInto(sr) // first read of this key: nothing stale yet
	}
	l.prev[k] = cur.DeepCopy() // remember the current state to serve next time
	return nil
}

// stampOnCreate mimics apiserver admission: assign a creationTimestamp and UID to
// objects that lack them (the fake client does neither, and the reconciler needs
// job.UID for status.ExecutorJobUID and a recent job.CreationTimestamp so the
// executor-start deadline doesn't trip immediately).
func stampOnCreate(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	if ts := obj.GetCreationTimestamp(); ts.IsZero() {
		obj.SetCreationTimestamp(metav1.Now())
	}
	if obj.GetUID() == "" {
		obj.SetUID(types.UID("uid-" + obj.GetName()))
	}
	return c.Create(ctx, obj, opts...)
}

// driveApproved runs the real Reconcile loop against a lagging cache + an
// RV-enforcing apiserver and asserts the approved request never spuriously moves
// to Failed and ends up healthy: child names minted, exactly one executor Job at
// the minted name, its UID recorded. extra objects (referenced secrets) are seeded
// into the apiserver first.
func driveApproved(t *testing.T, sr *SudoRequest, extra ...client.Object) {
	t.Helper()
	ctx := context.Background()
	scheme := raceTestScheme(t)

	objs := append([]client.Object{sr}, extra...)
	apiserver := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&SudoRequest{}).
		WithObjects(objs...).
		Build()

	lag := &laggyCache{prev: map[string]*SudoRequest{}}
	cached := interceptor.NewClient(apiserver, interceptor.Funcs{
		Get:    lag.get,
		Create: stampOnCreate,
	})

	r := &SudoRequestReconciler{
		Client:      cached,
		APIReader:   apiserver,
		Scheme:      scheme,
		Recorder:    record.NewFakeRecorder(128),
		Broadcaster: NewBroadcaster(),
	}

	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sr)}
	// More than enough iterations to clear the finalizer/mint/create/record
	// transitions and settle into the waiting-for-pod steady state. The buggy
	// code failed the request within ~5 reconciles of recording the UID.
	for i := 0; i < 15; i++ {
		// Errors model transient apiserver conflicts; the real workqueue just
		// requeues, so keep going rather than aborting the test.
		_, _ = r.Reconcile(ctx, req)

		var cur SudoRequest
		if err := apiserver.Get(ctx, req.NamespacedName, &cur); err != nil {
			t.Fatalf("get after reconcile %d: %v", i, err)
		}
		if cur.Status.Phase == PhaseFailed {
			t.Fatalf("request spuriously moved to Failed after approval (iter %d): %q",
				i, cur.Status.FailureReason)
		}
	}

	var final SudoRequest
	if err := apiserver.Get(ctx, req.NamespacedName, &final); err != nil {
		t.Fatalf("get final: %v", err)
	}
	if final.Status.Phase != PhaseApproved {
		t.Fatalf("phase=%q, want Approved (still running)", final.Status.Phase)
	}
	if final.Status.ExecutorJobName == "" {
		t.Fatal("ExecutorJobName not minted")
	}
	if final.Status.ExecutorJobUID == "" {
		t.Fatal("ExecutorJobUID not recorded")
	}
	if isManagedJob(&final) && final.Status.ExecutorJobLifecycle != JobLifecycleCreated {
		t.Fatalf("managed lifecycle=%q, want Created", final.Status.ExecutorJobLifecycle)
	}

	var jobs batchv1.JobList
	if err := apiserver.List(ctx, &jobs, client.InNamespace(executorNamespace(sr))); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("want exactly one executor Job, got %d", len(jobs.Items))
	}
	if jobs.Items[0].Name != final.Status.ExecutorJobName {
		t.Fatalf("job name %q != recorded %q", jobs.Items[0].Name, final.Status.ExecutorJobName)
	}
	if string(jobs.Items[0].UID) != final.Status.ExecutorJobUID {
		t.Fatalf("job UID %q != recorded %q", jobs.Items[0].UID, final.Status.ExecutorJobUID)
	}
}

// TestApprovedPlainCommandSurvivesCacheLag reproduces issue #20 repro 1/comment-1:
// a plain same-namespace command must not fail after approval when the manager
// cache lags the controller's own status writes.
func TestApprovedPlainCommandSurvivesCacheLag(t *testing.T) {
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "http-plain",
			Namespace:       ControllerNamespace,
			UID:             "uid-plain",
			ResourceVersion: "100",
		},
		Spec: SudoRequestSpec{
			Requester: "alice",
			Reason:    "seaweedfs preflight",
			Command:   "kubectl apply -f -",
		},
		Status: SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "andrew"},
	}
	driveApproved(t, sr)
}

func TestApprovedManagedJobSurvivesCacheLagAndRecordsCreated(t *testing.T) {
	deadline := int32(3600)
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: "http-managed", Namespace: ControllerNamespace,
			UID: "uid-managed", ResourceVersion: "100",
		},
		Spec: SudoRequestSpec{
			Requester: "alice", Reason: "long drift run", Command: "ansible-playbook drift.yaml",
			Execution: SudoRequestExecution{
				Mode: ExecutionModeManagedJob, ResourceClass: ResourceClassLongRunning,
				ActiveDeadlineSeconds: &deadline,
			},
		},
		Status: SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "andrew"},
	}
	driveApproved(t, sr)
}

// TestApprovedWidenedCrossNamespaceJobSurvivesCacheLag reproduces the comment-2
// case: a cross-namespace, non-cluster-admin executor with imagePullSecrets and a
// mounted Secret. Cross-namespace adds the cleanup finalizer and, on the buggy
// stale re-read, failed the request as a "foreign" pre-existing Job.
func TestApprovedWidenedCrossNamespaceJobSurvivesCacheLag(t *testing.T) {
	sshKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ansible-drift-ssh-key", Namespace: "ansible"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"id_ed25519": []byte("key")},
	}
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "http-ansible",
			Namespace:       ControllerNamespace,
			UID:             "uid-ansible",
			ResourceVersion: "100",
		},
		Spec: SudoRequestSpec{
			Requester: "alice",
			Reason:    "seaweedfs decommission preflight",
			Command:   "ansible-playbook seaweedfs.yaml --check --diff",
			Image:     "ghcr.io/werdnum/k8s-agent@sha256:deadbeef",
			Namespace: "ansible",
			ImagePullSecrets: rawList(
				corev1.LocalObjectReference{Name: "ghcr-secret"},
			),
			Volumes: rawList(
				corev1.Volume{Name: "ssh", VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: "ansible-drift-ssh-key"},
				}},
				corev1.Volume{Name: "work", VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				}},
			),
			VolumeMounts: rawList(
				corev1.VolumeMount{Name: "ssh", MountPath: "/keys", ReadOnly: true},
				corev1.VolumeMount{Name: "work", MountPath: "/work"},
			),
			Privileges: SudoRequestPrivileges{ClusterAdmin: boolPtr(false)},
		},
		Status: SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "andrew"},
	}
	driveApproved(t, sr, sshKey)
}
