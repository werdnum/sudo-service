package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// buildJobFor decodes a request's extras and renders the executor Job, failing
// the test on a decode error.
func buildJobFor(t *testing.T, sr *SudoRequest) corev1.PodSpec {
	t.Helper()
	extras, err := decodePodExtras(sr)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return buildExecutorJob(sr, ControllerNamespace, "sudo-exec-test", extras).Spec.Template.Spec
}

func mountPath(mounts []corev1.VolumeMount, path string) (corev1.VolumeMount, bool) {
	for _, m := range mounts {
		if m.MountPath == path {
			return m, true
		}
	}
	return corev1.VolumeMount{}, false
}

func volumeByName(vols []corev1.Volume, name string) (corev1.Volume, bool) {
	for _, v := range vols {
		if v.Name == name {
			return v, true
		}
	}
	return corev1.Volume{}, false
}

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// A plain request gets writable /tmp and HOME scratch by default, so a command
// that writes a temp file or a dotfile cache doesn't hit the read-only root FS.
func TestScratchDefaultsTmpAndHome(t *testing.T) {
	pod := buildJobFor(t, srWith(SudoRequestSpec{Command: "true"}))
	c := pod.Containers[0]

	if _, ok := mountPath(c.VolumeMounts, tmpMountDir); !ok {
		t.Errorf("/tmp scratch mount missing: %+v", c.VolumeMounts)
	}
	if _, ok := mountPath(c.VolumeMounts, homeMountDir); !ok {
		t.Errorf("HOME scratch mount missing: %+v", c.VolumeMounts)
	}
	if home, _ := envValue(c.Env, "HOME"); home != homeMountDir {
		t.Errorf("HOME env = %q, want %q", home, homeMountDir)
	}

	// Both backing volumes exist as emptyDirs without sudo-service-specific size
	// ceilings; cluster scheduling/quota/eviction policy governs aggregate use.
	for _, name := range []string{tmpVolumeName, homeVolumeName} {
		v, ok := volumeByName(pod.Volumes, name)
		if !ok {
			t.Fatalf("scratch volume %q missing: %+v", name, pod.Volumes)
		}
		if v.EmptyDir == nil || v.EmptyDir.SizeLimit != nil {
			t.Errorf("scratch volume %q is not an unbounded emptyDir: %+v", name, v)
		}
	}
}

// Scratch is mounted into init containers too — they run in the same read-only
// pod and routinely stage tools into a writable dir.
func TestScratchAppliesToInitContainers(t *testing.T) {
	pod := buildJobFor(t, srWith(SudoRequestSpec{
		InitContainers: rawList(corev1.Container{Name: "i", Image: "busybox", Command: []string{"sh"}}),
	}))
	ic := pod.InitContainers[0]
	if _, ok := mountPath(ic.VolumeMounts, tmpMountDir); !ok {
		t.Errorf("init container missing /tmp scratch: %+v", ic.VolumeMounts)
	}
	if home, _ := envValue(ic.Env, "HOME"); home != homeMountDir {
		t.Errorf("init container HOME env = %q, want %q", home, homeMountDir)
	}
}

// A requester who mounts their own volume at /tmp opts out of the tmp default
// (no duplicate mountPath the apiserver would reject), but still gets HOME.
func TestScratchTmpOptOut(t *testing.T) {
	pod := buildJobFor(t, srWith(SudoRequestSpec{
		Volumes:      rawList(corev1.Volume{Name: "mytmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}),
		VolumeMounts: rawList(corev1.VolumeMount{Name: "mytmp", MountPath: tmpMountDir}),
	}))
	c := pod.Containers[0]
	m, ok := mountPath(c.VolumeMounts, tmpMountDir)
	if !ok || m.Name != "mytmp" {
		t.Errorf("/tmp should keep the requester's mount, got %+v", m)
	}
	if _, ok := volumeByName(pod.Volumes, tmpVolumeName); ok {
		t.Error("controller tmp scratch volume added despite requester /tmp mount")
	}
	// HOME default still applies.
	if _, ok := volumeByName(pod.Volumes, homeVolumeName); !ok {
		t.Error("HOME scratch should still be added")
	}
}

// A requester who sets HOME themselves owns their home dir; the controller must
// not override the value or add its home scratch.
func TestScratchHomeEnvOptOut(t *testing.T) {
	pod := buildJobFor(t, srWith(SudoRequestSpec{
		Env: rawList(corev1.EnvVar{Name: "HOME", Value: "/custom"}),
	}))
	c := pod.Containers[0]
	if home, _ := envValue(c.Env, "HOME"); home != "/custom" {
		t.Errorf("requester HOME overridden: got %q", home)
	}
	if _, ok := mountPath(c.VolumeMounts, homeMountDir); ok {
		t.Error("home scratch mounted despite requester-set HOME")
	}
	if _, ok := volumeByName(pod.Volumes, homeVolumeName); ok {
		t.Error("home scratch volume added despite requester-set HOME")
	}
	// /tmp default is independent and still applies.
	if _, ok := mountPath(c.VolumeMounts, tmpMountDir); !ok {
		t.Error("/tmp scratch should still be added")
	}
}

// A requester mount nested under /tmp (e.g. a Secret at /tmp/creds) suppresses
// the controller /tmp default — nesting an emptyDir parent under a requester
// child mount is what Kubernetes handles unreliably.
func TestScratchTmpNestedMountOptOut(t *testing.T) {
	pod := buildJobFor(t, srWith(SudoRequestSpec{
		Volumes:      rawList(corev1.Volume{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}}),
		VolumeMounts: rawList(corev1.VolumeMount{Name: "creds", MountPath: "/tmp/creds"}),
	}))
	if _, ok := volumeByName(pod.Volumes, tmpVolumeName); ok {
		t.Error("controller tmp scratch added despite requester mount under /tmp")
	}
	if _, ok := mountPath(pod.Containers[0].VolumeMounts, tmpMountDir); ok {
		t.Error("controller /tmp mount added despite requester mount under /tmp")
	}
}

// A sibling path like /tmpfoo must NOT be mistaken for being under /tmp.
func TestScratchTmpSiblingPathNotConfused(t *testing.T) {
	pod := buildJobFor(t, srWith(SudoRequestSpec{
		Volumes:      rawList(corev1.Volume{Name: "x", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}),
		VolumeMounts: rawList(corev1.VolumeMount{Name: "x", MountPath: "/tmpfoo"}),
	}))
	if _, ok := volumeByName(pod.Volumes, tmpVolumeName); !ok {
		t.Error("/tmp default wrongly suppressed by sibling path /tmpfoo")
	}
}

// A requester who mounts their own writable volume exactly at the home path but
// doesn't set HOME should still get HOME pointed at it (so dotfiles land in the
// writable mount, not the image's read-only home) — without a second volume.
func TestScratchHomeMountSetsHomeWithoutVolume(t *testing.T) {
	pod := buildJobFor(t, srWith(SudoRequestSpec{
		Volumes:      rawList(corev1.Volume{Name: "myhome", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}),
		VolumeMounts: rawList(corev1.VolumeMount{Name: "myhome", MountPath: homeMountDir}),
	}))
	c := pod.Containers[0]
	if home, _ := envValue(c.Env, "HOME"); home != homeMountDir {
		t.Errorf("HOME not pointed at requester home mount: got %q", home)
	}
	if _, ok := volumeByName(pod.Volumes, homeVolumeName); ok {
		t.Error("controller home scratch volume added despite requester home mount")
	}
	m, _ := mountPath(c.VolumeMounts, homeMountDir)
	if m.Name != "myhome" {
		t.Errorf("home mount should be the requester's, got %q", m.Name)
	}
}

// envFrom could carry HOME (which a literal HOME env would override, since env
// beats envFrom), so the controller leaves HOME alone — but /tmp is unaffected.
func TestScratchEnvFromSuppressesHome(t *testing.T) {
	pod := buildJobFor(t, srWith(SudoRequestSpec{
		EnvFrom: rawList(corev1.EnvFromSource{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}}),
	}))
	c := pod.Containers[0]
	if _, ok := envValue(c.Env, "HOME"); ok {
		t.Error("controller set HOME despite requester envFrom (which may carry HOME)")
	}
	if _, ok := volumeByName(pod.Volumes, homeVolumeName); ok {
		t.Error("controller home scratch volume added despite requester envFrom")
	}
	// /tmp is independent of HOME handling and still applies.
	if _, ok := volumeByName(pod.Volumes, tmpVolumeName); !ok {
		t.Error("/tmp scratch should still be added when only envFrom is present")
	}
}

// The scratch volume names are reserved, like the stdin payload: a requester
// volume reusing one is rejected up front rather than producing a duplicate the
// apiserver rejects after approval.
func TestScratchVolumeNamesReserved(t *testing.T) {
	for _, name := range []string{tmpVolumeName, homeVolumeName} {
		err := validateSpecExtras(srWith(SudoRequestSpec{
			Volumes: rawList(corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}),
		}))
		if err == nil {
			t.Errorf("reserved volume name %q: got nil, want rejection", name)
		}
	}
}
