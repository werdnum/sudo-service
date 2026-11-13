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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

func cronJobTriggerRequest() *SudoRequest {
	namespace, cronJob, name := "lake-system", "simplefin-sync", "simplefin-sync-manual-20261113"
	return &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "trigger", Namespace: DefaultControllerNamespace, UID: "trigger-request-uid"},
		Spec: SudoRequestSpec{
			Requester: "system:serviceaccount:k8s-agent:k8s-agent-sa",
			Reason:    "re-run SimpleFIN sync after upstream transient timeout",
			Command:   cronJobTriggerCommand(namespace, cronJob, name),
			Playbook: &SudoRequestPlaybook{Name: cronJobTriggerPlaybook, Parameters: map[string]string{
				"namespace": namespace, "cronJob": cronJob, "name": name,
			}},
		},
	}
}

func argocdRefreshRequest() *SudoRequest {
	namespace, name := "argocd", "sudo-service"
	return &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "refresh", Namespace: DefaultControllerNamespace, UID: "refresh-request-uid"},
		Spec: SudoRequestSpec{
			Requester: "system:serviceaccount:k8s-agent:k8s-agent-sa",
			Reason:    "refresh Argo CD Application after merging GitOps PR",
			Command:   argocdRefreshCommand(namespace, name),
			Playbook: &SudoRequestPlaybook{Name: argocdRefreshPlaybook, Parameters: map[string]string{
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

func sampleCronJob() *batchv1.CronJob {
	backoff := int32(1)
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name: "simplefin-sync", Namespace: "lake-system", UID: "cronjob-uid",
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 */6 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"app": "simplefin-sync"},
					Annotations: map[string]string{"example.com/template": "preserved"},
				},
				Spec: batchv1.JobSpec{
					BackoffLimit: &backoff,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: "sync", Image: "ghcr.io/example/simplefin:latest"}},
						},
					},
				},
			},
		},
	}
}

func sampleArgoCDApplication() *unstructured.Unstructured {
	app := newArgoCDApplicationObject()
	app.SetNamespace("argocd")
	app.SetName("sudo-service")
	app.SetUID("app-uid")
	app.SetAnnotations(map[string]string{"existing": "true"})
	return app
}

func cleanupConfig() *PlaybookConfig {
	return &PlaybookConfig{TerminalJobCleanup: []TerminalJobCleanupRule{{
		Requester: "system:serviceaccount:k8s-agent:k8s-agent-sa",
		Namespace: "ansible",
		CronJobs:  []string{"ansible-drift-metrics"},
	}}}
}

func triggerConfig() *PlaybookConfig {
	return &PlaybookConfig{CronJobTrigger: []CronJobTriggerRule{{
		Requester: "system:serviceaccount:k8s-agent:k8s-agent-sa",
		Namespace: "lake-system",
		CronJobs:  []string{"simplefin-sync"},
	}}}
}

func refreshConfig() *PlaybookConfig {
	return &PlaybookConfig{ArgoCDRefresh: []ArgoCDRefreshRule{{
		Requester:            "system:serviceaccount:k8s-agent:k8s-agent-sa",
		Namespace:            "argocd",
		AllowAllApplications: true,
	}}}
}

func TestValidatePlaybookSpecRequiresCanonicalClosedRequest(t *testing.T) {
	for _, sr := range []*SudoRequest{cleanupRequest(), cronJobTriggerRequest(), argocdRefreshRequest()} {
		if err := validatePlaybookSpec(sr); err != nil {
			t.Fatalf("valid playbook %s rejected: %v", sr.Spec.Playbook.Name, err)
		}
		badCmd := sr.DeepCopy()
		badCmd.Spec.Command += " --extra"
		if err := validatePlaybookSpec(badCmd); err == nil || !strings.Contains(err.Error(), "canonical rendering") {
			t.Fatalf("non-canonical command error for %s = %v", sr.Spec.Playbook.Name, err)
		}
		badImage := sr.DeepCopy()
		badImage.Spec.Image = "busybox"
		if err := validatePlaybookSpec(badImage); err == nil || !strings.Contains(err.Error(), "cannot set image") {
			t.Fatalf("custom execution surface error for %s = %v", sr.Spec.Playbook.Name, err)
		}
	}

	badPrefix := cronJobTriggerRequest()
	badPrefix.Spec.Playbook.Parameters["name"] = "other-job-manual-1"
	badPrefix.Spec.Command = cronJobTriggerCommand("lake-system", "simplefin-sync", "other-job-manual-1")
	if err := validatePlaybookSpec(badPrefix); err == nil || !strings.Contains(err.Error(), "must start with") {
		t.Fatalf("wrong manual prefix error = %v", err)
	}

	emptySuffix := cronJobTriggerRequest()
	emptySuffix.Spec.Playbook.Parameters["name"] = "simplefin-sync-manual-"
	emptySuffix.Spec.Command = cronJobTriggerCommand("lake-system", "simplefin-sync", "simplefin-sync-manual-")
	if err := validatePlaybookSpec(emptySuffix); err == nil {
		t.Fatal("trailing-hyphen manual Job name was accepted")
	}

	tooLong := cronJobTriggerRequest()
	longName := "simplefin-sync-manual-" + strings.Repeat("a", 50)
	tooLong.Spec.Playbook.Parameters["name"] = longName
	tooLong.Spec.Command = cronJobTriggerCommand("lake-system", "simplefin-sync", longName)
	if err := validatePlaybookSpec(tooLong); err == nil || !strings.Contains(err.Error(), "playbook Job name") {
		t.Fatalf("overlong Job name error = %v", err)
	}

	unknown := cleanupRequest()
	unknown.Spec.Playbook.Name = "unknown.playbook/v1"
	if err := validatePlaybookSpec(unknown); err == nil || !strings.Contains(err.Error(), "unsupported playbook") {
		t.Fatalf("unknown playbook error = %v", err)
	}
}

func TestLoadPlaybookConfigStrictAndDenyByDefault(t *testing.T) {
	config, err := LoadPlaybookConfig("")
	if err != nil || len(config.TerminalJobCleanup) != 0 || len(config.CronJobTrigger) != 0 || len(config.ArgoCDRefresh) != 0 {
		t.Fatalf("empty config = %#v, %v", config, err)
	}
	path := filepath.Join(t.TempDir(), "playbooks.yaml")
	if err := os.WriteFile(path, []byte(`terminalJobCleanup:
  - requester: system:serviceaccount:k8s-agent:k8s-agent-sa
    namespace: ansible
    cronJobs: [ansible-drift-metrics]
cronJobTrigger:
  - requester: system:serviceaccount:k8s-agent:k8s-agent-sa
    namespace: lake-system
    cronJobs: [simplefin-sync]
argocdRefresh:
  - requester: system:serviceaccount:k8s-agent:k8s-agent-sa
    namespace: argocd
    allowAllApplications: true
`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadPlaybookConfig(path)
	if err != nil || len(config.TerminalJobCleanup) != 1 || len(config.CronJobTrigger) != 1 || len(config.ArgoCDRefresh) != 1 {
		t.Fatalf("loaded config = %#v, %v", config, err)
	}

	ambiguousPath := filepath.Join(t.TempDir(), "ambiguous.yaml")
	if err := os.WriteFile(ambiguousPath, []byte(`argocdRefresh:
  - requester: system:serviceaccount:k8s-agent:k8s-agent-sa
    namespace: argocd
    allowAllApplications: true
    applications: [sudo-service]
`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPlaybookConfig(ambiguousPath); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
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
	if approved.Status.Phase != PhaseApproved || approved.Status.ApprovedBy != "auto-approve" || !approved.Status.PlaybookAutoApproved || approved.Status.PlaybookTargetUID != "job-uid" {
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
	sr.Status = SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "auto-approve", PlaybookAutoApproved: true, PlaybookTargetUID: "original-uid"}
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

func TestHumanNamedAutoApproveDoesNotEnterTypedExecution(t *testing.T) {
	ctx := context.Background()
	scheme := playbookTestScheme(t)
	sr := cleanupRequest()
	// approvedBy is an audit identity and can equal the controller's display
	// label. It must not select the controller-owned automatic execution path.
	sr.Status = SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "auto-approve"}
	job := terminalCronJobChild()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, job).Build()
	r := &SudoRequestReconciler{Client: cl, APIReader: cl, Recorder: record.NewFakeRecorder(10), Broadcaster: NewBroadcaster()}

	handled, _, err := r.executeAutoApprovedPlaybook(ctx, sr)
	if err != nil {
		t.Fatalf("executeAutoApprovedPlaybook: %v", err)
	}
	if handled {
		t.Fatal("human approval was treated as controller-owned automatic approval")
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
		t.Fatalf("target Job changed during human-approved path check: %v", err)
	}
}

func TestPrepareCronJobTriggerRejectsSuspendedActiveAndDuplicateJobs(t *testing.T) {
	ctx := context.Background()
	scheme := playbookTestScheme(t)
	cj := sampleCronJob()

	r := &SudoRequestReconciler{
		APIReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(cj).Build(),
		Playbooks: triggerConfig(),
	}
	prepared, reason, err := r.prepareAutoApprovedPlaybook(ctx, cronJobTriggerRequest())
	if err != nil || prepared == nil || prepared.targetUID != "cronjob-uid" || reason != "" {
		t.Fatalf("prepared=%#v reason=%q err=%v", prepared, reason, err)
	}

	suspended := cj.DeepCopy()
	tr := true
	suspended.Spec.Suspend = &tr
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(suspended).Build()
	if prepared, reason, err := r.prepareAutoApprovedPlaybook(ctx, cronJobTriggerRequest()); err != nil || prepared != nil || !strings.Contains(reason, "suspended") {
		t.Fatalf("suspended prepared=%#v reason=%q err=%v", prepared, reason, err)
	}

	activeManual := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "simplefin-sync-manual-earlier", Namespace: "lake-system"},
	}
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(cj, activeManual).Build()
	if prepared, reason, err := r.prepareAutoApprovedPlaybook(ctx, cronJobTriggerRequest()); err != nil || prepared != nil || !strings.Contains(reason, "already running") {
		t.Fatalf("active manual prepared=%#v reason=%q err=%v", prepared, reason, err)
	}

	reservedSA := cj.DeepCopy()
	reservedSA.Spec.JobTemplate.Spec.Template.Spec.ServiceAccountName = ExecutorSAName
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(reservedSA).Build()
	if prepared, reason, err := r.prepareAutoApprovedPlaybook(ctx, cronJobTriggerRequest()); err != nil || prepared != nil || !strings.Contains(reason, "reserved sudo-service executor identity") {
		t.Fatalf("reserved SA prepared=%#v reason=%q err=%v", prepared, reason, err)
	}
}

func TestAutoApprovedCronJobTriggerCreatesManualJobIdempotentlyWithoutExecutor(t *testing.T) {
	ctx := context.Background()
	scheme := playbookTestScheme(t)
	sr := cronJobTriggerRequest()
	cj := sampleCronJob()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, cj).Build()
	r := &SudoRequestReconciler{
		Client: cl, APIReader: cl, Playbooks: triggerConfig(),
		Recorder: record.NewFakeRecorder(10), Broadcaster: NewBroadcaster(),
	}

	if _, err := r.handleNew(ctx, sr); err != nil {
		t.Fatalf("handleNew: %v", err)
	}
	var approved SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Status.Phase != PhaseApproved || !approved.Status.PlaybookAutoApproved || approved.Status.PlaybookTargetUID != "cronjob-uid" {
		t.Fatalf("approved status = %#v", approved.Status)
	}
	if _, err := r.handleApproved(ctx, &approved); err != nil {
		t.Fatalf("handleApproved: %v", err)
	}

	var created batchv1.Job
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "lake-system", Name: "simplefin-sync-manual-20261113"}, &created); err != nil {
		t.Fatalf("created manual Job missing: %v", err)
	}
	if created.Annotations["cronjob.kubernetes.io/instantiate"] != "manual" ||
		created.Annotations[playbookRequestUIDAnnotation] != string(sr.UID) ||
		created.Annotations["example.com/template"] != "preserved" ||
		created.Labels["app"] != "simplefin-sync" {
		t.Fatalf("created metadata = labels:%v annotations:%v", created.Labels, created.Annotations)
	}
	if len(created.OwnerReferences) != 1 || created.OwnerReferences[0].UID != cj.UID || created.OwnerReferences[0].BlockOwnerDeletion != nil {
		t.Fatalf("ownerReferences = %#v", created.OwnerReferences)
	}
	// Verify child Job ownership is directly compatible with job.cleanup-terminal/v1.
	if !matchesTerminalJobRule(&created, TerminalJobCleanupRule{CronJobs: []string{"simplefin-sync"}}) {
		t.Fatal("triggered manual Job does not match terminalJobCleanup CronJob owner rule")
	}

	// Simulate status-write retry when the Job was already created in a prior reconcile.
	approvedReplay := approved.DeepCopy()
	if err := r.updateStatus(ctx, approvedReplay, func(current *SudoRequest) {
		current.Status.Phase = PhaseApproved
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handleApproved(ctx, approvedReplay); err != nil {
		t.Fatalf("replay handleApproved: %v", err)
	}
	var final SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &final); err != nil {
		t.Fatal(err)
	}
	if final.Status.Phase != PhaseExecuted || final.Status.ExitCode == nil || *final.Status.ExitCode != 0 || final.Status.ExecutorJobName != "" {
		t.Fatalf("final status = %#v", final.Status)
	}
}

func TestAutoApprovedCronJobTriggerRefusesForeignExistingJobOrReplacedCronJob(t *testing.T) {
	ctx := context.Background()
	scheme := playbookTestScheme(t)

	// Case 1: Replaced CronJob UID.
	sr := cronJobTriggerRequest()
	sr.Status = SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "auto-approve", PlaybookAutoApproved: true, PlaybookTargetUID: "old-cronjob-uid"}
	cj := sampleCronJob()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, cj).Build()
	r := &SudoRequestReconciler{Client: cl, APIReader: cl, Playbooks: triggerConfig(), Recorder: record.NewFakeRecorder(10), Broadcaster: NewBroadcaster()}
	if _, err := r.handleApproved(ctx, sr); err != nil {
		t.Fatalf("handleApproved: %v", err)
	}
	var final SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &final); err != nil {
		t.Fatal(err)
	}
	if final.Status.Phase != PhaseFailed || !strings.Contains(final.Status.FailureReason, "replaced") {
		t.Fatalf("expected replaced failure, got %#v", final.Status)
	}

	// Case 2: Foreign Job already occupies target name.
	sr2 := cronJobTriggerRequest()
	sr2.Status = SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "auto-approve", PlaybookAutoApproved: true, PlaybookTargetUID: "cronjob-uid"}
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "simplefin-sync-manual-20261113", Namespace: "lake-system"}}
	cl2 := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr2, cj, foreign).Build()
	r2 := &SudoRequestReconciler{Client: cl2, APIReader: cl2, Playbooks: triggerConfig(), Recorder: record.NewFakeRecorder(10), Broadcaster: NewBroadcaster()}
	if _, err := r2.handleApproved(ctx, sr2); err != nil {
		t.Fatalf("handleApproved foreign job: %v", err)
	}
	if err := cl2.Get(ctx, client.ObjectKeyFromObject(sr2), &final); err != nil {
		t.Fatal(err)
	}
	if final.Status.Phase != PhaseFailed || !strings.Contains(final.Status.FailureReason, "already exists") {
		t.Fatalf("expected already exists failure, got %#v", final.Status)
	}
}

func TestAutoApprovedArgoCDRefreshAnnotatesPinnedApplicationWithoutExecutor(t *testing.T) {
	ctx := context.Background()
	scheme := playbookTestScheme(t)
	sr := argocdRefreshRequest()
	app := sampleArgoCDApplication()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, app).Build()
	r := &SudoRequestReconciler{
		Client: cl, APIReader: cl, Playbooks: refreshConfig(),
		Recorder: record.NewFakeRecorder(10), Broadcaster: NewBroadcaster(),
	}

	if _, err := r.handleNew(ctx, sr); err != nil {
		t.Fatalf("handleNew: %v", err)
	}
	var approved SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Status.Phase != PhaseApproved || !approved.Status.PlaybookAutoApproved || approved.Status.PlaybookTargetUID != "app-uid" {
		t.Fatalf("approved status = %#v", approved.Status)
	}
	if _, err := r.handleApproved(ctx, &approved); err != nil {
		t.Fatalf("handleApproved: %v", err)
	}

	patched := newArgoCDApplicationObject()
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "argocd", Name: "sudo-service"}, patched); err != nil {
		t.Fatalf("get patched Application: %v", err)
	}
	if patched.GetAnnotations()["argocd.argoproj.io/refresh"] != "hard" || patched.GetAnnotations()["existing"] != "true" {
		t.Fatalf("patched annotations = %#v", patched.GetAnnotations())
	}

	var final SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &final); err != nil {
		t.Fatal(err)
	}
	if final.Status.Phase != PhaseExecuted || final.Status.ExitCode == nil || *final.Status.ExitCode != 0 || final.Status.ExecutorJobName != "" {
		t.Fatalf("final status = %#v", final.Status)
	}
}

func TestAutoApprovedArgoCDRefreshRefusesReplacedApplication(t *testing.T) {
	ctx := context.Background()
	scheme := playbookTestScheme(t)
	sr := argocdRefreshRequest()
	sr.Status = SudoRequestStatus{Phase: PhaseApproved, ApprovedBy: "auto-approve", PlaybookAutoApproved: true, PlaybookTargetUID: "original-app-uid"}
	replacement := sampleArgoCDApplication()
	replacement.SetUID("replacement-app-uid")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, replacement).Build()
	r := &SudoRequestReconciler{Client: cl, APIReader: cl, Playbooks: refreshConfig(), Recorder: record.NewFakeRecorder(10), Broadcaster: NewBroadcaster()}

	if _, err := r.handleApproved(ctx, sr); err != nil {
		t.Fatalf("handleApproved: %v", err)
	}
	current := newArgoCDApplicationObject()
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "argocd", Name: "sudo-service"}, current); err != nil {
		t.Fatal(err)
	}
	if _, refreshed := current.GetAnnotations()["argocd.argoproj.io/refresh"]; refreshed {
		t.Fatalf("replacement Application was unexpectedly refreshed: %#v", current.GetAnnotations())
	}
	var final SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &final); err != nil {
		t.Fatal(err)
	}
	if final.Status.Phase != PhaseFailed || !strings.Contains(final.Status.FailureReason, "replaced") {
		t.Fatalf("final status = %#v", final.Status)
	}
}
