package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

type permissionFixture struct {
	Name    string             `json:"name"`
	Command string             `json:"command"`
	Image   string             `json:"image"`
	Reason  string             `json:"reason"`
	Extras  string             `json:"extras,omitempty"`
	Request string             `json:"request"`
	Effects []PermissionEffect `json:"effects"`
}

func TestSanitizedPermissionAssessmentFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/permission_assessment_fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []permissionFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < 10 {
		t.Fatalf("fixture corpus unexpectedly small: %d", len(fixtures))
	}

	canonicalByName := make(map[string][]PermissionEffect)
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			response, err := json.Marshal(permissionModelResponse{Request: fixture.Request, Effects: fixture.Effects})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": string(response)}}},
				})
			}))
			defer server.Close()

			assessment, err := NewSummarizer("test-key", server.URL+"/v1", "fixture-model").Summarize(
				context.Background(), fixture.Command, fixture.Image, fixture.Reason, fixture.Extras,
			)
			if err != nil {
				t.Fatal(err)
			}
			if assessment.Request != fixture.Request {
				t.Fatalf("request = %q, want %q", assessment.Request, fixture.Request)
			}
			canonicalByName[fixture.Name] = assessment.Effects
		})
	}

	if !slices.Equal(canonicalByName["exact failed Job delete"], canonicalByName["equivalent exact failed Job delete"]) {
		t.Fatalf("equivalent exact-name deletes have unstable effects: %v vs %v",
			canonicalByName["exact failed Job delete"], canonicalByName["equivalent exact failed Job delete"])
	}
	for _, fixture := range fixtures {
		if strings.Contains(fmt.Sprint(fixture), "BEGIN") {
			t.Fatalf("fixture %q appears to contain credential material", fixture.Name)
		}
	}
}
