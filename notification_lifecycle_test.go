package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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

	var links []string
	attempt := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		links = append(links, req.Form.Get("url"))
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
	r := &SudoRequestReconciler{Client: cl, APIReader: cl, Scheme: scheme, Pushover: po, PublicBaseURL: "https://sudo.example", Recorder: record.NewFakeRecorder(20), Broadcaster: NewBroadcaster()}

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

	if _, err := r.handlePending(ctx, pending.DeepCopy()); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status.NotificationAttempts != 1 || pending.Status.NotificationState != NotificationPending || pending.Status.NotificationLastError == "" {
		t.Fatalf("failed attempt not recorded separately: %+v", pending.Status)
	}
	secretName := pending.Status.ApprovalTokenSecretName
	hash := pending.Status.ApprovalTokenHash

	if _, err := r.handlePending(ctx, pending.DeepCopy()); err != nil {
		t.Fatalf("retry delivery: %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status.NotificationState != NotificationDelivered || pending.Status.NotificationAttempts != 2 || pending.Status.PushoverRequestID != "push-2" {
		t.Fatalf("successful delivery not recorded: %+v", pending.Status)
	}
	if pending.Status.ApprovalTokenSecretName != secretName || pending.Status.ApprovalTokenHash != hash {
		t.Fatal("delivery retry reminted approval state")
	}
	if len(links) != 2 || links[0] != links[1] || !strings.Contains(links[0], "id=uid-notify") {
		t.Fatalf("delivery retries did not reuse one valid URL: %#v", links)
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
