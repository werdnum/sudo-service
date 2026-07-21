package main

import (
	"context"
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
		ObjectMeta: metav1.ObjectMeta{Name: "approval", Namespace: DefaultControllerNamespace, UID: "uid-approval", CreationTimestamp: metav1.Now()},
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

func TestApprovePageRendersCompactPermissionAssessmentAndGroundTruth(t *testing.T) {
	sr := pendingApprovalRequest()
	sr.Status.PermissionAssessment = &PermissionAssessment{
		Request:       "delete the exact failed Job build-123 in namespace ci.",
		Effects:       []PermissionEffect{EffectChangesCluster, EffectDeletesResource},
		SchemaVersion: PermissionAssessmentSchemaVersion,
		PromptVersion: PermissionAssessmentPromptVersion,
		Model:         "test-model",
		GeneratedAt:   metav1.Now(),
	}
	sr.Status.Summary = "legacy risk wall must not be shown"
	sr.Spec.Command = "kubectl delete job build-123 -n ci"
	a, _, claims := approvalTestServer(t, sr)

	req := httptest.NewRequest(http.MethodGet, "/approve?id=uid-approval", nil)
	rw := httptest.NewRecorder()
	a.renderApprovePage(rw, req, claims)
	body := rw.Body.String()
	for _, want := range []string{
		"Permission requested",
		"delete the exact failed Job build-123 in namespace ci.",
		"CHANGES CLUSTER",
		"DELETES RESOURCE",
		"kubectl delete job build-123 -n ci",
		"Full pod spec (ground truth",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("approval page missing %q", want)
		}
	}
	if strings.Contains(body, "legacy risk wall") || strings.Contains(body, "Confirm the command") {
		t.Fatal("approval page rendered a duplicate legacy or generic warning")
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

func TestCSRFCookieIsRefreshedForEachRenderedForm(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/approve", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "existing-token"})
	rw := httptest.NewRecorder()

	token, err := ensureCSRFCookie(rw, req)
	if err != nil {
		t.Fatal(err)
	}
	if token != "existing-token" {
		t.Fatalf("token = %q, want existing token", token)
	}
	cookies := rw.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != token || cookies[0].MaxAge != PendingRequestTTL {
		t.Fatalf("CSRF cookie was not refreshed for a full review window: %#v", cookies)
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

func TestAdminResubmitRequiresCSRFAndRecordsVerifiedActor(t *testing.T) {
	sr := pendingApprovalRequest()
	sr.Status.Phase = PhaseDenied
	a, cl, claims := approvalTestServer(t, sr)
	form := url.Values{"id": {string(sr.UID)}}
	request := func(withCSRF bool) *httptest.ResponseRecorder {
		values := form
		if withCSRF {
			values = url.Values{"id": {string(sr.UID)}, "csrf_token": {"csrf-value"}}
		}
		req := httptest.NewRequest(http.MethodPost, "/resubmit", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if withCSRF {
			req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf-value"})
		}
		rw := httptest.NewRecorder()
		a.resubmitHandlerWithClaims(rw, req, claims)
		return rw
	}
	if rw := request(false); rw.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", rw.Code, rw.Body.String())
	}
	if rw := request(true); rw.Code != http.StatusOK {
		t.Fatalf("valid resubmit status=%d body=%s", rw.Code, rw.Body.String())
	}
	var successor SudoRequest
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ControllerNamespace, Name: retryChildName(sr.UID)}, &successor); err != nil {
		t.Fatal(err)
	}
	if successor.Spec.Requester != sr.Spec.Requester || successor.Spec.SubmittedBy != claims.PreferredUsername {
		t.Fatalf("attribution = %#v", successor.Spec)
	}
}

func TestAdminResubmitRejectsVerifiedNonAdminClaims(t *testing.T) {
	sr := pendingApprovalRequest()
	sr.Status.Phase = PhaseExpired
	a, _, _ := approvalTestServer(t, sr)
	form := url.Values{"id": {string(sr.UID)}, "csrf_token": {"csrf-value"}}
	req := httptest.NewRequest(http.MethodPost, "/resubmit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf-value"})
	rw := httptest.NewRecorder()
	a.resubmitHandlerWithClaims(rw, req, &HumanClaims{PreferredUsername: "viewer", Groups: []string{"viewers"}})
	if rw.Code != http.StatusForbidden {
		t.Fatalf("non-admin status=%d body=%s", rw.Code, rw.Body.String())
	}
}

func TestAdminResubmitDuplicateLinksActiveRequest(t *testing.T) {
	source := pendingApprovalRequest()
	source.Status.Phase = PhaseDenied
	a, cl, claims := approvalTestServer(t, source)
	pending := source.DeepCopy()
	pending.Name = "equivalent-pending"
	pending.UID = "equivalent-pending-uid"
	pending.ResourceVersion = ""
	pending.Status = SudoRequestStatus{Phase: PhasePending}
	if err := cl.Create(context.Background(), pending); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"id": {string(source.UID)}, "csrf_token": {"csrf-value"}}
	req := httptest.NewRequest(http.MethodPost, "/resubmit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "csrf-value"})
	rw := httptest.NewRecorder()
	a.resubmitHandlerWithClaims(rw, req, claims)
	if rw.Code != http.StatusConflict {
		t.Fatalf("duplicate resubmit status=%d body=%s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if !strings.Contains(body, string(pending.UID)) || !strings.Contains(body, "/approve?id="+string(pending.UID)) {
		t.Fatalf("duplicate response does not link active UID: %s", body)
	}
	var current SudoRequest
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(source), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.SupersededByUID != "" {
		t.Fatalf("unrelated pending request became lineage successor: %s", current.Status.SupersededByUID)
	}
}
