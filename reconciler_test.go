package main

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := addKnownTypes(s); err != nil {
		t.Fatalf("add sudo types: %v", err)
	}
	return s
}

func conflictErr(name string) error {
	return apierrors.NewConflict(
		schema.GroupResource{Group: GroupName, Resource: "sudorequests"},
		name, errors.New("the object has been modified; please apply your changes to the latest version and try again"))
}

// TestUpdateStatusRetriesOnConflict is the core of the issue-#20 fix: a stale-object
// conflict on a status write must be transparently recovered (refetch + retry), not
// surfaced as an error, and the mutation must land on the persisted object.
func TestUpdateStatusRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "req1", Namespace: DefaultControllerNamespace, UID: "uid-1"},
		Status:     SudoRequestStatus{Phase: PhaseApproved},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&SudoRequest{}).
		WithObjects(sr).
		Build()

	injected := 0
	cl := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if injected == 0 {
				injected++
				return conflictErr(obj.GetName())
			}
			return c.Status().Update(ctx, obj, opts...)
		},
	})
	r := &SudoRequestReconciler{Client: cl, APIReader: cl}

	in := sr.DeepCopy()
	err := r.updateStatus(ctx, in, func(s *SudoRequest) {
		s.Status.ExecutorJobName = "sudo-exec-abc"
	})
	if err != nil {
		t.Fatalf("updateStatus returned error on a recoverable conflict: %v", err)
	}
	if injected != 1 {
		t.Fatalf("expected exactly one injected conflict, got %d", injected)
	}
	// The mutation must be persisted...
	var got SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.ExecutorJobName != "sudo-exec-abc" {
		t.Fatalf("mutation not persisted: ExecutorJobName=%q", got.Status.ExecutorJobName)
	}
	// ...and reflected in the caller's object for its later logic.
	if in.Status.ExecutorJobName != "sudo-exec-abc" {
		t.Fatalf("caller object not updated in place: ExecutorJobName=%q", in.Status.ExecutorJobName)
	}
}

// TestHandleApprovedMintSurvivesStatusConflict reproduces issue #20: a cache-lag
// conflict on the mint-names status write previously bubbled up as "record minted
// resource names: the object has been modified" and (with the Job path) failed the
// just-approved request. With the fix the conflict is recovered and the request
// stays Approved with its names minted.
func TestHandleApprovedMintSurvivesStatusConflict(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "req2", Namespace: DefaultControllerNamespace, UID: "uid-2"},
		Spec:       SudoRequestSpec{Requester: "alice", Command: "echo hi"},
		Status:     SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "andrew"},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&SudoRequest{}).
		WithObjects(sr).
		Build()

	injected := 0
	cl := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if injected == 0 {
				injected++
				return conflictErr(obj.GetName())
			}
			return c.Status().Update(ctx, obj, opts...)
		},
	})
	r := &SudoRequestReconciler{
		Client:      cl,
		APIReader:   cl,
		Scheme:      scheme,
		Recorder:    record.NewFakeRecorder(16),
		Broadcaster: NewBroadcaster(),
	}

	res, err := r.handleApproved(ctx, sr.DeepCopy())
	if err != nil {
		t.Fatalf("handleApproved returned error on a recoverable conflict: %v", err)
	}
	if !res.Requeue {
		t.Fatalf("expected requeue after minting names, got %+v", res)
	}
	if injected != 1 {
		t.Fatalf("expected one injected conflict, got %d", injected)
	}
	var got SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != PhaseApproved {
		t.Fatalf("request should still be Approved, got %q (reason=%q)", got.Status.Phase, got.Status.FailureReason)
	}
	if got.Status.ExecutorJobName == "" {
		t.Fatalf("expected ExecutorJobName to be minted despite the conflict")
	}
}

// TestMarkFailedSetsFailureReason verifies the requester-facing reason is persisted
// for a Failed request that has no exitCode/output (issue #20).
func TestMarkFailedSetsFailureReason(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "req3", Namespace: DefaultControllerNamespace, UID: "uid-3"},
		Spec:       SudoRequestSpec{Requester: "alice", Command: "echo hi"},
		Status:     SudoRequestStatus{Phase: PhaseApproved},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&SudoRequest{}).
		WithObjects(sr).
		Build()
	r := &SudoRequestReconciler{
		Client:      cl,
		APIReader:   cl,
		Recorder:    record.NewFakeRecorder(16),
		Broadcaster: NewBroadcaster(),
	}

	const reason = "Executor Job sudo-exec-x disappeared before controller observed completion"
	if _, err := r.markFailed(ctx, sr.DeepCopy(), reason); err != nil {
		t.Fatalf("markFailed: %v", err)
	}
	var got SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != PhaseFailed {
		t.Fatalf("phase=%q, want Failed", got.Status.Phase)
	}
	if got.Status.FailureReason != reason {
		t.Fatalf("FailureReason=%q, want %q", got.Status.FailureReason, reason)
	}
}
