package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	authv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

// JWTVerifier verifies Keycloak-issued JWTs against the realm's JWKS.
// The provider auto-refreshes the JWKS on cache miss.
type JWTVerifier struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
}

// HumanClaims is the verified set of claims we trust off a Keycloak ID token.
type HumanClaims struct {
	Subject           string   `json:"sub"`
	PreferredUsername string   `json:"preferred_username"`
	Email             string   `json:"email"`
	Groups            []string `json:"groups"`
}

func NewJWTVerifier(ctx context.Context, issuer, audience string) (*JWTVerifier, error) {
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC provider: %w", err)
	}
	return &JWTVerifier{
		provider: p,
		verifier: p.Verifier(&oidc.Config{
			ClientID:          audience,
			SkipClientIDCheck: false,
		}),
	}, nil
}

// Verify parses+verifies a raw JWT (either an ID token or an access token with `aud` set).
func (v *JWTVerifier) Verify(ctx context.Context, raw string) (*HumanClaims, error) {
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	var c HumanClaims
	if err := tok.Claims(&c); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	return &c, nil
}

// IsInGroup is a small convenience for membership checks.
func (c *HumanClaims) IsInGroup(g string) bool {
	for _, x := range c.Groups {
		// Keycloak often returns group names with a leading "/".
		if x == g || x == "/"+g || strings.TrimPrefix(x, "/") == g {
			return true
		}
	}
	return false
}

// TokenReviewer wraps the apiserver TokenReview API for SA-token authentication
// on the requester HTTP endpoints (/requests/{uid}, /output, /events).
type TokenReviewer struct {
	cs kubernetes.Interface
}

func NewTokenReviewer() (*TokenReviewer, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &TokenReviewer{cs: cs}, nil
}

// Review authenticates an SA token bearing the expected audience.
// Returns the complete identity established by the apiserver. Keeping the UID,
// groups and extras is important: the subsequent SubjectAccessReview must ask
// about the same identity that TokenReview authenticated.
func (t *TokenReviewer) Review(ctx context.Context, token, audience string) (authv1.UserInfo, error) {
	tr := &authv1.TokenReview{
		ObjectMeta: metav1.ObjectMeta{},
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{audience},
		},
	}
	out, err := t.cs.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return authv1.UserInfo{}, fmt.Errorf("TokenReview: %w", err)
	}
	if !out.Status.Authenticated {
		return authv1.UserInfo{}, fmt.Errorf("TokenReview: not authenticated: %s", out.Status.Error)
	}
	found := false
	for _, a := range out.Status.Audiences {
		if a == audience {
			found = true
			break
		}
	}
	if !found {
		return authv1.UserInfo{}, fmt.Errorf("TokenReview: token missing required audience %q", audience)
	}
	if out.Status.User.Username == "" {
		return authv1.UserInfo{}, fmt.Errorf("TokenReview: authenticated response has no username")
	}
	return out.Status.User, nil
}

// RequesterAuthorizer checks the dedicated HTTP-submission permission for an
// identity authenticated by TokenReview. `sudorequests/submit` is a virtual
// subresource used only for authorization; direct CR creation remains governed
// independently by `create` on `sudorequests`.
type RequesterAuthorizer struct {
	cs                  kubernetes.Interface
	ControllerNamespace string
}

func NewRequesterAuthorizer(controllerNamespace string) (*RequesterAuthorizer, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &RequesterAuthorizer{cs: cs, ControllerNamespace: controllerNamespace}, nil
}

func (a *RequesterAuthorizer) controllerNamespace() string {
	return configuredControllerNamespace(a.ControllerNamespace)
}

// AuthorizeSubmit asks the apiserver whether identity may submit a request via
// the HTTP API. A negative decision and an inability to obtain a decision are
// deliberately distinct so the handler can return 403 vs 503; both fail closed.
func (a *RequesterAuthorizer) AuthorizeSubmit(ctx context.Context, identity authv1.UserInfo) (bool, string, error) {
	extra := make(map[string]authorizationv1.ExtraValue, len(identity.Extra))
	for key, values := range identity.Extra {
		extra[key] = authorizationv1.ExtraValue(append([]string(nil), values...))
	}

	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   identity.Username,
			UID:    identity.UID,
			Groups: append([]string(nil), identity.Groups...),
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   a.controllerNamespace(),
				Verb:        "create",
				Group:       GroupName,
				Version:     GroupVersion,
				Resource:    "sudorequests",
				Subresource: "submit",
			},
		},
	}
	out, err := a.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, "", fmt.Errorf("SubjectAccessReview: %w", err)
	}
	if !out.Status.Allowed && out.Status.EvaluationError != "" {
		return false, out.Status.Reason, fmt.Errorf("SubjectAccessReview evaluation: %s", out.Status.EvaluationError)
	}
	return out.Status.Allowed, out.Status.Reason, nil
}
