package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func srWith(spec SudoRequestSpec) *SudoRequest {
	return &SudoRequest{Spec: spec}
}

func boolPtr(b bool) *bool { return &b }

// rawList marshals typed pod-field items into the runtime.RawExtension slices the
// spec now stores, so tests can keep writing concrete corev1 values.
func rawList[T any](items ...T) []runtime.RawExtension {
	out := make([]runtime.RawExtension, len(items))
	for i := range items {
		b, err := json.Marshal(items[i])
		if err != nil {
			panic(err)
		}
		out[i] = runtime.RawExtension{Raw: b}
	}
	return out
}

func TestValidateSpecExtrasVolumeAllowlist(t *testing.T) {
	allowed := []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "backup"}}},
		{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{}}},
		{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-0"}}},
	}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{Volumes: rawList(allowed...)})); err != nil {
		t.Fatalf("allowed volumes rejected: %v", err)
	}

	hostPath := []corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
	err := validateSpecExtrasForTest(srWith(SudoRequestSpec{Volumes: rawList(hostPath...)}))
	if err == nil || !strings.Contains(err.Error(), "hostPath") {
		t.Fatalf("hostPath volume: got %v, want hostPath rejection", err)
	}

	// A source outside the allowlist (e.g. an inline CSI volume) is rejected.
	csi := []corev1.Volume{{Name: "c", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "x"}}}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{Volumes: rawList(csi...)})); err == nil {
		t.Fatal("CSI volume: got nil, want rejection")
	}
}

func TestValidateSpecExtrasInitContainerSidecarAndDevices(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	sidecar := []corev1.Container{{Name: "s", Image: "busybox", Command: []string{"sh"}, RestartPolicy: &always}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(sidecar...)})); err == nil ||
		!strings.Contains(err.Error(), "permitted") {
		t.Errorf("sidecar init container: got %v, want allowlist rejection", err)
	}

	devices := []corev1.Container{{Name: "d", Image: "busybox", Command: []string{"sh"}, VolumeDevices: []corev1.VolumeDevice{{Name: "blk", DevicePath: "/dev/xvda"}}}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(devices...)})); err == nil ||
		!strings.Contains(err.Error(), "permitted") {
		t.Errorf("init container volumeDevices: got %v, want allowlist rejection", err)
	}
}

func TestValidateSpecExtrasInitContainerSecurityContext(t *testing.T) {
	ok := []corev1.Container{{Name: "copy", Image: "busybox", Command: []string{"sh"}}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(ok...)})); err != nil {
		t.Fatalf("plain init container rejected: %v", err)
	}

	withSC := []corev1.Container{{Name: "copy", Image: "busybox", Command: []string{"sh"}, SecurityContext: &corev1.SecurityContext{}}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(withSC...)})); err == nil {
		t.Fatal("init container securityContext: got nil, want rejection")
	}

	missingImage := []corev1.Container{{Name: "copy"}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(missingImage...)})); err == nil {
		t.Fatal("init container without image: got nil, want rejection")
	}
}

func TestDisplayPodTemplate(t *testing.T) {
	sr := srWith(SudoRequestSpec{
		Command: "weed export",
		Image:   "chrislusf/seaweedfs:3.84",
		Env:     rawList(corev1.EnvVar{Name: "AWS_SECRET", Value: "super-secret"}),
		Volumes: rawList(corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-0"}}}),
		VolumeMounts: rawList(corev1.VolumeMount{Name: "data", MountPath: "/data", ReadOnly: true}),
	})

	raw, err := displayPodTemplateForTest(sr, false)
	if err != nil {
		t.Fatalf("displayPodTemplate: %v", err)
	}
	for _, want := range []string{"chrislusf/seaweedfs:3.84", "weed export", "/data", "data-0", "super-secret"} {
		if !strings.Contains(raw, want) {
			t.Errorf("pod template missing %q:\n%s", want, raw)
		}
	}
	red, err := displayPodTemplateForTest(sr, true)
	if err != nil {
		t.Fatalf("displayPodTemplate redacted: %v", err)
	}
	if strings.Contains(red, "super-secret") || !strings.Contains(red, "<redacted>") {
		t.Errorf("redacted template leaked env value:\n%s", red)
	}
	if !strings.Contains(red, "data-0") {
		t.Errorf("redaction dropped non-env content:\n%s", red)
	}
}

func TestValidateSpecExtrasRejectsHiddenFields(t *testing.T) {
	// workingDir changes what relative commands do but isn't rendered -> rejected.
	wd := []corev1.Container{{Name: "c", Image: "busybox", Command: []string{"sh"}, WorkingDir: "/x"}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(wd...)})); err == nil ||
		!strings.Contains(err.Error(), "permitted") {
		t.Errorf("init workingDir: got %v, want allowlist rejection", err)
	}

	// A mount with subPathExpr / mountPropagation isn't fully rendered -> rejected.
	prop := corev1.MountPropagationBidirectional
	for _, m := range []corev1.VolumeMount{
		{Name: "v", MountPath: "/d", SubPathExpr: "$(POD_NAME)"},
		{Name: "v", MountPath: "/d", MountPropagation: &prop},
	} {
		sr := srWith(SudoRequestSpec{
			Volumes:      rawList(corev1.Volume{Name: "v", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}),
			VolumeMounts: rawList(m),
		})
		if err := validateSpecExtrasForTest(sr); err == nil || !strings.Contains(err.Error(), "may only set") {
			t.Errorf("hidden mount field: got %v, want rejection", err)
		}
	}
}

func TestVolumeAndArgvRenderingFaithful(t *testing.T) {
	// secret.items key->path mappings are surfaced.
	vol := corev1.Volume{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: "backup", Items: []corev1.KeyToPath{{Key: "admin-token", Path: "innocuous.txt"}}}}}
	if desc, _ := describeVolumeSource(vol); !strings.Contains(desc, "admin-token->innocuous.txt") {
		t.Errorf("secret items not rendered: %q", desc)
	}
	// argv boundaries preserved by shell quoting.
	if got := shellJoin([]string{"sh", "-c", "rm -rf /x"}); got != "sh -c 'rm -rf /x'" {
		t.Errorf("shellJoin = %q, want quoted script token", got)
	}
}

func TestValidateSpecExtrasRejectsImagePullPolicy(t *testing.T) {
	c := []corev1.Container{{Name: "c", Image: "busybox", Command: []string{"sh"}, ImagePullPolicy: corev1.PullNever}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(c...)})); err == nil ||
		!strings.Contains(err.Error(), "permitted") {
		t.Errorf("init imagePullPolicy: got %v, want allowlist rejection", err)
	}
}

func TestValidateSpecExtrasInitContainerRequiresCommand(t *testing.T) {
	noCmd := []corev1.Container{{Name: "c", Image: "busybox"}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(noCmd...)})); err == nil ||
		!strings.Contains(err.Error(), "explicit command") {
		t.Errorf("init without command: got %v, want command rejection", err)
	}
	ports := []corev1.Container{{Name: "c", Image: "busybox", Command: []string{"sh"},
		Ports: []corev1.ContainerPort{{ContainerPort: 80}}}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(ports...)})); err == nil ||
		!strings.Contains(err.Error(), "permitted") {
		t.Errorf("init with ports: got %v, want allowlist rejection", err)
	}
}

func TestValidateSpecExtrasClusterAdminNamespaceExclusivity(t *testing.T) {
	// cluster-admin in another namespace is incoherent and rejected.
	err := validateSpecExtrasForTest(srWith(SudoRequestSpec{
		Namespace:  "seaweedfs",
		Privileges: SudoRequestPrivileges{ClusterAdmin: boolPtr(true)},
	}))
	if err == nil || !strings.Contains(err.Error(), "clusterAdmin") {
		t.Fatalf("cluster-admin off-namespace: got %v, want rejection", err)
	}

	// cluster-admin disabled in another namespace is fine.
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{
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
		{"explicit controller ns", srWith(SudoRequestSpec{Namespace: DefaultControllerNamespace}), true},
		{"disabled in controller ns", srWith(SudoRequestSpec{Privileges: SudoRequestPrivileges{ClusterAdmin: boolPtr(false)}}), false},
		{"other namespace defaults off", srWith(SudoRequestSpec{Namespace: "seaweedfs"}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterAdminEnabledForTest(tc.sr); got != tc.want {
				t.Errorf("clusterAdminEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExecutorServiceAccount(t *testing.T) {
	name, automount := executorServiceAccountForTest(srWith(SudoRequestSpec{}))
	if name != ExecutorSAName || automount != nil {
		t.Errorf("cluster-admin path: got (%q, %v), want (%q, nil)", name, automount, ExecutorSAName)
	}

	name, automount = executorServiceAccountForTest(srWith(SudoRequestSpec{Namespace: "seaweedfs"}))
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
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{Volumes: rawList(v...)})); err == nil {
		t.Error("reserved stdin volume name: got nil, want rejection")
	}
	// A mount reusing the reserved name or path is rejected.
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{VolumeMounts: rawList(corev1.VolumeMount{Name: stdinVolumeName, MountPath: "/x"})})); err == nil {
		t.Error("reserved stdin mount name: got nil, want rejection")
	}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{VolumeMounts: rawList(corev1.VolumeMount{Name: "ok", MountPath: stdinMountDir})})); err == nil {
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
		InitContainers: rawList(corev1.Container{
			Name:         "copy",
			Image:        "rclone/rclone",
			Command:      []string{"/bin/sh", "-c", "cp x /tools/y"},
			VolumeMounts: []corev1.VolumeMount{{Name: "tools", MountPath: "/tools"}},
		}),
	})
	v := newSpecExtrasViewForTest(sr, false)
	if len(v.InitContainers) != 1 {
		t.Fatalf("expected 1 init container view, got %d", len(v.InitContainers))
	}
	ic := v.InitContainers[0]
	// argv boundaries are preserved: the multi-word script stays one token.
	if ic.Command != "/bin/sh -c 'cp x /tools/y'" {
		t.Errorf("init container command not surfaced faithfully: %q", ic.Command)
	}
	if len(ic.Mounts) != 1 {
		t.Errorf("init container mounts not surfaced: %v", ic.Mounts)
	}
	// And the plain-text rendering (push + summarizer) includes the command.
	if txt := specExtrasTextForTest(sr, false); !strings.Contains(txt, "cp x /tools/y") {
		t.Errorf("specExtrasText omits init container command: %q", txt)
	}
}

func TestValidateSpecExtrasRejectsMalformedItem(t *testing.T) {
	// A type-confused field (integer env name) is stored raw by the apiserver
	// (preserve-unknown-fields); the controller must reject it per-request, not
	// fail to decode the whole object.
	sr := srWith(SudoRequestSpec{Env: []runtime.RawExtension{{Raw: []byte(`{"name":123}`)}}})
	err := validateSpecExtrasForTest(sr)
	if err == nil || !strings.Contains(err.Error(), "invalid pod field") {
		t.Fatalf("malformed env item: got %v, want decode rejection", err)
	}
}

func TestValidateSpecExtrasRejectsProjected(t *testing.T) {
	v := []corev1.Volume{{Name: "p", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{}}}}
	err := validateSpecExtrasForTest(srWith(SudoRequestSpec{Volumes: rawList(v...)}))
	if err == nil || !strings.Contains(err.Error(), "projected") {
		t.Fatalf("projected volume: got %v, want projected rejection", err)
	}
	if _, allowed := describeVolumeSource(v[0]); allowed {
		t.Error("describeVolumeSource marks projected allowed; must be false")
	}
}

func TestValidateSpecExtrasMountReferences(t *testing.T) {
	// Mount referencing an undefined volume is rejected.
	err := validateSpecExtrasForTest(srWith(SudoRequestSpec{
		VolumeMounts: rawList(corev1.VolumeMount{Name: "missing", MountPath: "/x"}),
	}))
	if err == nil || !strings.Contains(err.Error(), "no volume named") {
		t.Fatalf("dangling mount: got %v, want rejection", err)
	}

	// Duplicate mountPath within a container is rejected.
	err = validateSpecExtrasForTest(srWith(SudoRequestSpec{
		Volumes: rawList(
			corev1.Volume{Name: "a", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			corev1.Volume{Name: "b", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		),
		VolumeMounts: rawList(
			corev1.VolumeMount{Name: "a", MountPath: "/same"},
			corev1.VolumeMount{Name: "b", MountPath: "/same"},
		),
	}))
	if err == nil || !strings.Contains(err.Error(), "duplicate mountPath") {
		t.Fatalf("duplicate mountPath: got %v, want rejection", err)
	}

	// A mount referencing a defined volume (and the stdin volume) is accepted.
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{
		Stdin:        "data",
		Volumes:      rawList(corev1.Volume{Name: "a", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}),
		VolumeMounts: rawList(corev1.VolumeMount{Name: "a", MountPath: "/a"}),
	})); err != nil {
		t.Fatalf("valid mounts rejected: %v", err)
	}

	// An init container referencing an undefined volume is rejected.
	err = validateSpecExtrasForTest(srWith(SudoRequestSpec{
		InitContainers: rawList(corev1.Container{
			Name: "i", Image: "busybox", Command: []string{"sh"},
			VolumeMounts: []corev1.VolumeMount{{Name: "nope", MountPath: "/x"}},
		}),
	}))
	if err == nil || !strings.Contains(err.Error(), "no volume named") {
		t.Fatalf("init dangling mount: got %v, want rejection", err)
	}
}

func TestDescribeEnvRedaction(t *testing.T) {
	env := []corev1.EnvVar{
		{Name: "LITERAL", Value: "secret-value"},
		{Name: "REF", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "creds"}, Key: "k"}}},
	}
	if full := strings.Join(describeEnv(env, false), " | "); !strings.Contains(full, "LITERAL=secret-value") {
		t.Errorf("unredacted: literal value missing: %q", full)
	}
	red := strings.Join(describeEnv(env, true), " | ")
	if strings.Contains(red, "secret-value") || !strings.Contains(red, "LITERAL=<redacted>") {
		t.Errorf("redacted: literal value leaked: %q", red)
	}
	if !strings.Contains(red, "REF <- secret/creds:k") {
		t.Errorf("redacted: dropped secret ref: %q", red)
	}
}

func TestDescribeEnvFromShowsPrefix(t *testing.T) {
	got := describeEnvFrom([]corev1.EnvFromSource{
		{Prefix: "AWS_", SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "creds"}}},
	})
	if len(got) != 1 || !strings.Contains(got[0], "prefix=AWS_") {
		t.Errorf("envFrom prefix not surfaced: %v", got)
	}
}

func TestValidateSpecExtrasRejectsLifecycle(t *testing.T) {
	ic := corev1.Container{Name: "i", Image: "busybox", Command: []string{"sh"}, Lifecycle: &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"sh", "-c", "evil"}}}}}
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{InitContainers: rawList(ic)})); err == nil ||
		!strings.Contains(err.Error(), "permitted") {
		t.Fatalf("init lifecycle hook: got %v, want allowlist rejection", err)
	}
}

func TestDescribeEnvSurfacesValuesAndSources(t *testing.T) {
	sr := srWith(SudoRequestSpec{Env: rawList(
		corev1.EnvVar{Name: "LITERAL", Value: "KUBECONFIG=/x"},
		corev1.EnvVar{Name: "FROMSECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "creds"}, Key: "token"}}},
	)})
	v := newSpecExtrasViewForTest(sr, false)
	joined := strings.Join(v.Env, " | ")
	if !strings.Contains(joined, "LITERAL=KUBECONFIG=/x") {
		t.Errorf("literal env value not surfaced: %q", joined)
	}
	if !strings.Contains(joined, "FROMSECRET <- secret/creds:token") {
		t.Errorf("secret-ref env source not surfaced: %q", joined)
	}
}

func TestExecutorWaitStartAfterInitCompletion(t *testing.T) {
	created := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	initDone := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: created}}

	// All inits terminated -> reference is the latest init finish, giving the
	// executor its own start window (not the 20-min-old job creation).
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{
			{Name: "copy", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: initDone}}},
		},
	}}
	if got := executorWaitStart(pod, job); !got.Equal(initDone.Time) {
		t.Errorf("all-inits-done: ref = %v, want init finish %v", got, initDone.Time)
	}

	// No init containers -> reference is job creation.
	if got := executorWaitStart(&corev1.Pod{}, job); !got.Equal(created.Time) {
		t.Errorf("no-inits: ref = %v, want job creation %v", got, created.Time)
	}

	// An init still running/waiting -> fall back to job creation (window not open).
	podRunning := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{
			{Name: "copy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		},
	}}
	if got := executorWaitStart(podRunning, job); !got.Equal(created.Time) {
		t.Errorf("init-not-finished: ref = %v, want job creation %v", got, created.Time)
	}
}

func TestExecutorContainerTerminated(t *testing.T) {
	term := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	// Executor exited while an injected sidecar keeps running -> terminated=true
	// (so the request completes instead of hanging on the never-finishing Job).
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "executor", State: term},
		{Name: "istio-proxy", State: running},
	}}}
	if !executorContainerTerminated(pod) {
		t.Error("executor terminated + sidecar running: want terminated")
	}
	stillRunning := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "executor", State: running},
	}}}
	if executorContainerTerminated(stillRunning) {
		t.Error("executor running: want not terminated")
	}
}

func TestExecutorStarted(t *testing.T) {
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	term := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}

	// Executor running/terminated -> started.
	if !executorStarted(&corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "executor", State: running}}}}) {
		t.Error("running executor: want started")
	}
	// A merely-terminated init with the executor still waiting is NOT started.
	if executorStarted(&corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{Name: "copy", State: term}},
		ContainerStatuses:     []corev1.ContainerStatus{{Name: "executor", State: waiting}},
	}}) {
		t.Error("terminated-init + waiting-executor: want NOT started")
	}
	// A still-running init does NOT count as the executor having started — so a
	// never-exiting init can't defeat the deadline.
	if executorStarted(&corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{Name: "copy", State: running}},
		ContainerStatuses:     []corev1.ContainerStatus{{Name: "executor", State: waiting}},
	}}) {
		t.Error("running init + waiting executor: want NOT started")
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

func TestInitContainerGetsSchedulingRequestsWithoutLimits(t *testing.T) {
	// Requester init containers get the same modest scheduler hints as the executor.
	sr := srWith(SudoRequestSpec{InitContainers: rawList(corev1.Container{Name: "i", Image: "busybox", Command: []string{"sh"}})})
	extras, err := decodePodExtras(sr)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	job := buildExecutorJobForTest(sr, DefaultControllerNamespace, "sudo-exec-test", extras)
	got := job.Spec.Template.Spec.InitContainers[0].Resources
	if got.Requests.Memory().IsZero() || got.Requests.Cpu().IsZero() {
		t.Errorf("init container scheduling requests not stamped: %+v", got)
	}
	if len(got.Limits) != 0 {
		t.Errorf("init container has unexpected hard resource limits: %+v", got.Limits)
	}
}

func TestEmptyDirHasNoDefaultLimitAndPreservesExplicitLimit(t *testing.T) {
	custom := resource.MustParse("5Gi")
	sr := srWith(SudoRequestSpec{Volumes: rawList(
		corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: "big", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &custom}}},
	)})
	extras, err := decodePodExtras(sr)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	job := buildExecutorJobForTest(sr, DefaultControllerNamespace, "sudo-exec-test", extras)
	vols := job.Spec.Template.Spec.Volumes
	if vols[0].EmptyDir.SizeLimit != nil {
		t.Errorf("emptyDir received unexpected default limit: %v", vols[0].EmptyDir.SizeLimit)
	}
	if vols[1].EmptyDir.SizeLimit == nil || !vols[1].EmptyDir.SizeLimit.Equal(custom) {
		t.Errorf("requester sizeLimit not preserved: %v", vols[1].EmptyDir.SizeLimit)
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
	if !hasSpecExtras(srWith(SudoRequestSpec{ImagePullSecrets: rawList(corev1.LocalObjectReference{Name: "reg"})})) {
		t.Error("imagePullSecrets not detected as extra")
	}
}

func TestImagePullSecretsSplicedAndReviewed(t *testing.T) {
	sr := srWith(SudoRequestSpec{
		Command:          "kubectl get nodes",
		Image:            "registry.internal/private:1.0",
		ImagePullSecrets: rawList(corev1.LocalObjectReference{Name: "registry-creds"}),
	})

	extras, err := decodePodExtras(sr)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	job := buildExecutorJobForTest(sr, DefaultControllerNamespace, "sudo-exec-test", extras)
	ips := job.Spec.Template.Spec.ImagePullSecrets
	if len(ips) != 1 || ips[0].Name != "registry-creds" {
		t.Fatalf("imagePullSecrets not spliced onto pod: %+v", ips)
	}

	// Surfaced to the reviewer in both the structured view and the plain text.
	v := newSpecExtrasViewForTest(sr, false)
	if len(v.ImagePullSecrets) != 1 || v.ImagePullSecrets[0] != "registry-creds" {
		t.Errorf("imagePullSecrets not in review view: %v", v.ImagePullSecrets)
	}
	if txt := specExtrasTextForTest(sr, false); !strings.Contains(txt, "imagePullSecrets: registry-creds") {
		t.Errorf("specExtrasText omits imagePullSecrets: %q", txt)
	}

	// And present in the ground-truth pod spec the reviewer sees.
	tmpl, err := displayPodTemplateForTest(sr, false)
	if err != nil {
		t.Fatalf("displayPodTemplate: %v", err)
	}
	if !strings.Contains(tmpl, "registry-creds") {
		t.Errorf("pod template missing imagePullSecrets:\n%s", tmpl)
	}
}

func TestValidateSpecExtrasImagePullSecrets(t *testing.T) {
	// A well-formed reference is accepted.
	if err := validateSpecExtrasForTest(srWith(SudoRequestSpec{
		ImagePullSecrets: rawList(corev1.LocalObjectReference{Name: "registry-creds"}),
	})); err != nil {
		t.Fatalf("valid imagePullSecret rejected: %v", err)
	}

	// A nameless reference is rejected before the human round-trip.
	err := validateSpecExtrasForTest(srWith(SudoRequestSpec{
		ImagePullSecrets: rawList(corev1.LocalObjectReference{}),
	}))
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("nameless imagePullSecret: got %v, want name rejection", err)
	}

	// A type-confused item (numeric name) is rejected at decode, per-request.
	sr := srWith(SudoRequestSpec{ImagePullSecrets: []runtime.RawExtension{{Raw: []byte(`{"name":123}`)}}})
	if err := validateSpecExtrasForTest(sr); err == nil || !strings.Contains(err.Error(), "invalid pod field") {
		t.Fatalf("malformed imagePullSecret: got %v, want decode rejection", err)
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

func TestRejectServiceAccountTokenSecrets(t *testing.T) {
	ctx := context.Background()
	tokenSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sa-token", Namespace: "team-a"},
		Type:       corev1.SecretTypeServiceAccountToken,
	}
	opaqueSec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "team-a"},
		Type:       corev1.SecretTypeOpaque,
	}
	cl := fake.NewClientBuilder().WithObjects(tokenSec, opaqueSec).Build()
	r := &SudoRequestReconciler{APIReader: cl}

	secVol := func(name string) *podExtras {
		return &podExtras{Volumes: []corev1.Volume{{
			Name:         "v",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name}},
		}}}
	}

	// Opaque secret mounted as a volume: allowed.
	if err := r.rejectServiceAccountTokenSecrets(ctx, "team-a", secVol("creds")); err != nil {
		t.Errorf("opaque secret volume: unexpected error %v", err)
	}

	// SA-token secret via volume: rejected (permanently).
	if err := r.rejectServiceAccountTokenSecrets(ctx, "team-a", secVol("sa-token")); !errors.Is(err, errDisallowedSecret) {
		t.Errorf("sa-token volume: want errDisallowedSecret, got %v", err)
	}

	// SA-token via envFrom: rejected.
	badEnvFrom := &podExtras{EnvFrom: []corev1.EnvFromSource{{
		SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "sa-token"}},
	}}}
	if err := r.rejectServiceAccountTokenSecrets(ctx, "team-a", badEnvFrom); !errors.Is(err, errDisallowedSecret) {
		t.Errorf("sa-token envFrom: want errDisallowedSecret, got %v", err)
	}

	// SA-token via an init container's env secretKeyRef: rejected.
	badInit := &podExtras{InitContainers: []corev1.Container{{Env: []corev1.EnvVar{{
		Name: "T",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "sa-token"}, Key: "token",
		}},
	}}}}}
	if err := r.rejectServiceAccountTokenSecrets(ctx, "team-a", badInit); !errors.Is(err, errDisallowedSecret) {
		t.Errorf("sa-token init env: want errDisallowedSecret, got %v", err)
	}

	// Missing reference: rejected, so a requester can't reference an absent name and
	// create an SA-token Secret there before the kubelet mounts it.
	if err := r.rejectServiceAccountTokenSecrets(ctx, "team-a", secVol("missing")); !errors.Is(err, errDisallowedSecret) {
		t.Errorf("missing secret: want errDisallowedSecret, got %v", err)
	}
}

func TestValidateSpecExtrasStdinSizeCap(t *testing.T) {
	ok := srWith(SudoRequestSpec{Stdin: strings.Repeat("x", MaxStdinBytes)})
	if err := validateSpecExtrasForTest(ok); err != nil {
		t.Errorf("stdin at limit rejected: %v", err)
	}
	tooBig := srWith(SudoRequestSpec{Stdin: strings.Repeat("x", MaxStdinBytes+1)})
	if err := validateSpecExtrasForTest(tooBig); err == nil || !strings.Contains(err.Error(), "stdin is") {
		t.Errorf("oversized stdin: got %v, want size rejection", err)
	}
}
