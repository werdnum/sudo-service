package main

import (
	"context"
	"errors"
	"testing"

	authv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestRequesterAuthorizerPassesCompleteIdentityToSAR(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var got *authorizationv1.SubjectAccessReview
	cs.Fake.PrependReactor("create", "subjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		got = action.(ktesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
		return true, &authorizationv1.SubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true, Reason: "bound by test role"},
		}, nil
	})

	identity := authv1.UserInfo{
		Username: "system:serviceaccount:agents:worker",
		UID:      "sa-uid",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:agents"},
		Extra: map[string]authv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {"agent-0"},
			"example.invalid/multi":                 {"one", "two"},
		},
	}
	allowed, reason, err := (&RequesterAuthorizer{cs: cs}).AuthorizeSubmit(context.Background(), identity)
	if err != nil {
		t.Fatalf("AuthorizeSubmit: %v", err)
	}
	if !allowed || reason != "bound by test role" {
		t.Fatalf("allowed=%v reason=%q, want true and server reason", allowed, reason)
	}
	if got == nil {
		t.Fatal("no SubjectAccessReview was created")
	}
	if got.Spec.User != identity.Username || got.Spec.UID != identity.UID {
		t.Fatalf("SAR user=%q uid=%q, want %q %q", got.Spec.User, got.Spec.UID, identity.Username, identity.UID)
	}
	if len(got.Spec.Groups) != 2 || got.Spec.Groups[1] != identity.Groups[1] {
		t.Fatalf("SAR groups=%v, want %v", got.Spec.Groups, identity.Groups)
	}
	if values := got.Spec.Extra["example.invalid/multi"]; len(values) != 2 || values[1] != "two" {
		t.Fatalf("SAR extras=%v, want complete TokenReview extras", got.Spec.Extra)
	}
	want := authorizationv1.ResourceAttributes{
		Namespace:   DefaultControllerNamespace,
		Verb:        "create",
		Group:       GroupName,
		Version:     GroupVersion,
		Resource:    "sudorequests",
		Subresource: "submit",
	}
	if got.Spec.ResourceAttributes == nil || *got.Spec.ResourceAttributes != want {
		t.Fatalf("SAR attributes=%+v, want %+v", got.Spec.ResourceAttributes, want)
	}
}

func TestRequesterAuthorizerDenyAndErrorFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		status     authorizationv1.SubjectAccessReviewStatus
		reactorErr error
		wantReason string
		wantErr    bool
	}{
		{name: "denied", status: authorizationv1.SubjectAccessReviewStatus{Allowed: false, Reason: "no matching binding"}, wantReason: "no matching binding"},
		{name: "evaluation error", status: authorizationv1.SubjectAccessReviewStatus{EvaluationError: "authorizer unavailable"}, wantErr: true},
		{name: "api error", reactorErr: errors.New("apiserver unavailable"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			cs.Fake.PrependReactor("create", "subjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
				if tt.reactorErr != nil {
					return true, nil, tt.reactorErr
				}
				return true, &authorizationv1.SubjectAccessReview{Status: tt.status}, nil
			})
			allowed, reason, err := (&RequesterAuthorizer{cs: cs}).AuthorizeSubmit(context.Background(), authv1.UserInfo{Username: "requester"})
			if allowed {
				t.Fatal("authorization unexpectedly allowed")
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason=%q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestTokenReviewerReturnsCompleteAuthenticatedIdentity(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Fake.PrependReactor("create", "tokenreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		in := action.(ktesting.CreateAction).GetObject().(*authv1.TokenReview)
		if in.Spec.Token != "secret-token" {
			t.Fatalf("reviewed token=%q", in.Spec.Token)
		}
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{RequesterTokenAudience},
			User: authv1.UserInfo{
				Username: "system:serviceaccount:agents:worker",
				UID:      "sa-uid",
				Groups:   []string{"system:serviceaccounts"},
				Extra:    map[string]authv1.ExtraValue{"pod": {"agent-0"}},
			},
		}, ObjectMeta: metav1.ObjectMeta{}}, nil
	})
	got, err := (&TokenReviewer{cs: cs}).Review(context.Background(), "secret-token", RequesterTokenAudience)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Username != "system:serviceaccount:agents:worker" || got.UID != "sa-uid" || got.Extra["pod"][0] != "agent-0" {
		t.Fatalf("identity=%+v, want complete TokenReview identity", got)
	}
}
