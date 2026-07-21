package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestExecutionFingerprintIsSemanticAndSecretSafe(t *testing.T) {
	a := SudoRequestSpec{
		Requester: "requester-a", SubmittedBy: "actor-a", Reason: "first", Command: "tool run",
		Stdin: "sensitive-stdin",
		Env:   []runtime.RawExtension{{Raw: []byte(`{"value":"secret","name":"TOKEN"}`)}},
	}
	b := SudoRequestSpec{
		Requester: "requester-b", SubmittedBy: "actor-b", Reason: "second", RetryOfUID: "old",
		Command: "tool run", Stdin: "sensitive-stdin",
		Env: []runtime.RawExtension{{Raw: []byte(`{"name":"TOKEN","value":"secret"}`)}},
	}
	ttl := int32(DefaultPostApproval)
	b.Image = DefaultExecutorImage
	b.Namespace = ControllerNamespace
	b.TTLSecondsAfterApproval = &ttl
	clusterAdmin := true
	b.Privileges.ClusterAdmin = &clusterAdmin
	fa, err := executionFingerprint(&a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := executionFingerprint(&b)
	if err != nil {
		t.Fatal(err)
	}
	if fa != fb {
		t.Fatalf("semantic fingerprints differ: %s != %s", fa, fb)
	}
	if fa == "" || fa == a.Stdin || fa == "secret" {
		t.Fatalf("fingerprint exposed a value: %q", fa)
	}
	b.Stdin = "different-secret"
	fc, _ := executionFingerprint(&b)
	if fc == fa {
		t.Fatal("changing stdin did not change fingerprint")
	}
}

func TestPendingDuplicateIsRequesterScoped(t *testing.T) {
	base := SudoRequestSpec{Requester: "alice", Reason: "one", Command: "kubectl get pods", Stdin: "secret"}
	objects := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&SudoRequest{ObjectMeta: metav1.ObjectMeta{Name: "alice-pending", Namespace: ControllerNamespace, UID: "alice-uid"}, Spec: base, Status: SudoRequestStatus{Phase: PhasePending}},
		&SudoRequest{ObjectMeta: metav1.ObjectMeta{Name: "bob-pending", Namespace: ControllerNamespace, UID: "bob-uid"}, Spec: SudoRequestSpec{Requester: "bob", Command: base.Command, Stdin: base.Stdin}, Status: SudoRequestStatus{Phase: PhasePending}},
	).Build()
	api := &APIServer{Client: objects}
	candidate := &SudoRequest{Spec: base}
	candidate.Spec.Reason = "different non-execution reason"
	duplicate, err := api.findPendingDuplicate(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate == nil || duplicate.UID != "alice-uid" {
		t.Fatalf("duplicate = %#v, want alice only", duplicate)
	}
	candidate.Spec.Requester = "charlie"
	duplicate, err = api.findPendingDuplicate(context.Background(), candidate)
	if err != nil || duplicate != nil {
		t.Fatalf("cross-requester match leaked: duplicate=%#v err=%v", duplicate, err)
	}
}

func retryTestClient(t *testing.T, source *SudoRequest, failFirstLink *atomic.Bool) client.Client {
	t.Helper()
	return ctrlfake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&SudoRequest{}).WithObjects(source).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if sr, ok := obj.(*SudoRequest); ok && sr.UID == "" {
					sr.UID = types.UID("uid-" + sr.Name)
				}
				return c.Create(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if failFirstLink != nil && failFirstLink.CompareAndSwap(true, false) {
					return errors.New("injected predecessor status failure")
				}
				return c.SubResource(subresource).Update(ctx, obj, opts...)
			},
		}).Build()
}

func expiredSource() *SudoRequest {
	return &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: ControllerNamespace, UID: "source-uid"},
		Spec:       SudoRequestSpec{Requester: "alice", SubmittedBy: "alice", Reason: "try it", Command: "kubectl get pods", Stdin: "secret"},
		Status:     SudoRequestStatus{Phase: PhaseExpired},
	}
}

func TestRetryIsConcurrentAndRepeatedIdempotent(t *testing.T) {
	cl := retryTestClient(t, expiredSource(), nil)
	api := &APIServer{Client: cl}
	var wg sync.WaitGroup
	results := make(chan types.UID, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var source SudoRequest
			if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ControllerNamespace, Name: "source"}, &source); err != nil {
				t.Error(err)
				return
			}
			successor, _, err := api.retryRequest(context.Background(), &source, "alice", false)
			if err != nil {
				t.Error(err)
				return
			}
			results <- successor.UID
		}()
	}
	wg.Wait()
	close(results)
	var want types.UID
	for uid := range results {
		if want == "" {
			want = uid
		} else if uid != want {
			t.Errorf("got multiple successor UIDs: %s and %s", want, uid)
		}
	}
	var list SudoRequestList
	if err := cl.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("request count = %d, want source + one successor", len(list.Items))
	}
	var source SudoRequest
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: ControllerNamespace, Name: "source"}, &source)
	if source.Status.SupersededByUID != string(want) {
		t.Fatalf("supersededByUID = %q, want %q", source.Status.SupersededByUID, want)
	}
}

func TestRetryRecoversPredecessorLinkAfterPostCreateFailure(t *testing.T) {
	fail := &atomic.Bool{}
	fail.Store(true)
	cl := retryTestClient(t, expiredSource(), fail)
	api := &APIServer{Client: cl}
	var source SudoRequest
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: ControllerNamespace, Name: "source"}, &source)
	if _, _, err := api.retryRequest(context.Background(), &source, "alice", false); err == nil {
		t.Fatal("first retry succeeded despite injected link failure")
	}
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: ControllerNamespace, Name: "source"}, &source)
	successor, created, err := api.retryRequest(context.Background(), &source, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("recovery created a second successor")
	}
	_ = cl.Get(context.Background(), client.ObjectKey{Namespace: ControllerNamespace, Name: "source"}, &source)
	if source.Status.SupersededByUID != string(successor.UID) {
		t.Fatalf("link not repaired: %q != %q", source.Status.SupersededByUID, successor.UID)
	}
}

func TestRequesterCannotRetryDeniedOrExecuted(t *testing.T) {
	for _, phase := range []string{PhaseDenied, PhaseExecuted, PhasePending} {
		source := expiredSource()
		source.Status.Phase = phase
		api := &APIServer{Client: retryTestClient(t, source, nil)}
		if _, _, err := api.retryRequest(context.Background(), source, "alice", false); err == nil {
			t.Errorf("requester retried phase %s", phase)
		}
	}
}

func TestAdminRetryAttributesActorWithoutImpersonatingRequester(t *testing.T) {
	source := expiredSource()
	source.Status.Phase = PhaseDenied
	cl := retryTestClient(t, source, nil)
	api := &APIServer{Client: cl}
	successor, _, err := api.retryRequest(context.Background(), source, "verified-admin", true)
	if err != nil {
		t.Fatal(err)
	}
	if successor.Spec.Requester != "alice" || successor.Spec.SubmittedBy != "verified-admin" {
		t.Fatalf("attribution = requester %q submittedBy %q", successor.Spec.Requester, successor.Spec.SubmittedBy)
	}
	if successor.Spec.RetryOfUID != string(source.UID) {
		t.Fatalf("retryOfUID = %q", successor.Spec.RetryOfUID)
	}
}
