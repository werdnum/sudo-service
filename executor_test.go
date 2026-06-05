package main

import "testing"

func TestExecutorLabelsDoNotMatchControllerService(t *testing.T) {
	labels := executorLabels()

	if got := labels[AppLabelKey]; got != ExecutorAppLabelValue {
		t.Fatalf("executor app label = %q, want %q", got, ExecutorAppLabelValue)
	}
	if got := labels[RoleLabelKey]; got != ExecutorRoleLabelValue {
		t.Fatalf("executor role label = %q, want %q", got, ExecutorRoleLabelValue)
	}
	if labels[AppLabelKey] == ControllerAppLabelValue {
		t.Fatalf("executor app label must not match controller service selector value %q", ControllerAppLabelValue)
	}
}
