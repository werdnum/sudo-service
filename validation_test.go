package main

import "testing"

func TestValidateCommandSyntax(t *testing.T) {
	valid := []string{
		"kubectl get nodes",
		"kubectl get secret foo -n bar -o jsonpath={.data.password}",
		"kubectl rollout restart deployment/foo -n bar",
		"kubectl get pods -A | grep CrashLoop",
		"for p in a b c; do kubectl delete pod $p -n x; done",
		`kubectl get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'`,
		"echo $(date) && kubectl get nodes",
	}
	for _, cmd := range valid {
		if err := validateCommandSyntax(cmd); err != nil {
			t.Errorf("validateCommandSyntax(%q) = %v, want nil", cmd, err)
		}
	}

	invalid := []string{
		"kubectl get secret foo -o jsonpath='{.data.password}", // unterminated single quote
		"kubectl get nodes |",                                  // dangling pipe
		"kubectl get $(date",                                   // unterminated command substitution
		`kubectl get "nodes`,                                   // unterminated double quote
		"if true; then kubectl get nodes",                      // unterminated if
	}
	for _, cmd := range invalid {
		if err := validateCommandSyntax(cmd); err == nil {
			t.Errorf("validateCommandSyntax(%q) = nil, want error", cmd)
		}
	}
}
