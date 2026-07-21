package main

import (
	"strings"
	"testing"
)

func ref(namespace, kind, name string) TypedResourceRef {
	return TypedResourceRef{Namespace: namespace, Kind: kind, Name: name}
}

func TestCompileTypedActions(t *testing.T) {
	tests := []struct {
		name       string
		action     TypedAction
		command    string
		permission string
	}{
		{
			name: "exact jobs canonical order and grouping",
			action: TypedAction{Version: "v1", Operation: OperationJobDelete, Resources: []TypedResourceRef{
				ref("zeta", "Job", "second"), ref("alpha", "Job", "one"), ref("zeta", "Job", "first"),
			}},
			command: "set -eu\nkubectl delete job --namespace alpha --ignore-not-found=true --wait=true -- one\n" +
				"kubectl delete job --namespace zeta --ignore-not-found=true --wait=true -- first second",
			permission: "Delete the exact Jobs alpha/one, zeta/first, and zeta/second.",
		},
		{
			name:       "cronjob with cleanup",
			action:     TypedAction{Version: "v1", Operation: OperationCronJobRun, Resources: []TypedResourceRef{ref("ops", "CronJob", "drift")}, JobName: "drift-manual-20260721153000"},
			command:    "set -eu\nkubectl create job drift-manual-20260721153000 --from=cronjob/drift --namespace ops --dry-run=client --output=json >/tmp/typed-cronjob.json\nkubectl patch --local --filename=/tmp/typed-cronjob.json --type=merge --patch='{\"spec\":{\"ttlSecondsAfterFinished\":86400}}' --output=json >/tmp/typed-cronjob-patched.json\nkubectl create --filename=/tmp/typed-cronjob-patched.json",
			permission: "Create Job ops/drift-manual-20260721153000 from CronJob ops/drift and delete it 24 hours after it finishes.",
		},
		{
			name:       "deployment restart",
			action:     TypedAction{Version: "v1", Operation: OperationWorkloadRestart, Resources: []TypedResourceRef{ref("apps", "Deployment", "web")}},
			command:    "kubectl rollout restart deployment/web --namespace apps",
			permission: "Restart Deployment apps/web.",
		},
		{
			name:       "scoped secret key",
			action:     TypedAction{Version: "v1", Operation: OperationSecretRead, Resources: []TypedResourceRef{ref("apps", "Secret", "credentials")}, Key: "db.password"},
			command:    `kubectl get secret credentials --namespace apps --output=go-template='{{ index .data "db.password" | base64decode }}'`,
			permission: "Read key db.password from Secret apps/credentials.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := compileTypedAction(&tt.action)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Command != tt.command {
				t.Fatalf("command:\n%s\nwant:\n%s", plan.Command, tt.command)
			}
			if plan.PermissionRequest != tt.permission {
				t.Fatalf("permission = %q, want %q", plan.PermissionRequest, tt.permission)
			}
			sr := &SudoRequest{Spec: SudoRequestSpec{
				Reason: "test context", Command: plan.Command, Action: &tt.action,
				Profile: DefaultExecutorProfile,
			}}
			if _, _, _, err := resolveAndPreflight(sr); err != nil {
				t.Fatalf("canonical command failed profile preflight: %v", err)
			}
			if _, err := validateTypedActionBinding(sr); err != nil {
				t.Fatalf("canonical command failed binding validation: %v", err)
			}
		})
	}
}

func TestCompileTypedActionRejectsBroadOrAmbiguousSemantics(t *testing.T) {
	tests := []TypedAction{
		{Version: "v1", Operation: OperationJobDelete, Resources: []TypedResourceRef{ref("ops", "Pod", "anything")}},
		{Version: "v1", Operation: OperationJobDelete, Resources: []TypedResourceRef{ref("ops", "Job", "one"), ref("ops", "Job", "one")}},
		{Version: "v1", Operation: OperationWorkloadRestart, Resources: []TypedResourceRef{ref("apps", "Pod", "web")}},
		{Version: "v1", Operation: OperationSecretRead, Resources: []TypedResourceRef{ref("apps", "Secret", "creds")}, Key: "*"},
		{Version: "v1", Operation: OperationCronJobRun, Resources: []TypedResourceRef{ref("ops", "CronJob", "drift")}, JobName: ""},
	}
	for _, action := range tests {
		if _, err := compileTypedAction(&action); err == nil {
			t.Fatalf("compileTypedAction(%+v) succeeded, want rejection", action)
		}
	}
}

func TestTypedActionBindingRejectsDivergentCommandAndWidening(t *testing.T) {
	action := &TypedAction{Version: "v1", Operation: OperationWorkloadRestart, Resources: []TypedResourceRef{ref("apps", "Deployment", "web")}}
	plan, err := compileTypedAction(action)
	if err != nil {
		t.Fatal(err)
	}
	sr := &SudoRequest{Spec: SudoRequestSpec{Reason: "recover web", Command: plan.Command, Action: action, Profile: DefaultExecutorProfile}}
	if _, err := validateTypedActionBinding(sr); err != nil {
		t.Fatalf("canonical binding rejected: %v", err)
	}
	if _, ok := autoApproveTokens(sr); ok {
		t.Fatal("typed action was auto-approvable")
	}

	sr.Spec.Command += "; kubectl delete pods --all -A"
	if _, err := validateTypedActionBinding(sr); err == nil || !strings.Contains(err.Error(), "canonical expansion") {
		t.Fatalf("divergent command error = %v", err)
	}
	sr.Spec.Command = plan.Command
	sr.Spec.Image = "busybox"
	if _, err := validateTypedActionBinding(sr); err == nil {
		t.Fatal("typed action with image override was accepted")
	}
}
