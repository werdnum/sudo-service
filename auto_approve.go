package main

import (
	"fmt"
	"os"

	"github.com/google/shlex"
	"sigs.k8s.io/yaml"
)

type AutoApproveRule struct {
	Exact  []string `json:"exact,omitempty"`
	Prefix []string `json:"prefix,omitempty"`
	Image  string   `json:"image,omitempty"`
}

var autoApproveRules []AutoApproveRule

func LoadAutoApproveConfig(path string) error {
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read auto-approve config %s: %w", path, err)
	}

	var rules []AutoApproveRule
	if err := yaml.UnmarshalStrict(data, &rules); err != nil {
		return fmt.Errorf("unmarshal auto-approve config %s: %w", path, err)
	}

	autoApproveRules = rules
	return nil
}

// getAutoApproveParsedCommand splits the command using shlex and checks if it matches
// the auto-approve allowlist for the given image. If it does, it returns the parsed
// command tokens and true.
func getAutoApproveParsedCommand(command string, reqImage string) ([]string, bool) {
	tokens, err := shlex.Split(command)
	if err != nil || len(tokens) == 0 {
		return nil, false
	}

	for _, rule := range autoApproveRules {
		// Check image constraint. Empty rule.Image means DefaultExecutorImage.
		expectedImage := rule.Image
		if expectedImage == "" {
			expectedImage = DefaultExecutorImage
		}
		if reqImage != expectedImage {
			continue
		}

		if len(rule.Exact) > 0 {
			if len(tokens) == len(rule.Exact) {
				match := true
				for i := range rule.Exact {
					if tokens[i] != rule.Exact[i] {
						match = false
						break
					}
				}
				if match {
					return tokens, true
				}
			}
		}

		if len(rule.Prefix) > 0 {
			if len(tokens) >= len(rule.Prefix) {
				match := true
				for i := range rule.Prefix {
					if tokens[i] != rule.Prefix[i] {
						match = false
						break
					}
				}
				if match {
					return tokens, true
				}
			}
		}
	}

	return nil, false
}
