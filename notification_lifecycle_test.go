package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNotificationLifecyclePersistsLinkBeforeDeliveryAndReusesIt(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "notify", Namespace: ControllerNamespace, UID: "uid-notify", CreationTimestamp: metav1.Now()},
		Spec:       SudoRequestSpec{Requester: "alice", Reason: "inspect it", Command: "kubectl get pods"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr).Build()
	summaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"request\":\"list Pods in the sudo-service namespace.\",\"effects\":[\"READ_ONLY\"]}"}}]}`)
	}))
	defer summaryServer.Close()

	var links []string
	var messages []string
	attempt := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		links = append(links, req.Form.Get("url"))
		messages = append(messages, req.Form.Get("message"))
		attempt++
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":0,"errors":["try again"]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(pushoverResponse{Status: 1, Request: "push-2"})
	}))
	defer server.Close()

	po := NewPushoverClient("token", "user")
	po.APIEndpoint = server.URL
	r := &SudoRequestReconciler{Client: cl, APIReader: cl, Scheme: scheme, Pushover: po, Summarizer: NewSummarizer("test-key", summaryServer.URL+"/v1", "test-model"), PublicBaseURL: "https://sudo.example", Recorder: record.NewFakeRecorder(20), Broadcaster: NewBroadcaster()}

	if _, err := r.handleNew(ctx, sr.DeepCopy()); err != nil {
		t.Fatalf("handleNew: %v", err)
	}
	var pending SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &pending); err != nil {
		t.Fatal(err)
	}
	if attempt != 0 {
		t.Fatalf("notification sent before Pending was persisted: attempts=%d", attempt)
	}
	if pending.Status.Phase != PhasePending || pending.Status.NotificationState != NotificationPending {
		t.Fatalf("status after initialization = phase %q notification %q", pending.Status.Phase, pending.Status.NotificationState)
	}
	if pending.Status.ApprovalTokenSecretName == "" || pending.Status.ApprovalTokenHash == "" {
		t.Fatal("persisted Pending state does not reference an approval token")
	}
	var tokenSecret corev1.Secret
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ControllerNamespace, Name: pending.Status.ApprovalTokenSecretName}, &tokenSecret); err != nil {
		t.Fatalf("approval token Secret: %v", err)
	}
	if tokenSecret.Labels["role"] != "approval-token" || tokenSecret.Labels["expires-at"] == "" {
		t.Fatalf("approval token Secret is not expiry-sweepable: labels=%v", tokenSecret.Labels)
	}

	firstResult, err := r.handlePending(ctx, pending.DeepCopy())
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if firstResult.RequeueAfter <= 0 {
		t.Fatalf("failed delivery did not schedule a delayed retry: %+v", firstResult)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status.NotificationAttempts != 1 || pending.Status.NotificationState != NotificationPending || pending.Status.NotificationLastError == "" {
		t.Fatalf("failed attempt not recorded separately: %+v", pending.Status)
	}
	if pending.Status.NotificationNextAttemptAt == nil || !pending.Status.NotificationNextAttemptAt.After(time.Now()) {
		t.Fatalf("failed attempt did not persist its retry gate: %+v", pending.Status)
	}
	if pending.Status.PermissionAssessment == nil || pending.Status.PermissionAssessmentState != PermissionAssessmentGenerated {
		t.Fatalf("permission assessment was not persisted before notification: %+v", pending.Status)
	}
	secretName := pending.Status.ApprovalTokenSecretName
	hash := pending.Status.ApprovalTokenHash

	gatedResult, err := r.handlePending(ctx, pending.DeepCopy())
	if err != nil {
		t.Fatalf("gated retry: %v", err)
	}
	if gatedResult.RequeueAfter <= 0 || len(links) != 1 {
		t.Fatalf("retry gate allowed an immediate duplicate: result=%+v links=%#v", gatedResult, links)
	}
	past := metav1.NewTime(time.Now().Add(-time.Second))
	pending.Status.NotificationNextAttemptAt = &past
	if _, err := r.handlePending(ctx, pending.DeepCopy()); err != nil {
		t.Fatalf("retry delivery: %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status.NotificationState != NotificationDelivered || pending.Status.NotificationAttempts != 2 || pending.Status.PushoverRequestID != "push-2" {
		t.Fatalf("successful delivery not recorded: %+v", pending.Status)
	}
	if pending.Status.NotificationNextAttemptAt != nil {
		t.Fatalf("successful delivery retained retry gate: %+v", pending.Status)
	}
	if pending.Status.ApprovalTokenSecretName != secretName || pending.Status.ApprovalTokenHash != hash {
		t.Fatal("delivery retry reminted approval state")
	}
	if len(links) != 2 || links[0] != links[1] || !strings.Contains(links[0], "id=uid-notify") {
		t.Fatalf("delivery retries did not reuse one valid URL: %#v", links)
	}
	if len(messages) != 2 || !strings.Contains(messages[0], "Permission requested: list Pods in the sudo-service namespace.") || messages[0] != messages[1] {
		t.Fatalf("notification did not reuse the persisted permission sentence: %#v", messages)
	}
}

func TestAmbiguousPendingStatusWriteKeepsReferencedTokenSecret(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "ambiguous", Namespace: ControllerNamespace, UID: "uid-ambiguous", CreationTimestamp: metav1.Now()},
		Spec:       SudoRequestSpec{Requester: "alice", Reason: "inspect", Command: "kubectl get pods"},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if err := c.Status().Update(ctx, obj, opts...); err != nil {
				return err
			}
			return errors.New("response lost after commit")
		},
	})
	r := &SudoRequestReconciler{Client: cl, APIReader: base, Scheme: scheme, Recorder: record.NewFakeRecorder(20), Broadcaster: NewBroadcaster()}

	if _, err := r.handleNew(ctx, sr.DeepCopy()); err == nil {
		t.Fatal("expected ambiguous status update error")
	}
	var pending SudoRequest
	if err := base.Get(ctx, client.ObjectKeyFromObject(sr), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status.Phase != PhasePending || pending.Status.ApprovalTokenSecretName == "" {
		t.Fatalf("status was not committed before the lost response: %+v", pending.Status)
	}
	var tokenSecret corev1.Secret
	if err := base.Get(ctx, client.ObjectKey{Namespace: pending.Namespace, Name: pending.Status.ApprovalTokenSecretName}, &tokenSecret); err != nil {
		t.Fatalf("ambiguous write deleted the referenced token Secret: %v", err)
	}
}

func TestGarbageCollectorSweepsExpiredApprovalTokenSecret(t *testing.T) {
	ctx := context.Background()
	expired := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "expired-token", Namespace: ControllerNamespace,
		Labels: map[string]string{"app": "sudo-service", "role": "approval-token", "expires-at": strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)},
	}}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(expired).Build()
	if err := (&GarbageCollector{Client: cl}).sweepSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(expired), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expired approval token Secret was not swept: %v", err)
	}
}

func TestAcceptedPushBeforeStatusFailureRetriesSamePersistedLink(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	token := "persisted-token"
	expires := metav1.NewTime(time.Now().Add(time.Hour))
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "window", Namespace: ControllerNamespace, UID: "uid-window", CreationTimestamp: metav1.Now()},
		Spec:       SudoRequestSpec{Requester: "alice", Reason: "inspect", Command: "kubectl get pods"},
		Status:     SudoRequestStatus{Phase: PhasePending, NotificationState: NotificationPending, ApprovalTokenHash: sha256Hex(token), ApprovalTokenExpiresAt: &expires, ApprovalTokenSecretName: "approval-token"},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "approval-token", Namespace: ControllerNamespace}, Data: map[string][]byte{"token": []byte(token)}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, secret).Build()
	failDeliveryWrite := true
	cl := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			candidate := obj.(*SudoRequest)
			if failDeliveryWrite && candidate.Status.NotificationState == NotificationDelivered {
				failDeliveryWrite = false
				return errors.New("status apiserver unavailable")
			}
			return c.Status().Update(ctx, obj, opts...)
		},
	})
	var links []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		links = append(links, req.Form.Get("url"))
		_ = json.NewEncoder(w).Encode(pushoverResponse{Status: 1, Request: "accepted"})
	}))
	defer server.Close()
	po := NewPushoverClient("token", "user")
	po.APIEndpoint = server.URL
	r := &SudoRequestReconciler{Client: cl, APIReader: cl, Pushover: po, PublicBaseURL: "https://sudo.example", Recorder: record.NewFakeRecorder(20), Broadcaster: NewBroadcaster()}

	if _, err := r.handlePending(ctx, sr.DeepCopy()); err == nil {
		t.Fatal("expected delivery status write failure")
	}
	var stillPending SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &stillPending); err != nil {
		t.Fatal(err)
	}
	if stillPending.Status.NotificationState != NotificationPending {
		t.Fatalf("failed status write unexpectedly persisted %q", stillPending.Status.NotificationState)
	}
	if _, err := r.handlePending(ctx, stillPending.DeepCopy()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(links) != 2 || links[0] != links[1] {
		t.Fatalf("accepted-push failure window produced different links: %#v", links)
	}
}

func TestApprovalTokenLifetimeMatchesPendingLifetime(t *testing.T) {
	if ApprovalTokenTTL != PendingRequestTTL {
		t.Fatalf("approval token TTL %s does not match Pending TTL %s", time.Duration(ApprovalTokenTTL)*time.Second, time.Duration(PendingRequestTTL)*time.Second)
	}
}

func TestAssessmentFailureDoesNotBlockApprovalOrNotification(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	token := "persisted-token"
	expires := metav1.NewTime(time.Now().Add(time.Hour))
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "assessment-failure", Namespace: ControllerNamespace, UID: "uid-assessment-failure", CreationTimestamp: metav1.Now()},
		Spec:       SudoRequestSpec{Requester: "alice", Reason: "inspect", Command: "kubectl get pods"},
		Status: SudoRequestStatus{
			Phase: PhasePending, NotificationState: NotificationPending,
			PermissionAssessmentState: PermissionAssessmentPending,
			ApprovalTokenHash:         sha256Hex(token), ApprovalTokenExpiresAt: &expires,
			ApprovalTokenSecretName: "approval-token",
		},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "approval-token", Namespace: ControllerNamespace}, Data: map[string][]byte{"token": []byte(token)}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, secret).Build()

	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"not-json"}}]}`)
	}))
	defer modelServer.Close()
	pushes := 0
	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		pushes++
		_ = json.NewEncoder(w).Encode(pushoverResponse{Status: 1, Request: "delivered"})
	}))
	defer pushServer.Close()
	po := NewPushoverClient("token", "user")
	po.APIEndpoint = pushServer.URL
	r := &SudoRequestReconciler{
		Client: cl, APIReader: cl,
		Pushover: po, Summarizer: NewSummarizer("test-key", modelServer.URL+"/v1", "test-model"),
		PublicBaseURL: "https://sudo.example", Recorder: record.NewFakeRecorder(20), Broadcaster: NewBroadcaster(),
	}

	if _, err := r.handlePending(ctx, sr.DeepCopy()); err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	var got SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != PhasePending || got.Status.PermissionAssessment != nil || got.Status.PermissionAssessmentState != PermissionAssessmentFailed {
		t.Fatalf("approval was not left usable after model failure: %+v", got.Status)
	}
	if pushes != 1 || got.Status.NotificationState != NotificationDelivered {
		t.Fatalf("notification blocked by model failure: pushes=%d state=%q", pushes, got.Status.NotificationState)
	}
}
