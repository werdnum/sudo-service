package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"request\":\"list Pods in the sudo-service namespace.\",\"effects\":[\"READ_ONLY\"]}"}}]}`)
	}))
	defer srv.Close()

	s := NewSummarizer("sk-secret", srv.URL+"/v1", "test-model")
	out, err := s.Summarize(context.Background(), "kubectl get pods", "alpine/k8s:1.35.5", "debugging", "")
	if err != nil {
		t.Fatalf("Summarize() error = %v", err)
	}

	if out.Request != "list Pods in the sudo-service namespace." || len(out.Effects) != 1 || out.Effects[0] != EffectReadOnly {
		t.Errorf("unexpected assessment: %+v", out)
	}
	if out.Model != "test-model" || out.SchemaVersion != PermissionAssessmentSchemaVersion || out.PromptVersion != PermissionAssessmentPromptVersion || out.GeneratedAt.IsZero() {
		t.Errorf("missing audit metadata: %+v", out)
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
	if gotRawBody["max_completion_tokens"] != float64(160) {
		t.Errorf("max_completion_tokens = %v, want 160", gotRawBody["max_completion_tokens"])
	}
	if _, ok := gotRawBody["max_tokens"]; ok {
		t.Fatal("request must not send legacy max_tokens")
	}
	responseFormat, ok := gotRawBody["response_format"].(map[string]any)
	if !ok || responseFormat["type"] != "json_schema" {
		t.Fatalf("response_format is not strict JSON schema: %+v", gotRawBody["response_format"])
	}
	jsonSchema, ok := responseFormat["json_schema"].(map[string]any)
	if !ok || jsonSchema["strict"] != true || jsonSchema["name"] != "sudo_permission_assessment" {
		t.Fatalf("unexpected json_schema: %+v", responseFormat["json_schema"])
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

func TestValidatePermissionResponseRejectsUntrustedShape(t *testing.T) {
	tests := []struct {
		name string
		in   permissionModelResponse
	}{
		{name: "empty request", in: permissionModelResponse{Effects: []PermissionEffect{EffectReadOnly}}},
		{name: "multiline", in: permissionModelResponse{Request: "read pods\nand secrets", Effects: []PermissionEffect{EffectReadOnly}}},
		{name: "unknown effect", in: permissionModelResponse{Request: "read Pods.", Effects: []PermissionEffect{"HIGH_RISK"}}},
		{name: "no effects", in: permissionModelResponse{Request: "read Pods."}},
		{name: "read only mutation", in: permissionModelResponse{Request: "delete a Pod.", Effects: []PermissionEffect{EffectReadOnly, EffectDeletesResource}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validatePermissionResponse(&tt.in); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidatePermissionResponseCanonicalizesEffects(t *testing.T) {
	result := permissionModelResponse{
		Request: "delete the exact failed Job build-123 in namespace ci.",
		Effects: []PermissionEffect{EffectDeletesResource, EffectChangesCluster, EffectDeletesResource},
	}
	if err := validatePermissionResponse(&result); err != nil {
		t.Fatal(err)
	}
	want := []PermissionEffect{EffectChangesCluster, EffectDeletesResource}
	if !slices.Equal(result.Effects, want) {
		t.Fatalf("effects = %v, want %v", result.Effects, want)
	}
}

func TestSummarizeAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	s := NewSummarizer("sk-bad", srv.URL+"/v1", "m")
	if _, err := s.Summarize(context.Background(), "echo hi", "img", "", ""); err == nil {
		t.Fatal("expected error on non-2xx response, got nil")
	}
}

func TestSummarizeEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	s := NewSummarizer("sk", srv.URL+"/v1", "m")
	if _, err := s.Summarize(context.Background(), "echo hi", "img", "", ""); err == nil {
		t.Fatal("expected error when no choices returned, got nil")
	}
}
