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

func TestValidateSpecExtrasInitContainerSidecarAndDevices(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	sidecar := []corev1.Container{{Name: "s", Image: "busybox", RestartPolicy: &always}}
	if err := validateSpecExtras(srWith(SudoRequestSpec{InitContainers: sidecar})); err == nil ||
		!strings.Contains(err.Error(), "restartPolicy") {
		t.Errorf("sidecar init container: got %v, want restartPolicy rejection", err)
	}

	devices := []corev1.Container{{Name: "d", Image: "busybox", VolumeDevices: []corev1.VolumeDevice{{Name: "blk", DevicePath: "/dev/xvda"}}}}
	if err := validateSpecExtras(srWith(SudoRequestSpec{InitContainers: devices})); err == nil ||
		!strings.Contains(err.Error(), "volumeDevices") {
		t.Errorf("init container volumeDevices: got %v, want rejection", err)
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

func TestValidateSpecExtrasReservedStdinMount(t *testing.T) {
	// A requester volume reusing the reserved stdin name is rejected.
	v := []corev1.Volume{{Name: stdinVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	if err := validateSpecExtras(srWith(SudoRequestSpec{Volumes: v})); err == nil {
		t.Error("reserved stdin volume name: got nil, want rejection")
	}
	// A mount reusing the reserved name or path is rejected.
	if err := validateSpecExtras(srWith(SudoRequestSpec{VolumeMounts: []corev1.VolumeMount{{Name: stdinVolumeName, MountPath: "/x"}}})); err == nil {
		t.Error("reserved stdin mount name: got nil, want rejection")
	}
	if err := validateSpecExtras(srWith(SudoRequestSpec{VolumeMounts: []corev1.VolumeMount{{Name: "ok", MountPath: stdinMountDir}}})); err == nil {
		t.Error("reserved stdin mount path: got nil, want rejection")
	}
}

func TestDescribeVolumeSourceMatchesValidation(t *testing.T) {
	// describeVolumeSource is the single allowlist source of truth: anything it
	// marks allowed must pass validateVolumeSource and vice versa.
	cases := []struct {
		v       corev1.Volume
		allowed bool
	}{
		{corev1.Volume{Name: "a", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}}, true},
		{corev1.Volume{Name: "b", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}, false},
		{corev1.Volume{Name: "c", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "x"}}}, false},
	}
	for _, tc := range cases {
		_, allowed := describeVolumeSource(tc.v)
		if allowed != tc.allowed {
			t.Errorf("describeVolumeSource(%q) allowed=%v, want %v", tc.v.Name, allowed, tc.allowed)
		}
		err := validateVolumeSource(tc.v)
		if tc.allowed && err != nil {
			t.Errorf("validateVolumeSource(%q) = %v, want nil", tc.v.Name, err)
		}
		if !tc.allowed && err == nil {
			t.Errorf("validateVolumeSource(%q) = nil, want error", tc.v.Name)
		}
	}
}

func TestInitContainerCommandSurfacedToReviewer(t *testing.T) {
	sr := srWith(SudoRequestSpec{
		InitContainers: []corev1.Container{{
			Name:         "copy",
			Image:        "rclone/rclone",
			Command:      []string{"/bin/sh", "-c", "cp x /tools/y"},
			VolumeMounts: []corev1.VolumeMount{{Name: "tools", MountPath: "/tools"}},
		}},
	})
	v := newSpecExtrasView(sr)
	if len(v.InitContainers) != 1 {
		t.Fatalf("expected 1 init container view, got %d", len(v.InitContainers))
	}
	ic := v.InitContainers[0]
	if ic.Command != "/bin/sh -c cp x /tools/y" {
		t.Errorf("init container command not surfaced: %q", ic.Command)
	}
	if len(ic.Mounts) != 1 {
		t.Errorf("init container mounts not surfaced: %v", ic.Mounts)
	}
	// And the plain-text rendering (push + summarizer) includes the command.
	if txt := specExtrasText(sr); !strings.Contains(txt, "cp x /tools/y") {
		t.Errorf("specExtrasText omits init container command: %q", txt)
	}
}

func TestValidateSpecExtrasRejectsProjected(t *testing.T) {
	v := []corev1.Volume{{Name: "p", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}}}
	err := validateSpecExtras(srWith(SudoRequestSpec{Volumes: v}))
	if err == nil || !strings.Contains(err.Error(), "projected") {
		t.Fatalf("projected volume: got %v, want projected rejection", err)
	}
	if _, allowed := describeVolumeSource(v[0]); allowed {
		t.Error("describeVolumeSource marks projected allowed; must be false")
	}
}

func TestValidateSpecExtrasMountReferences(t *testing.T) {
	// Mount referencing an undefined volume is rejected.
	err := validateSpecExtras(srWith(SudoRequestSpec{
		VolumeMounts: []corev1.VolumeMount{{Name: "missing", MountPath: "/x"}},
	}))
	if err == nil || !strings.Contains(err.Error(), "no volume named") {
		t.Fatalf("dangling mount: got %v, want rejection", err)
	}

	// Duplicate mountPath within a container is rejected.
	err = validateSpecExtras(srWith(SudoRequestSpec{
		Volumes: []corev1.Volume{
			{Name: "a", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "b", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "a", MountPath: "/same"},
			{Name: "b", MountPath: "/same"},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "duplicate mountPath") {
		t.Fatalf("duplicate mountPath: got %v, want rejection", err)
	}

	// A mount referencing a defined volume (and the stdin volume) is accepted.
	if err := validateSpecExtras(srWith(SudoRequestSpec{
		Stdin:        "data",
		Volumes:      []corev1.Volume{{Name: "a", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		VolumeMounts: []corev1.VolumeMount{{Name: "a", MountPath: "/a"}},
	})); err != nil {
		t.Fatalf("valid mounts rejected: %v", err)
	}

	// An init container referencing an undefined volume is rejected.
	err = validateSpecExtras(srWith(SudoRequestSpec{
		InitContainers: []corev1.Container{{
			Name: "i", Image: "busybox",
			VolumeMounts: []corev1.VolumeMount{{Name: "nope", MountPath: "/x"}},
		}},
	}))
	if err == nil || !strings.Contains(err.Error(), "no volume named") {
		t.Fatalf("init dangling mount: got %v, want rejection", err)
	}
}

func TestDescribeEnvSurfacesValuesAndSources(t *testing.T) {
	sr := srWith(SudoRequestSpec{Env: []corev1.EnvVar{
		{Name: "LITERAL", Value: "KUBECONFIG=/x"},
		{Name: "FROMSECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "creds"}, Key: "token"}}},
	}})
	v := newSpecExtrasView(sr)
	joined := strings.Join(v.Env, " | ")
	if !strings.Contains(joined, "LITERAL=KUBECONFIG=/x") {
		t.Errorf("literal env value not surfaced: %q", joined)
	}
	if !strings.Contains(joined, "FROMSECRET <- secret/creds:token") {
		t.Errorf("secret-ref env source not surfaced: %q", joined)
	}
}

func TestContainerToReportInitStillRunning(t *testing.T) {
	// Executor waiting, init still running -> nothing terminated yet, requeue.
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{
			{Name: "copy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		},
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "executor", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
		},
	}}
	if _, _, err := containerToReport(pod); err == nil {
		t.Error("expected error (nothing terminated yet), got nil")
	}
}

func TestContainerToReportPicksFailedInit(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{
			{Name: "copy", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}}},
		},
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "executor", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
		},
	}}
	c, code, err := containerToReport(pod)
	if err != nil || c != "copy" || code != 3 {
		t.Fatalf("got (%q, %d, %v), want (copy, 3, nil)", c, code, err)
	}

	// Executor terminated -> report it.
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	c, code, err = containerToReport(pod)
	if err != nil || c != "executor" || code != 0 {
		t.Fatalf("got (%q, %d, %v), want (executor, 0, nil)", c, code, err)
	}
}

func TestInitContainerResourcesAreStamped(t *testing.T) {
	// A requester init container with no resources still ends up bounded.
	r := &SudoRequestReconciler{}
	sr := srWith(SudoRequestSpec{InitContainers: []corev1.Container{{Name: "i", Image: "busybox"}}})
	job := r.buildExecutorJob(sr, ControllerNamespace, jobName(sr))
	got := job.Spec.Template.Spec.InitContainers[0].Resources
	if got.Limits.Memory().IsZero() || got.Limits.Cpu().IsZero() {
		t.Errorf("init container resources not stamped: %+v", got)
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
