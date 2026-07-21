package main

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	ExecutionModeForeground = "foreground"
	ExecutionModeManagedJob = "managedJob"

	ResourceClassStandard    = "standard"
	ResourceClassLongRunning = "long-running"

	DefaultExecutionDeadline int32 = 3600
	MinManagedJobDeadline    int32 = 300
	MaxExecutionDeadline     int32 = 7200

	JobLifecycleCreated          = "Created"
	JobLifecycleRunning          = "Running"
	JobLifecycleResultCaptured   = "ResultCaptured"
	JobLifecycleCleanupRequested = "CleanupRequested"
	JobLifecycleCleaned          = "Cleaned"
)

type effectiveExecutionPolicy struct {
	Mode                  string
	ResourceClass         string
	ActiveDeadlineSeconds int32
}

func resolveExecutionPolicy(spec SudoRequestExecution) (effectiveExecutionPolicy, error) {
	if spec.Mode == "" {
		if spec.ResourceClass != "" || spec.ActiveDeadlineSeconds != nil {
			return effectiveExecutionPolicy{}, fmt.Errorf("execution.mode is required when resourceClass or activeDeadlineSeconds is set")
		}
		return effectiveExecutionPolicy{
			Mode: ExecutionModeForeground, ResourceClass: ResourceClassStandard,
			ActiveDeadlineSeconds: DefaultExecutionDeadline,
		}, nil
	}

	switch spec.Mode {
	case ExecutionModeForeground:
		if spec.ResourceClass != "" && spec.ResourceClass != ResourceClassStandard {
			return effectiveExecutionPolicy{}, fmt.Errorf("foreground execution only supports resourceClass %q", ResourceClassStandard)
		}
		if spec.ActiveDeadlineSeconds != nil && *spec.ActiveDeadlineSeconds != DefaultExecutionDeadline {
			return effectiveExecutionPolicy{}, fmt.Errorf("foreground execution uses the fixed %ds deadline", DefaultExecutionDeadline)
		}
		return effectiveExecutionPolicy{
			Mode: ExecutionModeForeground, ResourceClass: ResourceClassStandard,
			ActiveDeadlineSeconds: DefaultExecutionDeadline,
		}, nil

	case ExecutionModeManagedJob:
		if spec.ResourceClass != ResourceClassLongRunning {
			return effectiveExecutionPolicy{}, fmt.Errorf("managedJob execution requires resourceClass %q", ResourceClassLongRunning)
		}
		if spec.ActiveDeadlineSeconds == nil {
			return effectiveExecutionPolicy{}, fmt.Errorf("managedJob execution requires activeDeadlineSeconds")
		}
		if *spec.ActiveDeadlineSeconds < MinManagedJobDeadline || *spec.ActiveDeadlineSeconds > MaxExecutionDeadline {
			return effectiveExecutionPolicy{}, fmt.Errorf("managedJob activeDeadlineSeconds must be between %d and %d", MinManagedJobDeadline, MaxExecutionDeadline)
		}
		return effectiveExecutionPolicy{
			Mode: ExecutionModeManagedJob, ResourceClass: ResourceClassLongRunning,
			ActiveDeadlineSeconds: *spec.ActiveDeadlineSeconds,
		}, nil
	default:
		return effectiveExecutionPolicy{}, fmt.Errorf("unsupported execution.mode %q", spec.Mode)
	}
}

func executionPolicyFor(sr *SudoRequest) effectiveExecutionPolicy {
	policy, err := resolveExecutionPolicy(sr.Spec.Execution)
	if err != nil {
		// All call sites after admission/reconciliation have validated the spec.
		// The conservative fallback avoids ever rendering an unbounded Job.
		return effectiveExecutionPolicy{
			Mode: ExecutionModeForeground, ResourceClass: ResourceClassStandard,
			ActiveDeadlineSeconds: DefaultExecutionDeadline,
		}
	}
	return policy
}

func isManagedJob(sr *SudoRequest) bool {
	return executionPolicyFor(sr).Mode == ExecutionModeManagedJob
}

func executionResources(class string) corev1.ResourceRequirements {
	if class != ResourceClassLongRunning {
		return standardResources()
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("250m"),
			corev1.ResourceMemory:           resource.MustParse("256Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("2"),
			corev1.ResourceMemory:           resource.MustParse("2Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
	}
}
