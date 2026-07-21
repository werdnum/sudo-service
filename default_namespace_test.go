package main

import batchv1 "k8s.io/api/batch/v1"

// These wrappers keep legacy unit fixtures concise while production helpers
// require an explicit runtime namespace. Tests that exercise a non-default
// installation call the production helpers directly.

func validateSpecExtrasForTest(sr *SudoRequest) error {
	return validateSpecExtras(sr, DefaultControllerNamespace)
}

func executorNamespaceForTest(sr *SudoRequest) string {
	return executorNamespace(sr, DefaultControllerNamespace)
}

func clusterAdminEnabledForTest(sr *SudoRequest) bool {
	return clusterAdminEnabled(sr, DefaultControllerNamespace)
}

func executorServiceAccountForTest(sr *SudoRequest) (string, *bool) {
	return executorServiceAccount(sr, DefaultControllerNamespace)
}

func buildExecutorJobForTest(sr *SudoRequest, namespace, name string, extras *podExtras) batchv1.Job {
	return buildExecutorJob(sr, namespace, name, extras, DefaultControllerNamespace)
}

func displayPodTemplateForTest(sr *SudoRequest, redactEnv bool) (string, error) {
	return displayPodTemplate(sr, redactEnv, DefaultControllerNamespace)
}

func newSpecExtrasViewForTest(sr *SudoRequest, redactEnv bool) specExtrasView {
	return newSpecExtrasView(sr, redactEnv, DefaultControllerNamespace)
}

func specExtrasTextForTest(sr *SudoRequest, redactEnv bool) string {
	return specExtrasText(sr, redactEnv, DefaultControllerNamespace)
}
