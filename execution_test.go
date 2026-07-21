package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func int32Ptr(v int32) *int32 { return &v }

func TestResolveExecutionPolicy(t *testing.T) {
	defaultPolicy, err := resolveExecutionPolicy(SudoRequestExecution{})
	if err != nil {
		t.Fatal(err)
	}
	if defaultPolicy.Mode != ExecutionModeForeground || defaultPolicy.ResourceClass != ResourceClassStandard || defaultPolicy.ActiveDeadlineSeconds != DefaultExecutionDeadline {
		t.Fatalf("default policy = %+v", defaultPolicy)
	}

	managed, err := resolveExecutionPolicy(SudoRequestExecution{
		Mode: ExecutionModeManagedJob, ResourceClass: ResourceClassLongRunning,
		ActiveDeadlineSeconds: int32Ptr(5400),
	})
	if err != nil {
		t.Fatal(err)
	}
	if managed.ActiveDeadlineSeconds != 5400 {
		t.Fatalf("managed policy = %+v", managed)
	}

	invalid := []SudoRequestExecution{
		{ResourceClass: ResourceClassLongRunning},
		{Mode: ExecutionModeManagedJob, ActiveDeadlineSeconds: int32Ptr(600)},
		{Mode: ExecutionModeManagedJob, ResourceClass: ResourceClassLongRunning},
		{Mode: ExecutionModeManagedJob, ResourceClass: ResourceClassLongRunning, ActiveDeadlineSeconds: int32Ptr(299)},
		{Mode: ExecutionModeManagedJob, ResourceClass: ResourceClassLongRunning, ActiveDeadlineSeconds: int32Ptr(7201)},
		{Mode: ExecutionModeForeground, ResourceClass: ResourceClassLongRunning},
	}
	for _, spec := range invalid {
		if _, err := resolveExecutionPolicy(spec); err == nil {
			t.Errorf("resolveExecutionPolicy(%+v) succeeded, want rejection", spec)
		}
	}
}

func TestManagedJobRendersBoundedResourcesDeadlineAndGroundTruth(t *testing.T) {
	sr := srWith(SudoRequestSpec{
		Requester: "alice", Reason: "run drift", Command: "ansible-playbook drift.yaml",
		Execution: SudoRequestExecution{
			Mode: ExecutionModeManagedJob, ResourceClass: ResourceClassLongRunning,
			ActiveDeadlineSeconds: int32Ptr(5400),
		},
		InitContainers: rawList(corev1.Container{
			Name: "prepare", Image: "busybox", Command: []string{"true"},
		}),
	})
	job := buildExecutorJob(sr, ControllerNamespace, "sudo-exec-test", mustDecodePodExtras(t, sr))
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 5400 {
		t.Fatalf("activeDeadlineSeconds=%v", job.Spec.ActiveDeadlineSeconds)
	}
	for _, container := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...) {
		if got := container.Resources.Limits.Memory().String(); got != "2Gi" {
			t.Errorf("container %s memory limit=%s, want 2Gi", container.Name, got)
		}
		if got := container.Resources.Limits.Cpu().String(); got != "2" {
			t.Errorf("container %s cpu limit=%s, want 2", container.Name, got)
		}
	}
	if !hasSpecExtras(sr) {
		t.Fatal("managedJob must be excluded from auto-approval")
	}
	view := newExecutionView(sr)
	if view.Mode != ExecutionModeManagedJob || !strings.Contains(view.Cleanup, "foreground deletion") {
		t.Fatalf("execution view = %+v", view)
	}
	rendered, err := displayJobTemplate(sr, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"activeDeadlineSeconds: 5400", "memory: 2Gi", "foreground"} {
		if !strings.Contains(rendered+view.Cleanup, want) {
			t.Errorf("managed review ground truth missing %q", want)
		}
	}
}

func mustDecodePodExtras(t *testing.T, sr *SudoRequest) *podExtras {
	t.Helper()
	extras, err := decodePodExtras(sr)
	if err != nil {
		t.Fatal(err)
	}
	return extras
}
