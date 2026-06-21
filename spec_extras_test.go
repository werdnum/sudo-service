package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func srWith(spec SudoRequestSpec) *SudoRequest {
	return &SudoRequest{Spec: spec}
}

func boolPtr(b bool) *bool { return &b }

func TestValidateSpecExtrasVolumeAllowlist(t *testing.T) {
	allowed := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "backup"}}},
		{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-0"}}},
	}
	if err := validateSpecExtras(srWith(SudoRequestSpec{Volumes: allowed})); err != nil {
		t.Fatalf("allowed volumes rejected: %v", err)
	}

	hostPath := []corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
	err := validateSpecExtras(srWith(SudoRequestSpec{Volumes: hostPath}))
	if err == nil || !strings.Contains(err.Error(), "hostPath") {
		t.Fatalf("hostPath volume: got %v, want hostPath rejection", err)
	}

	// A source outside the allowlist (e.g. an inline CSI volume) is rejected.
	csi := []corev1.Volume{{Name: "c", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "x"}}}}
	if err := validateSpecExtras(srWith(SudoRequestSpec{Volumes: csi})); err == nil {
		t.Fatal("CSI volume: got nil, want rejection")
	}
}

func TestValidateSpecExtrasInitContainerSecurityContext(t *testing.T) {
	ok := []corev1.Container{{Name: "copy", Image: "busybox"}}
	if err := validateSpecExtras(srWith(SudoRequestSpec{InitContainers: ok})); err != nil {
		t.Fatalf("plain init container rejected: %v", err)
	}

	withSC := []corev1.Container{{Name: "copy", Image: "busybox", SecurityContext: &corev1.SecurityContext{}}}
	if err := validateSpecExtras(srWith(SudoRequestSpec{InitContainers: withSC})); err == nil {
		t.Fatal("init container securityContext: got nil, want rejection")
	}

	missingImage := []corev1.Container{{Name: "copy"}}
	if err := validateSpecExtras(srWith(SudoRequestSpec{InitContainers: missingImage})); err == nil {
		t.Fatal("init container without image: got nil, want rejection")
	}
}

func TestValidateSpecExtrasClusterAdminNamespaceExclusivity(t *testing.T) {
	// cluster-admin in another namespace is incoherent and rejected.
	err := validateSpecExtras(srWith(SudoRequestSpec{
		Namespace:  "seaweedfs",
		Privileges: SudoRequestPrivileges{ClusterAdmin: boolPtr(true)},
	}))
	if err == nil || !strings.Contains(err.Error(), "clusterAdmin") {
		t.Fatalf("cluster-admin off-namespace: got %v, want rejection", err)
	}

	// cluster-admin disabled in another namespace is fine.
	if err := validateSpecExtras(srWith(SudoRequestSpec{
		Namespace:  "seaweedfs",
		Privileges: SudoRequestPrivileges{ClusterAdmin: boolPtr(false)},
	})); err != nil {
		t.Fatalf("cross-namespace without cluster-admin rejected: %v", err)
	}
}

func TestClusterAdminEnabledDefaults(t *testing.T) {
	cases := []struct {
		name string
		sr   *SudoRequest
		want bool
	}{
		{"default in controller ns", srWith(SudoRequestSpec{}), true},
		{"explicit controller ns", srWith(SudoRequestSpec{Namespace: ControllerNamespace}), true},
		{"disabled in controller ns", srWith(SudoRequestSpec{Privileges: SudoRequestPrivileges{ClusterAdmin: boolPtr(false)}}), false},
		{"other namespace defaults off", srWith(SudoRequestSpec{Namespace: "seaweedfs"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterAdminEnabled(tc.sr); got != tc.want {
				t.Errorf("clusterAdminEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExecutorServiceAccount(t *testing.T) {
	name, automount := executorServiceAccount(srWith(SudoRequestSpec{}))
	if name != ExecutorSAName || automount != nil {
		t.Errorf("cluster-admin path: got (%q, %v), want (%q, nil)", name, automount, ExecutorSAName)
	}

	name, automount = executorServiceAccount(srWith(SudoRequestSpec{Namespace: "seaweedfs"}))
	if name != "default" || automount == nil || *automount {
		t.Errorf("cross-namespace path: got (%q, %v), want (default, false)", name, automount)
	}
}

func TestExecutorCommandStdinWrapper(t *testing.T) {
	// Plain command, no extras: historical sh -c form.
	got := executorCommand(srWith(SudoRequestSpec{Command: "kubectl get nodes"}))
	want := []string{"/bin/sh", "-c", "kubectl get nodes"}
	if !equalSlice(got, want) {
		t.Errorf("plain command = %v, want %v", got, want)
	}

	// With stdin: command travels as a positional arg ($1), never interpolated,
	// and stdin is redirected from the mounted payload.
	got = executorCommand(srWith(SudoRequestSpec{Command: "kubectl apply -f -", Stdin: "kind: Job"}))
	if len(got) != 5 || got[4] != "kubectl apply -f -" {
		t.Fatalf("stdin command did not pass the command as a positional arg: %v", got)
	}
	if !strings.Contains(got[2], stdinMountDir+"/stdin") || !strings.Contains(got[2], `"$1"`) {
		t.Errorf("stdin wrapper script = %q, want redirect from payload and $1 expansion", got[2])
	}
}

func TestExecutorCommandExtrasBypassAutoApprove(t *testing.T) {
	// A command on the auto-approve allowlist must NOT be tokenized when the
	// request carries extras — it should still run via the shell so the mounts
	// and namespace take effect, and (per the reconciler) require a human.
	autoApproveRules = []AutoApproveRule{{Exact: []string{"echo", "hi"}}}
	defer func() { autoApproveRules = nil }()

	got := executorCommand(srWith(SudoRequestSpec{Command: "echo hi", Namespace: "seaweedfs"}))
	if equalSlice(got, []string{"echo", "hi"}) {
		t.Errorf("extras request was auto-approve tokenized: %v", got)
	}
}

func TestHasSpecExtras(t *testing.T) {
	if hasSpecExtras(srWith(SudoRequestSpec{Command: "kubectl get nodes"})) {
		t.Error("plain command reported as having extras")
	}
	if !hasSpecExtras(srWith(SudoRequestSpec{Stdin: "x"})) {
		t.Error("stdin not detected as extra")
	}
	if !hasSpecExtras(srWith(SudoRequestSpec{Privileges: SudoRequestPrivileges{ClusterAdmin: boolPtr(false)}})) {
		t.Error("explicit privilege toggle not detected as extra")
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
