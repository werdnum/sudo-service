package main

import (
	"strings"
	"testing"
)

func TestResolveExecutorProfile(t *testing.T) {
	profile, image, err := resolveExecutorProfile(srWith(SudoRequestSpec{Command: "kubectl get nodes"}))
	if err != nil {
		t.Fatal(err)
	}
	if profile == nil || profile.Name != DefaultExecutorProfile {
		t.Fatalf("default profile = %#v, want %q", profile, DefaultExecutorProfile)
	}
	if image != DefaultExecutorImage || !strings.Contains(image, "@sha256:") {
		t.Fatalf("default image = %q, want digest-pinned %q", image, DefaultExecutorImage)
	}

	profile, image, err = resolveExecutorProfile(srWith(SudoRequestSpec{Profile: "network-tools"}))
	if err != nil || profile == nil || profile.Name != "network-tools" || !strings.Contains(image, "@sha256:") {
		t.Fatalf("network-tools resolution = (%#v, %q, %v)", profile, image, err)
	}

	profile, image, err = resolveExecutorProfile(srWith(SudoRequestSpec{Image: "example.invalid/custom:v1"}))
	if err != nil || profile != nil || image != "example.invalid/custom:v1" {
		t.Fatalf("raw image resolution = (%#v, %q, %v)", profile, image, err)
	}
}

func TestResolveExecutorProfileRejectsSpoofableOrUnknownSelection(t *testing.T) {
	for _, spec := range []SudoRequestSpec{
		{Profile: "kubectl", Image: "attacker.invalid/lookalike:latest"},
		{Profile: "does-not-exist"},
	} {
		if _, _, err := resolveExecutorProfile(srWith(spec)); err == nil {
			t.Fatalf("resolveExecutorProfile(%#v) succeeded, want error", spec)
		}
	}
}

func TestProfileCatalogIsMachineReadableAndPinned(t *testing.T) {
	profiles := listExecutorProfiles()
	if len(profiles) < 2 {
		t.Fatalf("got %d profiles, want at least two", len(profiles))
	}
	for _, profile := range profiles {
		if profile.Name == "" || profile.Shell.Path == "" || profile.Shell.Dialect == "" {
			t.Errorf("profile has incomplete identity/shell metadata: %#v", profile)
		}
		if !strings.Contains(profile.Image, "@sha256:") {
			t.Errorf("profile %q image is not digest-pinned: %q", profile.Name, profile.Image)
		}
		if len(profile.Executables) == 0 || len(profile.Capabilities) == 0 {
			t.Errorf("profile %q lacks executable/capability metadata", profile.Name)
		}
	}
}

func TestPreflightRejectsKnownMissingVisibleExecutable(t *testing.T) {
	profile := executorProfiles["kubectl"]
	_, err := preflightCommand("printf ok; /usr/bin/ssh host.example", &profile)
	if err == nil || !strings.Contains(err.Error(), `known not to include directly invoked executable "ssh"`) {
		t.Fatalf("preflight error = %v", err)
	}

	// Dynamic execution cannot be inferred safely; do not claim it is absent.
	if _, err := preflightCommand(`tool=ssh; "$tool" host.example`, &profile); err != nil {
		t.Fatalf("dynamic command was treated as statically certain: %v", err)
	}
}

func TestPreflightWarningsAreConservative(t *testing.T) {
	command := "apk add openssh; echo ZWNobyBoaQ== | base64 -d | sh; sleep 5m"
	warnings, err := preflightCommand(command, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(warnings, "\n")
	for _, want := range []string{"read-only root filesystem", "opaque base64", "advisory heuristic"} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings %q do not contain %q", got, want)
		}
	}
}

func TestPreflightWarnsUnknownRawImagePipefail(t *testing.T) {
	warnings, err := preflightCommand("set -o pipefail; do-something | other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(warnings, "\n"); !strings.Contains(got, "cannot verify") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestPreflightRejectsPipefailWhenProfileDoesNotGuaranteeIt(t *testing.T) {
	profile := executorProfiles["kubectl"]
	_, err := preflightCommand("set -o pipefail; kubectl get pods | jq .", &profile)
	if err == nil || !strings.Contains(err.Error(), "does not support 'set -o pipefail'") {
		t.Fatalf("preflight error = %v", err)
	}
}

func TestImageForUsesRecordedResolution(t *testing.T) {
	sr := srWith(SudoRequestSpec{Profile: "kubectl"})
	sr.Status.ResolvedImage = "registry.example/executor@sha256:reviewed"
	if got := imageFor(sr); got != sr.Status.ResolvedImage {
		t.Fatalf("imageFor = %q, want recorded resolution %q", got, sr.Status.ResolvedImage)
	}
}
