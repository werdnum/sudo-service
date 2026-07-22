package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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

func TestCreateRequestDeduplicatesOnlyForAuthenticatedRequester(t *testing.T) {
	const user = "system:serviceaccount:agents:alice"
	for _, tt := range []struct {
		name              string
		existingRequester string
		wantCount         int
		wantDuplicate     bool
	}{
		{name: "same requester", existingRequester: user, wantCount: 1, wantDuplicate: true},
		{name: "other requester is invisible", existingRequester: "system:serviceaccount:agents:bob", wantCount: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			kube := fake.NewSimpleClientset()
			kube.Fake.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
					Authenticated: true, Audiences: []string{RequesterTokenAudience},
					User: authv1.UserInfo{Username: user},
				}}, nil
			})
			kube.Fake.PrependReactor("create", "subjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
			})
			existing := &SudoRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: ControllerNamespace, UID: "existing-uid"},
				Spec:       SudoRequestSpec{Requester: tt.existingRequester, Reason: "older reason", Command: "tool run", Stdin: "sensitive-payload"},
				Status:     SudoRequestStatus{Phase: PhasePending},
			}
			objects := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
			api := &APIServer{Client: objects, TokenReviewer: &TokenReviewer{cs: kube}, Authorizer: &RequesterAuthorizer{cs: kube}}
			req := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(`{"reason":"new reason","command":"tool run","stdin":"sensitive-payload"}`))
			req.Header.Set("Authorization", "Bearer token")
			resp := httptest.NewRecorder()
			api.createRequestHandler(resp, req)
			if resp.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if got, _ := body["duplicate"].(bool); got != tt.wantDuplicate {
				t.Fatalf("duplicate=%v body=%v", got, body)
			}
			if strings.Contains(resp.Body.String(), "sensitive-payload") || strings.Contains(resp.Body.String(), tt.existingRequester) {
				t.Fatalf("duplicate response leaked payload/requester: %s", resp.Body.String())
			}
			var list SudoRequestList
			_ = objects.List(req.Context(), &list)
			if len(list.Items) != tt.wantCount {
				t.Fatalf("request count=%d, want %d", len(list.Items), tt.wantCount)
			}
		})
	}
}

func TestRetryEndpointRejectsAuthenticatedNonOwnerWithoutCreatingSuccessor(t *testing.T) {
	source := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: ControllerNamespace, UID: "source-uid"},
		Spec:       SudoRequestSpec{Requester: "system:serviceaccount:agents:alice", Command: "true", Reason: "test"},
		Status:     SudoRequestStatus{Phase: PhaseExpired},
	}
	kube := fake.NewSimpleClientset()
	kube.Fake.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true, Audiences: []string{RequesterTokenAudience},
			User: authv1.UserInfo{Username: "system:serviceaccount:agents:bob"},
		}}, nil
	})
	objects := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	api := &APIServer{Client: objects, TokenReviewer: &TokenReviewer{cs: kube}}
	req := httptest.NewRequest(http.MethodPost, "/requests/source-uid/retry", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer token")
	resp := httptest.NewRecorder()
	api.requestSubpathHandler(resp, req)
	if resp.Code != http.StatusForbidden || !strings.Contains(resp.Body.String(), "not the requester") {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var list SudoRequestList
	_ = objects.List(req.Context(), &list)
	if len(list.Items) != 1 {
		t.Fatalf("non-owner created successor; count=%d", len(list.Items))
	}
}
