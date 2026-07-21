package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

const DefaultExecutorProfile = "kubectl"

// ExecutorShell describes the shell that sudo-service invokes as /bin/sh in a
// curated executor image. Features are explicit because /bin/sh is not a
// portable promise of one implementation or feature set.
type ExecutorShell struct {
	Path             string `json:"path"`
	Dialect          string `json:"dialect"`
	SupportsPipefail bool   `json:"supportsPipefail"`
	SupportsBash     bool   `json:"supportsBash"`
}

// ExecutorProfile is controller-owned metadata for a reproducible executor.
// Executables is intentionally a positive, non-exhaustive inventory;
// AbsentExecutables is the smaller set the catalog has explicitly verified is
// absent. Preflight only rejects from the latter, avoiding false certainty.
type ExecutorProfile struct {
	Name              string        `json:"name"`
	Description       string        `json:"description"`
	Image             string        `json:"image"`
	Shell             ExecutorShell `json:"shell"`
	Executables       []string      `json:"executables"`
	AbsentExecutables []string      `json:"absentExecutables,omitempty"`
	Capabilities      []string      `json:"capabilities"`
}

var executorProfiles = map[string]ExecutorProfile{
	"kubectl": {
		Name:        "kubectl",
		Description: "Kubernetes administration with kubectl, Helm, and common structured-data tools",
		Image:       "alpine/k8s:1.35.5@sha256:d870622d004031c4e3ddad80d200692792509176db03c346a8526f4f45476e96",
		Shell: ExecutorShell{
			Path:    "/bin/sh",
			Dialect: "busybox-ash",
			// Conservative across every architecture in the manifest list and
			// consistent with the audited default-executor footgun. Do not relax
			// without executing the exact pinned /bin/sh on every platform.
			SupportsPipefail: false,
		},
		Executables:       []string{"bash", "curl", "git", "helm", "jq", "kubectl", "sh", "yq"},
		AbsentExecutables: []string{"ansible", "ansible-playbook", "cf-terraforming", "ssh", "ssh-keygen"},
		Capabilities:      []string{"kubernetes", "helm", "http", "git", "json", "yaml"},
	},
	"network-tools": {
		Name:        "network-tools",
		Description: "Network diagnosis with DNS, HTTP, packet, and socket tooling",
		Image:       "nicolaka/netshoot:v0.14@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2",
		Shell: ExecutorShell{
			Path:             "/bin/sh",
			Dialect:          "busybox-ash",
			SupportsPipefail: false,
		},
		Executables:       []string{"bash", "curl", "dig", "jq", "nc", "nmap", "sh", "ssh", "ssh-keygen", "tcpdump", "yq"},
		AbsentExecutables: []string{"ansible", "ansible-playbook", "cf-terraforming", "helm", "kubectl"},
		// Do not advertise packet capture/raw sockets merely because tcpdump/nmap
		// are installed: sudo-service drops every Linux capability at runtime.
		Capabilities: []string{"dns", "http", "sockets", "ssh", "json", "yaml"},
	},
}

// DefaultExecutorImage remains the compatibility name used by auto-approval
// rules. It is now reproducible: the default profile resolves to this digest.
var DefaultExecutorImage = executorProfiles[DefaultExecutorProfile].Image

func listExecutorProfiles() []ExecutorProfile {
	profiles := make([]ExecutorProfile, 0, len(executorProfiles))
	for _, profile := range executorProfiles {
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles
}

func resolveExecutorProfile(sr *SudoRequest) (*ExecutorProfile, string, error) {
	if sr.Spec.Profile != "" && sr.Spec.Image != "" {
		return nil, "", fmt.Errorf("profile and image are mutually exclusive; use a profile alias or an arbitrary raw image")
	}
	if sr.Spec.Image != "" {
		return nil, sr.Spec.Image, nil
	}
	name := sr.Spec.Profile
	if name == "" {
		name = DefaultExecutorProfile
	}
	profile, ok := executorProfiles[name]
	if !ok {
		names := make([]string, 0, len(executorProfiles))
		for name := range executorProfiles {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, "", fmt.Errorf("unknown executor profile %q (available: %s)", name, strings.Join(names, ", "))
	}
	return &profile, profile.Image, nil
}

// resolveAndPreflight validates the server-owned profile selection and returns
// conservative static diagnostics. An error means the command is known not to
// run as submitted; warnings describe likely footguns without claiming proof.
func resolveAndPreflight(sr *SudoRequest) (*ExecutorProfile, string, []string, error) {
	profile, image, err := resolveExecutorProfile(sr)
	if err != nil {
		return nil, "", nil, err
	}
	warnings, err := preflightCommand(sr.Spec.Command, profile)
	return profile, image, warnings, err
}

var (
	packageInstallRE = regexp.MustCompile(`(?m)(^|[;&|]\s*)(sudo\s+)?(apk|apt|apt-get|dnf|yum|microdnf|pip|pip3|npm)\s+(add|install)\b`)
	base64ScriptRE   = regexp.MustCompile(`(?is)base64\s+(-d|--decode)\b.{0,160}\|\s*(/bin/)?(ba)?sh\b|\|\s*base64\s+(-d|--decode)\b`)
	heredocRE        = regexp.MustCompile(`(?m)<<-?\s*['"]?[A-Za-z_][A-Za-z0-9_]*['"]?\s*$`)
	pipefailRE       = regexp.MustCompile(`(?m)(^|[;&|]\s*)set(?:\s+[^;&|\s]+)*\s+-[a-zA-Z]*[oO]\s+pipefail\b`)
	bashismRE        = regexp.MustCompile(`(^|[;&|()]\s*|\b(if|then|elif|while|until)\s+)\[\[|<\(|>\(|\bsource\s+`)
	sleepRE          = regexp.MustCompile(`(?:^|[;&|]\s*)sleep\s+([0-9]+)([smhd]?)\b`)
)

func preflightCommand(command string, profile *ExecutorProfile) ([]string, error) {
	var warnings []string
	if profile != nil {
		for _, executable := range visibleExecutables(command) {
			if containsString(profile.AbsentExecutables, executable) {
				return nil, fmt.Errorf("executor profile %q is known not to include directly invoked executable %q; choose another profile or an explicit image", profile.Name, executable)
			}
		}
		if commandUsesPipefail(command) && !profile.Shell.SupportsPipefail {
			return nil, fmt.Errorf("executor profile %q uses %s, whose catalog metadata does not support 'set -o pipefail'", profile.Name, profile.Shell.Dialect)
		}
		if commandUsesBashism(command) && !profile.Shell.SupportsBash {
			warnings = append(warnings, fmt.Sprintf("command contains Bash-specific syntax but profile %q executes it with %s at %s", profile.Name, profile.Shell.Dialect, profile.Shell.Path))
		}
	} else if commandUsesPipefail(command) {
		warnings = append(warnings, "raw image selected: sudo-service cannot verify whether its /bin/sh supports 'set -o pipefail'")
	}
	if packageInstallRE.MatchString(command) {
		warnings = append(warnings, "command appears to install packages at runtime; executors run non-root with a read-only root filesystem")
	}
	if base64ScriptRE.MatchString(command) {
		warnings = append(warnings, "command appears to decode an opaque base64 payload into a shell; reviewers cannot inspect the decoded program directly")
	}
	if len(command) > 4096 && heredocRE.MatchString(command) {
		warnings = append(warnings, "command contains a large heredoc; pass the payload with stdin/request-file to reduce quoting and review errors")
	}
	if likelyLongRuntime(command) {
		warnings = append(warnings, "command may be long-running; this is an advisory heuristic, and the one-shot executor has bounded resources and approval TTL")
	}
	return warnings, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

var shellBuiltins = map[string]bool{
	".": true, ":": true, "alias": true, "bg": true, "break": true,
	"cd": true, "command": true, "continue": true, "echo": true, "eval": true,
	"exec": true, "exit": true, "export": true, "false": true, "fg": true,
	"getopts": true, "hash": true, "jobs": true, "kill": true, "printf": true,
	"pwd": true, "read": true, "readonly": true, "return": true, "set": true,
	"shift": true, "test": true, "times": true, "trap": true, "true": true,
	"type": true, "ulimit": true, "umask": true, "unalias": true, "unset": true,
	"wait": true,
}

// visibleExecutables finds literal command words only. It deliberately ignores
// expansions, eval, sh -c payloads, PATH changes, and arguments: static
// preflight cannot safely infer those.
func visibleExecutables(command string) []string {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		args := literalArgs(call)
		if len(args) == 0 {
			return true
		}
		value := args[0]
		if value == "command" {
			args = commandTarget(args[1:])
		} else if value == "exec" {
			args = execTarget(args[1:])
		}
		if len(args) == 0 {
			return true
		}
		name, ok := catalogExecutableName(args[0])
		if !ok {
			return true
		}
		if !shellBuiltins[name] {
			seen[name] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func literalArgs(call *syntax.CallExpr) []string {
	args := make([]string, 0, len(call.Args))
	for _, word := range call.Args {
		if len(word.Parts) != 1 {
			break
		}
		lit, ok := word.Parts[0].(*syntax.Lit)
		if !ok || lit.Value == "" || strings.Contains(lit.Value, "=") {
			break
		}
		args = append(args, lit.Value)
	}
	return args
}

func commandTarget(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		// command -v/-V asks about a command without executing it.
		if strings.ContainsAny(strings.TrimPrefix(args[0], "-"), "vV") {
			return nil
		}
		args = args[1:]
	}
	return args
}

func execTarget(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		option := args[0]
		args = args[1:]
		if option == "-a" && len(args) > 0 {
			args = args[1:]
		}
	}
	return args
}

func catalogExecutableName(value string) (string, bool) {
	if !strings.Contains(value, "/") {
		return value, true
	}
	// The profile catalog describes its image, not arbitrary executables an
	// approved request stages or mounts. Only standard image paths can safely
	// be compared with the catalog's verified-absent list.
	slash := strings.LastIndex(value, "/")
	dir := value[:slash]
	switch dir {
	case "/bin", "/sbin", "/usr/bin", "/usr/sbin", "/usr/local/bin", "/usr/local/sbin":
		return value[slash+1:], true
	default:
		return "", false
	}
}

func commandUsesPipefail(command string) bool {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return pipefailRE.MatchString(command)
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return !found
		}
		args := literalArgs(call)
		if len(args) == 0 || args[0] != "set" {
			return !found
		}
		for i := 1; i+1 < len(args); i++ {
			option := strings.TrimLeft(args[i], "-+")
			if strings.ContainsAny(option, "oO") && args[i+1] == "pipefail" {
				found = true
				return false
			}
		}
		return !found
	})
	return found
}

func commandUsesBashism(command string) bool {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return bashismRE.MatchString(command)
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		switch node := node.(type) {
		case *syntax.TestClause, *syntax.ProcSubst:
			found = true
			return false
		case *syntax.CallExpr:
			args := literalArgs(node)
			if len(args) > 0 && args[0] == "source" {
				found = true
				return false
			}
		}
		return !found
	})
	return found
}

func likelyLongRuntime(command string) bool {
	if regexp.MustCompile(`\b(ansible-playbook|terraform\s+(apply|plan)|tofu\s+(apply|plan))\b`).MatchString(command) {
		return true
	}
	match := sleepRE.FindStringSubmatch(command)
	if len(match) == 0 {
		return false
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		return false
	}
	multiplier := 1
	switch match[2] {
	case "m":
		multiplier = 60
	case "h":
		multiplier = 3600
	case "d":
		multiplier = 86400
	}
	return n*multiplier >= 300
}
