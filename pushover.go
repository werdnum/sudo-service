package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PushoverClient is a tiny client for the Pushover messages API. We don't pull
// in a heavyweight SDK because a single POST is all this controller needs.
//
// Credentials match the existing repo convention (see
// kubernetes/jsonnet/monitoring/apps/alertmanager/pushover-credentials-sealed.yaml):
// a `token` (application token) plus a `user_key` (user or group key).
type PushoverClient struct {
	Token       string
	UserKey     string
	HTTP        *http.Client
	APIEndpoint string
}

func NewPushoverClient(token, userKey string) *PushoverClient {
	return &PushoverClient{
		Token:       token,
		UserKey:     userKey,
		HTTP:        &http.Client{Timeout: 15 * time.Second},
		APIEndpoint: "https://api.pushover.net/1/messages.json",
	}
}

type pushoverResponse struct {
	Status  int      `json:"status"`
	Request string   `json:"request"`
	Errors  []string `json:"errors,omitempty"`
}

// SendApproval sends a Pushover notification with a clickable URL. Returns the
// API request ID for audit-trail correlation. (Pushover only mints a receipt
// for emergency-priority messages; for ordinary pushes the request ID is the
// only correlation handle.)
func (c *PushoverClient) SendApproval(ctx context.Context, title, message, link string) (string, error) {
	form := url.Values{
		"token":     {c.Token},
		"user":      {c.UserKey},
		"title":     {title},
		"message":   {message},
		"url":       {link},
		"url_title": {"Approve or deny"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.APIEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("pushover %d: %s", resp.StatusCode, string(body))
	}
	var pr pushoverResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", fmt.Errorf("decode pushover response: %w", err)
	}
	if pr.Status != 1 {
		return "", fmt.Errorf("pushover error: %s", strings.Join(pr.Errors, "; "))
	}
	return pr.Request, nil
}
