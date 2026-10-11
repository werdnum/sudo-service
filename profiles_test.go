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

func TestPreflightBase64Warnings(t *testing.T) {
	// An embedded script: base64 of a short Python program.
	blob := "aW1wb3J0IGpzb24sIHN5cwpmb3IgcCBpbiBzeXMuYXJndlsxOl06CiAgICBwcmludChwLCBqc29uLmR1bXBzKHt9KSkK"
	for _, command := range []string{
		"echo ZWNobyBoaQ== | base64 -d | sh",
		"echo ZWNobyBoaQ== | base64 --decode | /bin/bash -s",
		"printf %s ZWNobyBoaQ== | base64 -d | python3 -",
		`eval "$(echo ZWNobyBoaQ== | base64 -d)"`,
		`sh -c "$(echo ZWNobyBoaQ== | base64 -d)"`,
		// Decode to a file, then run it with a non-shell interpreter.
		"set -e; kubectl get secret creds -n backup -o jsonpath='{.data.key}' | base64 -d > /tmp/k.json; echo " + blob + " | base64 -d > /tmp/g.py; /tmp/v/bin/python /tmp/g.py a/ b/",
		"base64 -d <<< '" + blob + "' > /tmp/x; sh /tmp/x",
		`python3 -c "import base64; exec(base64.b64decode('` + blob + `'))"`,
		"runner_b64='" + blob + "'; printf %s \"$runner_b64\" | base64 -d > /tmp/r.sh; sh /tmp/r.sh",
		"cat > /tmp/s.b64 <<'B64'\n" + blob + "\nB64\nbase64 -d /tmp/s.b64 > /tmp/s.py; python3 /tmp/s.py",
	} {
		warnings, err := preflightCommand(command, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(warnings, "\n"); !strings.Contains(got, "base64") {
			t.Errorf("preflightCommand(%q) warnings = %q, want a base64 warning", command, got)
		}
	}

	// Decoding a Secret fetched at runtime is the common case and hides nothing.
	for _, command := range []string{
		"kubectl get secret foo -n bar -o jsonpath={.data.x} | base64 -d",
		`T=$(kubectl -n ansible get secret semaphore -o jsonpath={.data.token} | base64 -d); curl -H "Authorization: Bearer $T" http://semaphore/api/projects`,
		`bash -c 'set -e; S=$(kubectl get secret kc -n default -o jsonpath="{.data.client-secret}" | base64 -d); curl -d "secret=$S" http://kc/token'`,
		"set -eu\nkubectl get secret gcs-creds -n backup -o jsonpath='{.data.backup-creds\\.json}' | base64 -d > /tmp/key.json\ngcloud auth activate-service-account --key-file=/tmp/key.json --quiet\nrm /tmp/key.json\nfor p in a b; do\n  gcloud storage objects list \"gs://bucket/$p/**\" | awk -F, '{ print $1 }' | sort || echo failed\ndone",
		"kubectl get secret foo -o jsonpath={.data.x} | base64 -d > /tmp/k; kubectl set image deploy/x x=img@sha256:0eea2d071c4e0f6a8a1c3b5d7e9f0a2b4c6d8e0f1a3b5c7d9e1f3a5b7c9d1e3f",
		`kubectl get secret k -o jsonpath={.data.key} | base64 -d > /tmp/k; echo "host ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINFeW1NJGKH0bfvjGUIjQOoFiBvXyMm+abc" > /tmp/kh; ssh -i /tmp/k -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/tmp/kh host true`,
	} {
		warnings, err := preflightCommand(command, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(warnings, "\n"); strings.Contains(got, "base64") {
			t.Errorf("preflightCommand(%q) warnings = %q, want no base64 warning", command, got)
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

func TestPreflightDoesNotExecutePipefailTextInHeredoc(t *testing.T) {
	profile := executorProfiles["kubectl"]
	command := "kubectl apply -f - <<'EOF'\napiVersion: v1\ndata:\n  script: |\n    set -o pipefail\nEOF\n"
	if _, err := preflightCommand(command, &profile); err != nil {
		t.Fatalf("heredoc payload was treated as an executed shell command: %v", err)
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
