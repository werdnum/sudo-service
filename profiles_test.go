package main

import (
	"bytes"
	"html/template"
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

	// The catalog describes the base image, not tools staged into arbitrary
	// approved mounts by an init container.
	if _, err := preflightCommand(`/tools/ssh host.example`, &profile); err != nil {
		t.Fatalf("staged executable was compared with the base-image catalog: %v", err)
	}
}

func TestPreflightInspectsBuiltinExecutionTargets(t *testing.T) {
	profile := executorProfiles["kubectl"]
	for _, command := range []string{`exec ssh "$host"`, "command ssh host.example", "command -p ssh host.example"} {
		if _, err := preflightCommand(command, &profile); err == nil || !strings.Contains(err.Error(), `executable "ssh"`) {
			t.Errorf("preflightCommand(%q) error = %v, want missing ssh", command, err)
		}
	}
	if _, err := preflightCommand("command -v ssh", &profile); err != nil {
		t.Fatalf("presence query was treated as execution: %v", err)
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
	for _, command := range []string{
		"set -o pipefail; kubectl get pods | jq .",
		"set -e -o pipefail; kubectl get pods | jq .",
		"set -o errexit -o pipefail; kubectl get pods | jq .",
		"set -eo pipefail; kubectl get pods | jq .",
	} {
		_, err := preflightCommand(command, &profile)
		if err == nil || !strings.Contains(err.Error(), "does not support 'set -o pipefail'") {
			t.Errorf("preflightCommand(%q) error = %v", command, err)
		}
	}
}

func TestPreflightWarnsForBashTestAfterShellKeyword(t *testing.T) {
	profile := executorProfiles["kubectl"]
	for _, command := range []string{
		"if [[ -f /tmp/x ]]; then echo yes; fi",
		"if false; then :; else [[ -f /tmp/x ]]; fi",
		"while true; do [[ -f /tmp/x ]]; break; done",
		"! [[ -f /tmp/x ]]",
	} {
		warnings, err := preflightCommand(command, &profile)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(warnings, "\n"); !strings.Contains(got, "Bash-specific syntax") {
			t.Errorf("preflightCommand(%q) warnings = %q, want Bash-specific syntax warning", command, got)
		}
	}
}

func TestImageForUsesRecordedResolution(t *testing.T) {
	sr := srWith(SudoRequestSpec{Profile: "kubectl"})
	sr.Status.ResolvedImage = "registry.example/executor@sha256:reviewed"
	if got := imageFor(sr); got != sr.Status.ResolvedImage {
		t.Fatalf("imageFor = %q, want recorded resolution %q", got, sr.Status.ResolvedImage)
	}
}

func TestApproveTemplateDistinguishesRawAndResolvedImages(t *testing.T) {
	tmpl := template.Must(template.New("root").Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("templates/approve.html"))

	for _, tc := range []struct {
		name    string
		profile string
		want    string
		notWant string
	}{
		{name: "raw", want: "Requested image (unprofiled)", notWant: "Resolved image"},
		{name: "profile", profile: "kubectl", want: "Resolved image", notWant: "Requested image (unprofiled)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rendered bytes.Buffer
			if err := tmpl.ExecuteTemplate(&rendered, "approve.html", approveView{Profile: tc.profile, Image: "example.invalid/executor:latest"}); err != nil {
				t.Fatal(err)
			}
			if got := rendered.String(); !strings.Contains(got, tc.want) || strings.Contains(got, tc.notWant) {
				t.Fatalf("rendered template does not distinguish image provenance: want %q and not %q", tc.want, tc.notWant)
			}
		})
	}
}
