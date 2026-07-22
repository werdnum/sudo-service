package main

import (
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"mvdan.cc/sh/v3/syntax"
)

// reservedVolumeNames are the controller-owned volume names a requester may not
// reuse: each names a volume the controller appends to the executor pod, so a
// requester volume (or mount) sharing the name would collide into a duplicate the
// apiserver rejects post-approval. The value is the human reason, surfaced in the
// rejection message.
var reservedVolumeNames = map[string]string{
	stdinVolumeName: "the stdin payload",
	tmpVolumeName:   "the writable /tmp scratch",
	homeVolumeName:  "the writable HOME scratch",
}

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
func validateSpecExtras(sr *SudoRequest, controllerNamespace string) error {
	// cluster-admin lives only in the controller namespace (that is where the
	// cluster-admin-bound executor SA exists). A cross-namespace Job runs under
	// the target namespace's default SA, so asking for both at once is incoherent
	// rather than merely unsupported — reject it with a clear message.
	if sr.Spec.Namespace != "" && sr.Spec.Namespace != controllerNamespace {
		if sr.Spec.Privileges.ClusterAdmin != nil && *sr.Spec.Privileges.ClusterAdmin {
			return fmt.Errorf("privileges.clusterAdmin is only available when the executor runs in the %q namespace; it cannot be combined with spec.namespace=%q",
				controllerNamespace, sr.Spec.Namespace)
		}
	}

	// Reject oversized stdin at submission rather than after approval — it is
	// materialised into a Secret, which Kubernetes caps at 1 MiB.
	if len(sr.Spec.Stdin) > MaxStdinBytes {
		return fmt.Errorf("stdin is %d bytes; the limit is %d (it is stored in a Secret, which Kubernetes caps at 1 MiB)", len(sr.Spec.Stdin), MaxStdinBytes)
	}

	// Decode the raw-JSON pod fields into concrete types. A malformed item is a
	// per-request validation error here, not a controller-wide decode failure.
	extras, err := decodePodExtras(sr)
	if err != nil {
		return fmt.Errorf("invalid pod field: %w", err)
	}

	for _, v := range extras.Volumes {
		if why, ok := reservedVolumeNames[v.Name]; ok {
			return fmt.Errorf("volume name %q is reserved for %s", v.Name, why)
		}
		if err := validateVolumeSource(v); err != nil {
			return err
		}
	}

	// The controller appends its own volumes+mounts (the stdin payload at
	// stdinMountDir, and the writable /tmp and HOME scratch); a requester reusing a
	// reserved name would produce a duplicate volume name the apiserver rejects, and
	// reusing stdinMountDir would shadow the payload. (A requester mount at /tmp or
	// homeMountDir is allowed — it opts out of that scratch default, see stampScratch.)
	for _, m := range extras.VolumeMounts {
		if why, ok := reservedVolumeNames[m.Name]; ok {
			return fmt.Errorf("volumeMount name %q is reserved for %s", m.Name, why)
		}
		if m.MountPath == stdinMountDir {
			return fmt.Errorf("volumeMount path %q is reserved for the stdin payload", stdinMountDir)
		}
	}

	// Build the set of volume names the pod will actually have: the requester's
	// volumes plus the controller's stdin volume when stdin is set. A mount that
	// references a name outside this set (or duplicates a path within a container)
	// produces a pod the apiserver rejects at Job creation — which, post-approval,
	// leaves the request stuck in Approved retrying forever. Catch it up front.
	volNames := map[string]bool{}
	for _, v := range extras.Volumes {
		volNames[v.Name] = true
	}
	if sr.Spec.Stdin != "" {
		volNames[stdinVolumeName] = true
	}
	if err := validateMounts("executor container", extras.VolumeMounts, volNames, sr.Spec.Stdin != ""); err != nil {
		return err
	}

	for _, c := range extras.InitContainers {
		if c.Name == "" || c.Image == "" {
			return fmt.Errorf("initContainer must set both name and image")
		}
		// Require an explicit command: without one the image's default entrypoint
		// runs, which the approve page can't show, so the reviewer would approve an
		// init step they can't see.
		if len(c.Command) == 0 {
			return fmt.Errorf("initContainer %q: an explicit command is required (the image's default entrypoint is not shown to the reviewer)", c.Name)
		}
		// Positive allowlist: an init container may only use the reviewable subset
		// of fields the approve page renders. Everything else — securityContext,
		// lifecycle hooks, volumeDevices, restartPolicy (sidecars), probes, ports,
		// requester-set resources, ... — is rejected by this single rule (rather
		// than denied field-by-field), so what runs can always be faithfully shown
		// to the human. securityContext and resources are stamped by the controller.
		permitted := corev1.Container{
			Name:         c.Name,
			Image:        c.Image,
			Command:      c.Command,
			Args:         c.Args,
			Env:          c.Env,
			EnvFrom:      c.EnvFrom,
			VolumeMounts: c.VolumeMounts,
		}
		if !reflect.DeepEqual(c, permitted) {
			return fmt.Errorf("initContainer %q: only name, image, command, args, env, envFrom and volumeMounts are permitted", c.Name)
		}
		// Init containers may not mount the controller-owned stdin volume (the
		// approve page presents stdin as fed to the executor command, not as a file
		// other containers read), and may only reference defined volumes.
		for _, m := range c.VolumeMounts {
			if why, ok := reservedVolumeNames[m.Name]; ok {
				return fmt.Errorf("initContainer %q: volumeMount name %q is reserved for %s", c.Name, m.Name, why)
			}
		}
		if err := validateMounts(fmt.Sprintf("initContainer %q", c.Name), c.VolumeMounts, volNames, false); err != nil {
			return err
		}
	}

	// imagePullSecrets are LocalObjectReferences ({name}); the only thing to vet
	// is a present name (the apiserver would reject a nameless ref at Job
	// creation, post-approval, leaving the request stuck in Approved). They are
	// not exposed to the container — the kubelet uses them only for registry
	// auth — so no allowlist or SA-token check applies; their names are surfaced
	// to the reviewer.
	for i, ref := range extras.ImagePullSecrets {
		if ref.Name == "" {
			return fmt.Errorf("imagePullSecrets[%d]: name is required", i)
		}
	}

	return nil
}

// validateMounts checks that every mount references a defined volume and that no
// two mounts in the same container share a mountPath. hasStdin marks the
// controller's stdin volume/path as already taken so a requester mount can't
// collide with it.
func validateMounts(where string, mounts []corev1.VolumeMount, volNames map[string]bool, hasStdin bool) error {
	seenPaths := map[string]bool{}
	if hasStdin {
		seenPaths[stdinMountDir] = true
	}
	for _, m := range mounts {
		if !volNames[m.Name] {
			return fmt.Errorf("%s: volumeMount %q references no volume named %q", where, m.MountPath, m.Name)
		}
		// Positive allowlist of mount fields the approve page renders. Anything else
		// — subPathExpr (variable-expanded subpath), mountPropagation,
		// recursiveReadOnly — would change what's mounted/written without the
		// reviewer seeing it.
		permitted := corev1.VolumeMount{Name: m.Name, MountPath: m.MountPath, SubPath: m.SubPath, ReadOnly: m.ReadOnly}
		if !reflect.DeepEqual(m, permitted) {
			return fmt.Errorf("%s: volumeMount %q may only set name, mountPath, subPath and readOnly", where, m.MountPath)
		}
		if seenPaths[m.MountPath] {
			return fmt.Errorf("%s: duplicate mountPath %q", where, m.MountPath)
		}
		seenPaths[m.MountPath] = true
	}
	return nil
}

// validateVolumeSource rejects any volume whose source is outside the reviewable
// allowlist. The allowlist itself lives in describeVolumeSource, which is the
// single source of truth shared with the approve-page renderer, so a source can
// never be permitted by validation yet rendered as "unknown" to the reviewer
// (or vice versa).
func validateVolumeSource(v corev1.Volume) error {
	if v.Name == "" {
		return fmt.Errorf("volume must have a name")
	}
	if v.HostPath != nil {
		return fmt.Errorf("volume %q: hostPath is not permitted (it would require an explicit privilege toggle, which does not exist yet)", v.Name)
	}
	if v.Projected != nil {
		return fmt.Errorf("volume %q: projected volumes are not permitted (a serviceAccountToken source could mint an API/cloud token for the namespace default ServiceAccount, bypassing the no-privileges guarantee)", v.Name)
	}
	if _, allowed := describeVolumeSource(v); !allowed {
		return fmt.Errorf("volume %q: only emptyDir, secret, configMap and persistentVolumeClaim sources are permitted", v.Name)
	}
	return nil
}
