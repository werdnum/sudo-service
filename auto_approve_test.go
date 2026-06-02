package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadAutoApproveConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	yamlContent := `
- prefix:
  - kubectl
  - rollout
  - restart
- exact:
  - echo
  - hello
  image: busybox:latest
`
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	if err := LoadAutoApproveConfig(configPath); err != nil {
		t.Fatalf("LoadAutoApproveConfig() error = %v", err)
	}

	if len(autoApproveRules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(autoApproveRules))
	}

	if !reflect.DeepEqual(autoApproveRules[0].Prefix, []string{"kubectl", "rollout", "restart"}) {
		t.Errorf("rule 0 prefix mismatch")
	}

	if autoApproveRules[1].Image != "busybox:latest" {
		t.Errorf("rule 1 image mismatch, got %s", autoApproveRules[1].Image)
	}
}

func TestGetAutoApproveParsedCommand(t *testing.T) {
	// Setup test rules
	autoApproveRules = []AutoApproveRule{
		{
			Prefix: []string{"kubectl", "rollout", "restart"},
			// Image implicitly DefaultExecutorImage
		},
		{
			Exact: []string{"kubectl", "get", "pods"},
			Image: "custom-image:v1",
		},
	}

	tests := []struct {
		name     string
		command  string
		reqImage string
		wantTok  []string
		wantOK   bool
	}{
		{"prefix match default image", "kubectl rollout restart deployment/foo", DefaultExecutorImage, []string{"kubectl", "rollout", "restart", "deployment/foo"}, true},
		{"prefix mismatch image", "kubectl rollout restart deployment/foo", "other-image:v1", nil, false},
		{"exact match custom image", "kubectl get pods", "custom-image:v1", []string{"kubectl", "get", "pods"}, true},
		{"exact match wrong image", "kubectl get pods", DefaultExecutorImage, nil, false},
		{"exact mismatch command", "kubectl get nodes", "custom-image:v1", nil, false},
		{"invalid parse", "echo \"hello", DefaultExecutorImage, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTok, gotOK := getAutoApproveParsedCommand(tt.command, tt.reqImage)
			if gotOK != tt.wantOK {
				t.Errorf("getAutoApproveParsedCommand() gotOK = %v, want %v", gotOK, tt.wantOK)
			}
			if tt.wantOK && !reflect.DeepEqual(gotTok, tt.wantTok) {
				t.Errorf("getAutoApproveParsedCommand() gotTok = %v, want %v", gotTok, tt.wantTok)
			}
		})
	}
}
