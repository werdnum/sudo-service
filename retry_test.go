package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	b.Profile = DefaultExecutorProfile
	b.Namespace = ControllerNamespace
	b.TTLSecondsAfterApproval = &ttl
	clusterAdmin := true
	b.Privileges.ClusterAdmin = &clusterAdmin
	fa, err := executionFingerprint(&SudoRequest{Spec: a})
	if err != nil {
		t.Fatal(err)
	}
	fb, err := executionFingerprint(&SudoRequest{Spec: b})
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
	fc, _ := executionFingerprint(&SudoRequest{Spec: b})
	if fc == fa {
		t.Fatal("changing stdin did not change fingerprint")
	}
}

func TestExecutionFingerprintNormalizesDefaultProfileAndPinsResolvedImage(t *testing.T) {
	implicit := &SudoRequest{Spec: SudoRequestSpec{Command: "kubectl get pods"}}
	explicit := &SudoRequest{Spec: SudoRequestSpec{Command: "kubectl get pods", Profile: DefaultExecutorProfile}}
	implicitFingerprint, err := executionFingerprint(implicit)
	if err != nil {
		t.Fatal(err)
	}
	explicitFingerprint, err := executionFingerprint(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if implicitFingerprint != explicitFingerprint {
		t.Fatal("omitted and explicit default profiles produced different fingerprints")
	}

	reviewed := explicit.DeepCopy()
	reviewed.Status.ResolvedImage = "registry.example/executor@sha256:previously-reviewed"
	reviewedFingerprint, err := executionFingerprint(reviewed)
	if err != nil {
		t.Fatal(err)
	}
	if reviewedFingerprint == explicitFingerprint {
		t.Fatal("resolved image change did not change execution fingerprint")
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

func TestDuplicateDetectionIncludesApprovedRequests(t *testing.T) {
	spec := SudoRequestSpec{Requester: "alice", Command: "kubectl get pods"}
	approved := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "approved", Namespace: ControllerNamespace, UID: "approved-uid"},
		Spec:       spec,
		Status:     SudoRequestStatus{Phase: PhaseApproved, ResolvedImage: DefaultExecutorImage},
	}
	cl := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(approved).Build()
	duplicate, err := (&APIServer{Client: cl}).findPendingDuplicate(
		context.Background(), &SudoRequest{Spec: spec},
	)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate == nil || duplicate.UID != approved.UID {
		t.Fatalf("duplicate = %#v, want approved request %s", duplicate, approved.UID)
	}
}

func TestDuplicateDetectionUsesUncachedReader(t *testing.T) {
	spec := SudoRequestSpec{Requester: "alice", Command: "kubectl get pods"}
	pending := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: ControllerNamespace, UID: "pending-uid"},
		Spec:       spec,
		Status:     SudoRequestStatus{Phase: PhasePending, ResolvedImage: DefaultExecutorImage},
	}
	staleCache := ctrlfake.NewClientBuilder().WithScheme(scheme).Build()
	directReader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(pending).Build()
	duplicate, err := (&APIServer{Client: staleCache, APIReader: directReader}).findPendingDuplicate(
		context.Background(), &SudoRequest{Spec: spec},
	)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate == nil || duplicate.UID != pending.UID {
		t.Fatalf("duplicate = %#v, want uncached request %s", duplicate, pending.UID)
	}
}

func TestFindByUIDUsesUncachedReader(t *testing.T) {
	want := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: ControllerNamespace, UID: "direct-uid"},
		Spec:       SudoRequestSpec{Requester: "alice", Command: "kubectl get pods"},
	}
	staleCache := ctrlfake.NewClientBuilder().WithScheme(scheme).Build()
	directReader := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(want).Build()
	got, err := (&APIServer{Client: staleCache, APIReader: directReader}).findByUID(
		context.Background(), want.UID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != want.UID {
		t.Fatalf("findByUID returned %s, want %s", got.UID, want.UID)
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
	type result struct {
		uid     types.UID
		created bool
	}
	results := make(chan result, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var source SudoRequest
			if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ControllerNamespace, Name: "source"}, &source); err != nil {
				t.Error(err)
				return
			}
			successor, created, err := api.retryRequest(context.Background(), &source, "alice", false)
			if err != nil {
				t.Error(err)
				return
			}
			results <- result{uid: successor.UID, created: created}
		}()
	}
	wg.Wait()
	close(results)
	var want types.UID
	createdCount := 0
	for result := range results {
		uid := result.uid
		if result.created {
			createdCount++
		}
		if want == "" {
			want = uid
		} else if uid != want {
			t.Errorf("got multiple successor UIDs: %s and %s", want, uid)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created=true count=%d, want exactly one", createdCount)
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

func TestRetryAdoptsDeterministicChildFoundByPendingDedupe(t *testing.T) {
	source := expiredSource()
	successor := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: retryChildName(source.UID), Namespace: ControllerNamespace, UID: "successor-uid",
		},
		Spec: source.Spec,
	}
	successor.Spec.RetryOfUID = string(source.UID)
	base := ctrlfake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&SudoRequest{}).WithObjects(source, successor).Build()
	missFirst := &atomic.Bool{}
	missFirst.Store(true)
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == successor.Name && missFirst.CompareAndSwap(true, false) {
				return apierrors.NewNotFound(schema.GroupResource{Group: GroupName, Resource: "sudorequests"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	api := &APIServer{Client: cl}
	got, created, err := api.retryRequest(context.Background(), source, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if created || got.UID != successor.UID {
		t.Fatalf("result uid=%s created=%v, want existing %s", got.UID, created, successor.UID)
	}
	var updated SudoRequest
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(source), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.SupersededByUID != string(successor.UID) {
		t.Fatalf("supersededByUID=%q, want %q", updated.Status.SupersededByUID, successor.UID)
	}
}

func TestRetrySuccessorValidationAllowsCurrentProfileResolution(t *testing.T) {
	source := expiredSource()
	source.Spec.Profile = DefaultExecutorProfile
	source.Status.ResolvedImage = "registry.example/executor@sha256:previously-reviewed"
	successor := &SudoRequest{
		Spec: source.Spec,
	}
	successor.Spec.RetryOfUID = string(source.UID)
	successor.Status.ResolvedImage = DefaultExecutorImage
	if err := validateRetrySuccessor(source, successor); err != nil {
		t.Fatalf("current profile resolution rejected: %v", err)
	}
}

func TestRetryRejectsDeterministicNameOccupants(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*SudoRequest)
	}{
		{
			name: "unrelated lineage",
			mutate: func(sr *SudoRequest) {
				sr.Spec.RetryOfUID = "different-source"
			},
		},
		{
			name: "different execution",
			mutate: func(sr *SudoRequest) {
				sr.Spec.Command = "kubectl delete namespace production"
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := expiredSource()
			cl := retryTestClient(t, source, nil)
			occupant := &SudoRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name: retryChildName(source.UID), Namespace: ControllerNamespace, UID: "occupant-uid",
				},
				Spec: source.Spec,
			}
			occupant.Spec.RetryOfUID = string(source.UID)
			tt.mutate(occupant)
			if err := cl.Create(context.Background(), occupant); err != nil {
				t.Fatal(err)
			}
			api := &APIServer{Client: cl}
			if _, _, err := api.retryRequest(context.Background(), source, "alice", false); err == nil {
				t.Fatal("retry adopted deterministic-name occupant")
			}
			var current SudoRequest
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(source), &current); err != nil {
				t.Fatal(err)
			}
			if current.Status.SupersededByUID != "" {
				t.Fatalf("source linked to rejected occupant: %s", current.Status.SupersededByUID)
			}
		})
	}
}

func TestRetryPreservesConflictForUnrelatedEquivalentPendingRequest(t *testing.T) {
	source := expiredSource()
	cl := retryTestClient(t, source, nil)
	pending := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "other-pending", Namespace: ControllerNamespace, UID: "pending-uid"},
		Spec:       source.Spec,
		Status:     SudoRequestStatus{Phase: PhasePending},
	}
	if err := cl.Create(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	api := &APIServer{Client: cl}
	_, _, err := api.retryRequest(context.Background(), source, "alice", false)
	var duplicate *pendingDuplicateError
	if !errors.As(err, &duplicate) || duplicate.UID != pending.UID {
		t.Fatalf("error=%v, want pending duplicate for %s", err, pending.UID)
	}
}

func TestRetryRevalidatesCurrentProfileBeforeCreating(t *testing.T) {
	source := expiredSource()
	source.Spec.Profile = "removed-profile"
	cl := retryTestClient(t, source, nil)
	api := &APIServer{Client: cl}
	if _, _, err := api.retryRequest(context.Background(), source, "alice", false); err == nil ||
		!strings.Contains(err.Error(), "unknown executor profile") {
		t.Fatalf("retry error=%v, want current profile rejection", err)
	}
	var requests SudoRequestList
	if err := cl.List(context.Background(), &requests); err != nil {
		t.Fatal(err)
	}
	if len(requests.Items) != 1 {
		t.Fatalf("retry created a successor before profile rejection; count=%d", len(requests.Items))
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

func TestLiveDashboardTemplateRendersRetryLineage(t *testing.T) {
	raw, err := templatesFS.ReadFile("templates/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, field := range []string{"ev.retryOfUID", "lineageHTML(ev)"} {
		if !strings.Contains(page, field) {
			t.Fatalf("live dashboard template does not render %s", field)
		}
	}
}
