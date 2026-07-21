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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func allowedRequesterAPI(t *testing.T) (*APIServer, *ctrlfake.ClientBuilder) {
	t.Helper()
	kube := fake.NewSimpleClientset()
	kube.Fake.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true, Audiences: []string{RequesterTokenAudience},
			User: authv1.UserInfo{Username: "system:serviceaccount:agents:worker"},
		}}, nil
	})
	kube.Fake.PrependReactor("create", "subjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	builder := ctrlfake.NewClientBuilder().WithScheme(scheme)
	return &APIServer{TokenReviewer: &TokenReviewer{cs: kube}, Authorizer: &RequesterAuthorizer{cs: kube}}, builder
}

func TestCreateTypedActionCompilesServerOwnedCommandAndPermission(t *testing.T) {
	api, builder := allowedRequesterAPI(t)
	objects := builder.Build()
	api.Client = objects
	body := `{"reason":"recover web after config repair","action":{"version":"v1","operation":"workload.restart","resources":[{"namespace":"apps","kind":"Deployment","name":"web"}]}}`
	req := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	resp := httptest.NewRecorder()
	api.createRequestHandler(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["permissionRequest"] != "Restart Deployment apps/web." {
		t.Fatalf("permissionRequest=%q", response["permissionRequest"])
	}
	if response["command"] != "kubectl rollout restart deployment/web --namespace apps" {
		t.Fatalf("command=%q", response["command"])
	}
	var requests SudoRequestList
	if err := objects.List(req.Context(), &requests); err != nil {
		t.Fatal(err)
	}
	if len(requests.Items) != 1 || requests.Items[0].Spec.Action == nil {
		t.Fatalf("stored requests=%+v", requests.Items)
	}
	if requests.Items[0].Spec.Reason != "recover web after config repair" {
		t.Fatalf("request reason was not preserved: %q", requests.Items[0].Spec.Reason)
	}
}

func TestCreateTypedActionRejectsDivergenceAndBroadFields(t *testing.T) {
	tests := map[string]string{
		"paired command": `{"reason":"x","command":"kubectl delete pods --all -A","action":{"version":"v1","operation":"job.delete","resources":[{"namespace":"ops","kind":"Job","name":"one"}]}}`,
		"selector field": `{"reason":"x","action":{"version":"v1","operation":"job.delete","resources":[{"namespace":"ops","kind":"Job","name":"one"}],"selector":"status=failed"}}`,
		"wildcard name":  `{"reason":"x","action":{"version":"v1","operation":"job.delete","resources":[{"namespace":"ops","kind":"Job","name":"*"}]}}`,
		"pod restart":    `{"reason":"x","action":{"version":"v1","operation":"workload.restart","resources":[{"namespace":"apps","kind":"Pod","name":"web"}]}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			api, builder := allowedRequesterAPI(t)
			objects := builder.Build()
			api.Client = objects
			req := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer token")
			resp := httptest.NewRecorder()
			api.createRequestHandler(resp, req)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
			}
			var requests SudoRequestList
			if err := objects.List(req.Context(), &requests); err != nil {
				t.Fatal(err)
			}
			if len(requests.Items) != 0 {
				t.Fatalf("created rejected request: %+v", requests.Items)
			}
		})
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
