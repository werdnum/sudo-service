package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func playbookTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := addKnownTypes(s); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func cleanupRequest() *SudoRequest {
	namespace, name := "ansible", "ansible-drift-metrics-12345"
	return &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup", Namespace: DefaultControllerNamespace, UID: "request-uid"},
		Spec: SudoRequestSpec{
			Requester: "system:serviceaccount:k8s-agent:k8s-agent-sa",
			Reason:    "clear a retained failed Job alert",
			Command:   terminalJobCleanupCommand(namespace, name),
			Playbook: &SudoRequestPlaybook{Name: terminalJobCleanupPlaybook, Parameters: map[string]string{
				"namespace": namespace, "name": name,
			}},
		},
	}
}

func terminalCronJobChild() *batchv1.Job {
	controller := true
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ansible-drift-metrics-12345", Namespace: "ansible", UID: "job-uid",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "CronJob", Name: "ansible-drift-metrics", Controller: &controller,
			}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}},
	}
}

func cleanupConfig() *PlaybookConfig {
	return &PlaybookConfig{TerminalJobCleanup: []TerminalJobCleanupRule{{
		Requester: "system:serviceaccount:k8s-agent:k8s-agent-sa",
		Namespace: "ansible",
		CronJobs:  []string{"ansible-drift-metrics"},
	}}}
}

func TestValidatePlaybookSpecRequiresCanonicalClosedRequest(t *testing.T) {
	sr := cleanupRequest()
	if err := validatePlaybookSpec(sr); err != nil {
		t.Fatalf("valid playbook rejected: %v", err)
	}
	sr.Spec.Command += " --all"
	if err := validatePlaybookSpec(sr); err == nil || !strings.Contains(err.Error(), "canonical rendering") {
		t.Fatalf("non-canonical command error = %v", err)
	}
	sr = cleanupRequest()
	sr.Spec.Image = "busybox"
	if err := validatePlaybookSpec(sr); err == nil || !strings.Contains(err.Error(), "cannot set image") {
		t.Fatalf("custom execution surface error = %v", err)
	}
}

func TestLoadPlaybookConfigStrictAndDenyByDefault(t *testing.T) {
	config, err := LoadPlaybookConfig("")
	if err != nil || len(config.TerminalJobCleanup) != 0 {
		t.Fatalf("empty config = %#v, %v", config, err)
	}
	path := filepath.Join(t.TempDir(), "playbooks.yaml")
	if err := os.WriteFile(path, []byte(`terminalJobCleanup:
  - requester: system:serviceaccount:k8s-agent:k8s-agent-sa
    namespace: ansible
    cronJobs: [ansible-drift-metrics]
`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadPlaybookConfig(path)
	if err != nil || len(config.TerminalJobCleanup) != 1 {
		t.Fatalf("loaded config = %#v, %v", config, err)
	}
}

func TestPrepareTerminalJobCleanupChecksIdentityStateAndOwner(t *testing.T) {
	scheme := playbookTestScheme(t)
	job := terminalCronJobChild()
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()
	r := &SudoRequestReconciler{APIReader: reader, Playbooks: cleanupConfig()}

	prepared, reason, err := r.prepareAutoApprovedPlaybook(context.Background(), cleanupRequest())
	if err != nil || prepared == nil || prepared.targetUID != "job-uid" || reason != "" {
		t.Fatalf("prepared=%#v reason=%q err=%v", prepared, reason, err)
	}

	nonterminal := job.DeepCopy()
	nonterminal.Status.Conditions = nil
	reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(nonterminal).Build()
	r.APIReader = reader
	prepared, reason, err = r.prepareAutoApprovedPlaybook(context.Background(), cleanupRequest())
	if err != nil || prepared != nil || !strings.Contains(reason, "not terminal") {
		t.Fatalf("nonterminal prepared=%#v reason=%q err=%v", prepared, reason, err)
	}

	wrongRequester := cleanupRequest()
	wrongRequester.Spec.Requester = "mallory"
	prepared, reason, err = r.prepareAutoApprovedPlaybook(context.Background(), wrongRequester)
	if err != nil || prepared != nil || !strings.Contains(reason, "not allowlisted") {
		t.Fatalf("wrong requester prepared=%#v reason=%q err=%v", prepared, reason, err)
	}
}

func TestAutoApprovedTerminalJobCleanupDeletesPinnedUIDWithoutExecutor(t *testing.T) {
	ctx := context.Background()
	scheme := playbookTestScheme(t)
	sr := cleanupRequest()
	job := terminalCronJobChild()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, job).Build()
	r := &SudoRequestReconciler{
		Client: cl, APIReader: cl, Playbooks: cleanupConfig(),
		Recorder: record.NewFakeRecorder(10), Broadcaster: NewBroadcaster(),
	}

	if _, err := r.handleNew(ctx, sr); err != nil {
		t.Fatalf("handleNew: %v", err)
	}
	var approved SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Status.Phase != PhaseApproved || approved.Status.ApprovedBy != "auto-approve" || approved.Status.PlaybookTargetUID != "job-uid" {
		t.Fatalf("approved status = %#v", approved.Status)
	}
	if _, err := r.handleApproved(ctx, &approved); err != nil {
		t.Fatalf("handleApproved: %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("target Job still exists: %v", err)
	}
	var final SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &final); err != nil {
		t.Fatal(err)
	}
	if final.Status.Phase != PhaseExecuted || final.Status.ExitCode == nil || *final.Status.ExitCode != 0 || final.Status.ExecutorJobName != "" {
		t.Fatalf("final status = %#v", final.Status)
	}
}

func TestAutoApprovedCleanupRefusesReplacementJob(t *testing.T) {
	ctx := context.Background()
	scheme := playbookTestScheme(t)
	sr := cleanupRequest()
	sr.Status = SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "auto-approve", PlaybookTargetUID: "original-uid"}
	replacement := terminalCronJobChild()
	replacement.UID = types.UID("replacement-uid")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, replacement).Build()
	r := &SudoRequestReconciler{Client: cl, APIReader: cl, Playbooks: cleanupConfig(), Recorder: record.NewFakeRecorder(10), Broadcaster: NewBroadcaster()}

	if _, err := r.handleApproved(ctx, sr); err != nil {
		t.Fatalf("handleApproved: %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(replacement), &batchv1.Job{}); err != nil {
		t.Fatalf("replacement Job was deleted: %v", err)
	}
	var final SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &final); err != nil {
		t.Fatal(err)
	}
	if final.Status.Phase != PhaseFailed || !strings.Contains(final.Status.FailureReason, "replaced") {
		t.Fatalf("final status = %#v", final.Status)
	}
}
