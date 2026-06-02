package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	authv1 "k8s.io/api/authentication/v1"
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
	cs *kubernetes.Clientset
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
// Returns the authenticated username, e.g. "system:serviceaccount:k8s-agent:k8s-agent-sa".
func (t *TokenReviewer) Review(ctx context.Context, token, audience string) (string, error) {
	tr := &authv1.TokenReview{
		ObjectMeta: metav1.ObjectMeta{},
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{audience},
		},
	}
	out, err := t.cs.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("TokenReview: %w", err)
	}
	if !out.Status.Authenticated {
		return "", fmt.Errorf("TokenReview: not authenticated: %s", out.Status.Error)
	}
	found := false
	for _, a := range out.Status.Audiences {
		if a == audience {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("TokenReview: token missing required audience %q", audience)
	}
	return out.Status.User.Username, nil
}
