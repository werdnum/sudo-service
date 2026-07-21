package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStatusResponseExposesManagedJobCorrelation(t *testing.T) {
	deadline := int32(5400)
	started := metav1.NewTime(time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC))
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "managed", UID: "request-uid"},
		Spec: SudoRequestSpec{Execution: SudoRequestExecution{
			Mode: ExecutionModeManagedJob, ResourceClass: ResourceClassLongRunning,
			ActiveDeadlineSeconds: &deadline,
		}},
		Status: SudoRequestStatus{
			Phase: PhaseApproved, ExecutorJobName: "sudo-exec-x", ExecutorJobUID: "job-uid",
			ExecutorJobLifecycle: JobLifecycleRunning, ExecutorJobStartedAt: &started,
		},
	}
	response := httptest.NewRecorder()
	(&APIServer{}).serveStatus(response, sr)
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	execution := body["execution"].(map[string]any)
	if execution["mode"] != ExecutionModeManagedJob || execution["activeDeadlineSeconds"] != float64(5400) {
		t.Fatalf("execution=%v", execution)
	}
	if body["executorJobUID"] != "job-uid" || body["executorJobLifecycle"] != JobLifecycleRunning || body["executorJobStartedAt"] != "2026-07-21T15:00:00Z" {
		t.Fatalf("correlation fields=%v", body)
	}
}

func TestCreateRequestRequiresRequesterAuthorization(t *testing.T) {
	const (
		token   = "sensitive-token"
		command = "echo sensitive-command"
		user    = "system:serviceaccount:agents:worker"
	)
	tests := []struct {
		name       string
		allowed    bool
		authzErr   error
		wantStatus int
		wantCount  int
	}{
		{name: "allow", allowed: true, wantStatus: http.StatusOK, wantCount: 1},
		{name: "deny", wantStatus: http.StatusForbidden},
		{name: "SAR unavailable", authzErr: errors.New("apiserver unavailable"), wantStatus: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kube := fake.NewSimpleClientset()
			kube.Fake.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
					Authenticated: true,
					Audiences:     []string{RequesterTokenAudience},
					User:          authv1.UserInfo{Username: user},
				}}, nil
			})
			kube.Fake.PrependReactor("create", "subjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
				if tt.authzErr != nil {
					return true, nil, tt.authzErr
				}
				return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: tt.allowed}}, nil
			})

			objects := ctrlfake.NewClientBuilder().WithScheme(scheme).Build()
			api := &APIServer{
				Client:        objects,
				TokenReviewer: &TokenReviewer{cs: kube},
				Authorizer:    &RequesterAuthorizer{cs: kube},
			}
			req := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(`{"reason":"test","command":"`+command+`"}`))
			req.Header.Set("Authorization", "Bearer "+token)
			resp := httptest.NewRecorder()
			api.createRequestHandler(resp, req)

			if resp.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%q, want %d", resp.Code, resp.Body.String(), tt.wantStatus)
			}
			if !tt.allowed && (strings.Contains(resp.Body.String(), token) || strings.Contains(resp.Body.String(), command)) {
				t.Fatalf("authorization failure leaked token or command: %q", resp.Body.String())
			}
			var list SudoRequestList
			if err := objects.List(req.Context(), &list); err != nil {
				t.Fatalf("list requests: %v", err)
			}
			if len(list.Items) != tt.wantCount {
				t.Fatalf("created %d requests, want %d", len(list.Items), tt.wantCount)
			}
			if tt.wantCount == 1 && list.Items[0].Spec.Requester != user {
				t.Fatalf("requester=%q, want authenticated user %q", list.Items[0].Spec.Requester, user)
			}
		})
	}
}
