package main

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"mvdan.cc/sh/v3/syntax"
)

// validateCommandSyntax parses command as a shell script and returns a non-nil
// error if it is syntactically invalid (unbalanced quotes, a dangling pipe, an
// unterminated `$(`, etc.). It never executes anything — the parser only reads
// the grammar — so it is safe to run on untrusted input in the controller.
//
// The executor runs the command as `sh -c <command>`, so a syntax error here
// guarantees the command can never run. Catching it at submission/acceptance
// time short-circuits the human-approval round-trip for a request that was
// doomed anyway.
//
// We parse in the bash language variant deliberately: it accepts a superset of
// POSIX sh, so we only reject input that is broken in *every* shell. The
// executor's busybox `ash` is stricter than bash for a handful of extensions,
// so a command can still fail at runtime; this check is a cheap early filter
// for obvious typos, not a guarantee of executability. The human reviewer
// remains the trust boundary.
func validateCommandSyntax(command string) error {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	if _, err := parser.Parse(strings.NewReader(command), ""); err != nil {
		return fmt.Errorf("invalid shell syntax: %w", err)
	}
	return nil
}

// validateSpecExtras enforces the curated allowlist on the structured pod
// fields (namespace, volumes, init containers, privilege toggles). The Go types
// reuse the upstream corev1 structs for free DeepCopy and a one-line splice into
// the executor pod, so this is the single place that narrows them back down to a
// reviewable, non-escalating subset. Both submission paths run it: the HTTP API
// rejects a bad spec with 400, and a CRD-created one is moved to Denied in
// handleNew before any approval push — exactly like the shell-syntax check.
//
// It deliberately does NOT try to judge whether the command is sensible; the
// human reviewer remains the trust boundary. It only blocks fields that would
// escalate privilege past what the request has explicitly, visibly asked for.
func validateSpecExtras(sr *SudoRequest) error {
	// cluster-admin lives only in the controller namespace (that is where the
	// cluster-admin-bound executor SA exists). A cross-namespace Job runs under
	// the target namespace's default SA, so asking for both at once is incoherent
	// rather than merely unsupported — reject it with a clear message.
	if sr.Spec.Namespace != "" && sr.Spec.Namespace != ControllerNamespace {
		if sr.Spec.Privileges.ClusterAdmin != nil && *sr.Spec.Privileges.ClusterAdmin {
			return fmt.Errorf("privileges.clusterAdmin is only available when the executor runs in the %q namespace; it cannot be combined with spec.namespace=%q",
				ControllerNamespace, sr.Spec.Namespace)
		}
	}

	for _, v := range sr.Spec.Volumes {
		if err := validateVolumeSource(v); err != nil {
			return err
		}
	}

	for _, c := range sr.Spec.InitContainers {
		if c.Name == "" || c.Image == "" {
			return fmt.Errorf("initContainer must set both name and image")
		}
		// The controller stamps the locked-down securityContext onto init
		// containers; letting the requester set it would be an escalation path
		// that bypasses the privilege toggles.
		if c.SecurityContext != nil {
			return fmt.Errorf("initContainer %q: securityContext is set by the controller and may not be specified", c.Name)
		}
	}

	return nil
}

// allowedVolumeSources is the reviewable set of volume sources a request may use
// without a privilege toggle. hostPath and other escalation-capable sources are
// intentionally excluded until they have an explicit approval-surfaced flag.
func validateVolumeSource(v corev1.Volume) error {
	if v.Name == "" {
		return fmt.Errorf("volume must have a name")
	}
	if v.HostPath != nil {
		return fmt.Errorf("volume %q: hostPath is not permitted (it would require an explicit privilege toggle, which does not exist yet)", v.Name)
	}
	switch {
	case v.EmptyDir != nil, v.Secret != nil, v.ConfigMap != nil, v.PersistentVolumeClaim != nil, v.Projected != nil:
		return nil
	default:
		return fmt.Errorf("volume %q: only emptyDir, secret, configMap, persistentVolumeClaim and projected sources are permitted", v.Name)
	}
}
