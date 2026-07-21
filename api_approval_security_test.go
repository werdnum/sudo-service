package main

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func approvalTestServer(t *testing.T, sr *SudoRequest) (*APIServer, client.Client, *HumanClaims) {
	t.Helper()
	scheme := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr).Build()
	tmpl, err := template.New("base").Funcs(template.FuncMap{"Lower": strings.ToLower}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &SudoRequestReconciler{Client: cl, APIReader: cl, Recorder: record.NewFakeRecorder(20), Broadcaster: NewBroadcaster()}
	a := &APIServer{Client: cl, Reconciler: reconciler, Config: &Config{AdminGroup: "admins"}, Templates: tmpl}
	return a, cl, &HumanClaims{PreferredUsername: "reviewer", Groups: []string{"admins"}}
}

func pendingApprovalRequest() *SudoRequest {
	return &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "approval", Namespace: ControllerNamespace, UID: "uid-approval", CreationTimestamp: metav1.Now()},
		Spec:       SudoRequestSpec{Requester: "alice", Reason: "inspect", Command: "kubectl get pods"},
		Status:     SudoRequestStatus{Phase: PhasePending},
	}
}

func TestExpiredNotificationLinkFallsBackForAdmin(t *testing.T) {
	sr := pendingApprovalRequest()
	token := "expired-token"
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	sr.Status.ApprovalTokenHash = sha256Hex(token)
	sr.Status.ApprovalTokenExpiresAt = &expired
	a, _, claims := approvalTestServer(t, sr)

	req := httptest.NewRequest(http.MethodGet, "/approve?id=uid-approval&t="+token, nil)
	rw := httptest.NewRecorder()
	a.renderApprovePage(rw, req, claims)
	if rw.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rw.Code, rw.Body.String())
	}
	if strings.Contains(rw.Body.String(), `name="t" value="expired-token"`) {
		t.Fatal("expired token was retained in the fallback approval form")
	}
	cookies := rw.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != csrfCookieName || cookies[0].SameSite != http.SameSiteStrictMode || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("CSRF cookie policy = %#v", cookies)
	}
	if !strings.Contains(rw.Body.String(), `name="csrf_token"`) {
		t.Fatal("approval page did not render CSRF fields")
	}
}

func TestApprovePostRequiresCSRF(t *testing.T) {
	sr := pendingApprovalRequest()
	a, cl, claims := approvalTestServer(t, sr)
	form := url.Values{"id": {string(sr.UID)}}
	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	a.handleApprovePost(rw, req, claims)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", rw.Code, rw.Body.String())
	}
	var got SudoRequest
	_ = cl.Get(req.Context(), client.ObjectKeyFromObject(sr), &got)
	if got.Status.Phase != PhasePending {
		t.Fatalf("request changed despite missing CSRF: %s", got.Status.Phase)
	}
}

func TestApprovePostAcceptsMatchingStrictCookieToken(t *testing.T) {
	sr := pendingApprovalRequest()
	a, cl, claims := approvalTestServer(t, sr)
	form := url.Values{"id": {string(sr.UID)}, "csrf_token": {"csrf-value"}}
	req := httptest.NewRequest(http.MethodPost, "/approve", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf-value"})
	rw := httptest.NewRecorder()
	a.handleApprovePost(rw, req, claims)
	if rw.Code != http.StatusOK {
		t.Fatalf("valid approval status=%d body=%s", rw.Code, rw.Body.String())
	}
	var got SudoRequest
	_ = cl.Get(req.Context(), client.ObjectKeyFromObject(sr), &got)
	if got.Status.Phase != PhaseApproved {
		t.Fatalf("phase=%s, want Approved", got.Status.Phase)
	}
}

func TestDenyPostRequiresCSRF(t *testing.T) {
	sr := pendingApprovalRequest()
	a, cl, claims := approvalTestServer(t, sr)
	form := url.Values{"id": {string(sr.UID)}}
	req := httptest.NewRequest(http.MethodPost, "/deny", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	a.denyHandlerWithClaims(rw, req, claims)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", rw.Code, rw.Body.String())
	}
	var got SudoRequest
	_ = cl.Get(req.Context(), client.ObjectKeyFromObject(sr), &got)
	if got.Status.Phase != PhasePending {
		t.Fatalf("request changed despite missing CSRF: %s", got.Status.Phase)
	}
}

func TestDenyPostAcceptsMatchingStrictCookieToken(t *testing.T) {
	sr := pendingApprovalRequest()
	a, cl, claims := approvalTestServer(t, sr)
	form := url.Values{"id": {string(sr.UID)}, "reason": {"no"}, "csrf_token": {"csrf-value"}}
	req := httptest.NewRequest(http.MethodPost, "/deny", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf-value"})
	rw := httptest.NewRecorder()
	a.denyHandlerWithClaims(rw, req, claims)
	if rw.Code != http.StatusOK {
		t.Fatalf("valid denial status=%d body=%s", rw.Code, rw.Body.String())
	}
	var got SudoRequest
	_ = cl.Get(req.Context(), client.ObjectKeyFromObject(sr), &got)
	if got.Status.Phase != PhaseDenied {
		t.Fatalf("phase=%s, want Denied", got.Status.Phase)
	}
}
