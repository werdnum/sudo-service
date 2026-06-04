package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewSummarizerDisabledWithoutKey(t *testing.T) {
	if s := NewSummarizer("", "", ""); s != nil {
		t.Fatalf("expected nil summarizer when API key is empty, got %+v", s)
	}
}

func TestNewSummarizerDefaults(t *testing.T) {
	s := NewSummarizer("sk-test", "", "")
	if s == nil {
		t.Fatal("expected non-nil summarizer when API key is set")
	}
	if s.BaseURL != DefaultOpenAIBaseURL {
		t.Errorf("BaseURL = %q, want default %q", s.BaseURL, DefaultOpenAIBaseURL)
	}
	if s.Model != DefaultOpenAIModel {
		t.Errorf("Model = %q, want default %q", s.Model, DefaultOpenAIModel)
	}
	// Trailing slash on the base URL must be trimmed so request paths are clean.
	if got := NewSummarizer("sk-test", "https://example.test/v1/", "m").BaseURL; got != "https://example.test/v1" {
		t.Errorf("trailing slash not trimmed: got %q", got)
	}
}

func TestSummarizeRequestAndResponse(t *testing.T) {
	var gotAuth, gotPath string
	var gotRawBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotRawBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"  Summary: lists pods.\nRisk: low — Confidence: high  "}}]}`)
	}))
	defer srv.Close()

	s := NewSummarizer("sk-secret", srv.URL+"/v1", "test-model")
	out, err := s.Summarize(context.Background(), "kubectl get pods", "alpine/k8s:1.35.5", "debugging")
	if err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}

	// Response content is trimmed.
	if out != "Summary: lists pods.\nRisk: low — Confidence: high" {
		t.Errorf("unexpected summary: %q", out)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Errorf("Authorization = %q, want bearer key", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotRawBody["model"] != "test-model" {
		t.Errorf("model = %q, want test-model", gotRawBody["model"])
	}
	if gotRawBody["max_completion_tokens"] != float64(220) {
		t.Errorf("max_completion_tokens = %v, want 220", gotRawBody["max_completion_tokens"])
	}
	if _, ok := gotRawBody["max_tokens"]; ok {
		t.Fatal("request must not send legacy max_tokens")
	}
	messages, ok := gotRawBody["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("unexpected messages: %+v", gotRawBody["messages"])
	}
	systemMessage, ok := messages[0].(map[string]any)
	if !ok || systemMessage["role"] != "system" {
		t.Fatalf("unexpected system message: %+v", messages[0])
	}
	userMessage, ok := messages[1].(map[string]any)
	if !ok || userMessage["role"] != "user" {
		t.Fatalf("unexpected user message: %+v", messages[1])
	}
	// The command and image must reach the model; the reason is included as
	// untrusted context.
	userContent, _ := userMessage["content"].(string)
	if !strings.Contains(userContent, "kubectl get pods") {
		t.Errorf("user message missing command: %q", userContent)
	}
	if !strings.Contains(userContent, "alpine/k8s:1.35.5") {
		t.Errorf("user message missing image: %q", userContent)
	}
}

func TestSummarizeAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	s := NewSummarizer("sk-bad", srv.URL+"/v1", "m")
	if _, err := s.Summarize(context.Background(), "echo hi", "img", ""); err == nil {
		t.Fatal("expected error on non-2xx response, got nil")
	}
}

func TestSummarizeEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	s := NewSummarizer("sk", srv.URL+"/v1", "m")
	if _, err := s.Summarize(context.Background(), "echo hi", "img", ""); err == nil {
		t.Fatal("expected error when no choices returned, got nil")
	}
}
