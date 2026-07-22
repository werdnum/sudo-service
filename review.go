package main

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// displayPodTemplate renders the exact pod spec the executor Job will run, as
// YAML, for the ground-truth section of the approve page. It is the same
// buildExecutorJob the controller uses, so the reviewer sees the real thing — no
// curation gap is possible. Names minted at approval time (the Job, the stdin
// Secret) aren't known yet, so placeholders stand in. With redactEnv set, literal
// env values are masked (for the off-cluster AI aid; the human page passes false).
func displayPodTemplate(sr *SudoRequest, redactEnv bool) (string, error) {
	extras, err := decodePodExtras(sr)
	if err != nil {
		return "", err
	}
	d := sr.DeepCopy()
	if d.Spec.Stdin != "" && d.Status.StdinSecretName == "" {
		d.Status.StdinSecretName = "<minted-at-approval>"
	}
	job := buildExecutorJob(d, executorNamespace(d), "<minted-at-approval>", extras)
	pod := job.Spec.Template.Spec
	if redactEnv {
		redactPodEnv(&pod)
	}
	out, err := yaml.Marshal(pod)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func redactPodEnv(pod *corev1.PodSpec) {
	redact := func(cs []corev1.Container) {
		for i := range cs {
			for j := range cs[i].Env {
				if cs[i].Env[j].Value != "" && cs[i].Env[j].ValueFrom == nil {
					cs[i].Env[j].Value = "<redacted>"
				}
			}
		}
	}
	redact(pod.InitContainers)
	redact(pod.Containers)
}

// specExtrasView is the reviewer-facing summary of a request's widened pod
// fields: where it runs, what privilege it holds, and what it mounts and runs.
// The approve page renders it as named rows so the human sees exactly what power
// is being handed over — including each init container's command and mounts —
// rather than having to infer it from a command string.
type specExtrasView struct {
	Namespace        string
	ClusterAdmin     bool
	Stdin            bool
	Volumes          []string
	Mounts           []string
	Env              []string
	EnvFrom          []string
	ImagePullSecrets []string
	InitContainers   []initContainerView
}

// initContainerView surfaces everything a requester-supplied init container can
// do — it runs before the executor, in the same pod, with the same mounts — so
// the reviewer can see its command and what it touches, not just its image.
type initContainerView struct {
	Name    string
	Image   string
	Command string
	Mounts  []string
	Env     []string
	EnvFrom []string
}

// newSpecExtrasView renders the widened fields for review. redactEnv hides
// literal env values (NAME=<redacted>) for contexts that leave the cluster — the
// AI summarizer — while the human-facing approve page and push pass false so the
// reviewer still sees exactly what value is being approved.
func newSpecExtrasView(sr *SudoRequest, redactEnv bool) specExtrasView {
	v := specExtrasView{
		Namespace:    executorNamespace(sr),
		ClusterAdmin: clusterAdminEnabled(sr),
		Stdin:        sr.Spec.Stdin != "",
	}
	// Best-effort decode: by the time the approve page or push renders, the spec
	// has passed validateSpecExtras, so this succeeds; a (theoretical) decode
	// failure just yields an empty extras view rather than panicking.
	extras, err := decodePodExtras(sr)
	if err != nil {
		return v
	}
	for _, vol := range extras.Volumes {
		desc, _ := describeVolumeSource(vol)
		v.Volumes = append(v.Volumes, fmt.Sprintf("%s: %s", vol.Name, desc))
	}
	for _, m := range extras.VolumeMounts {
		v.Mounts = append(v.Mounts, describeMount(m))
	}
	v.Env = describeEnv(extras.Env, redactEnv)
	v.EnvFrom = describeEnvFrom(extras.EnvFrom)
	for _, ips := range extras.ImagePullSecrets {
		v.ImagePullSecrets = append(v.ImagePullSecrets, ips.Name)
	}
	for _, c := range extras.InitContainers {
		icv := initContainerView{
			Name:    c.Name,
			Image:   c.Image,
			Command: shellJoin(append(append([]string{}, c.Command...), c.Args...)),
			Env:     describeEnv(c.Env, redactEnv),
			EnvFrom: describeEnvFrom(c.EnvFrom),
		}
		for _, m := range c.VolumeMounts {
			icv.Mounts = append(icv.Mounts, describeMount(m))
		}
		v.InitContainers = append(v.InitContainers, icv)
	}
	return v
}

// describeEnv renders each env var with its value or source, not just its name —
// a literal value (KUBECONFIG=..., AWS_*) or a valueFrom secret/configMap key is
// part of what the human is approving and must be visible.
func describeEnv(env []corev1.EnvVar, redact bool) []string {
	var out []string
	for _, e := range env {
		switch {
		case e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil:
			out = append(out, fmt.Sprintf("%s <- secret/%s:%s", e.Name, e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Key))
		case e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil:
			out = append(out, fmt.Sprintf("%s <- configMap/%s:%s", e.Name, e.ValueFrom.ConfigMapKeyRef.Name, e.ValueFrom.ConfigMapKeyRef.Key))
		case e.ValueFrom != nil && e.ValueFrom.FieldRef != nil:
			out = append(out, fmt.Sprintf("%s <- field:%s", e.Name, e.ValueFrom.FieldRef.FieldPath))
		case e.ValueFrom != nil && e.ValueFrom.ResourceFieldRef != nil:
			out = append(out, fmt.Sprintf("%s <- resource:%s", e.Name, e.ValueFrom.ResourceFieldRef.Resource))
		case redact:
			// A literal value may be a credential; don't ship it off-cluster.
			out = append(out, e.Name+"=<redacted>")
		default:
			out = append(out, fmt.Sprintf("%s=%s", e.Name, e.Value))
		}
	}
	return out
}

func describeMount(m corev1.VolumeMount) string {
	ro := ""
	if m.ReadOnly {
		ro = " (ro)"
	}
	sub := ""
	if m.SubPath != "" {
		sub = " [" + m.SubPath + "]"
	}
	return fmt.Sprintf("%s <- %s%s%s", m.MountPath, m.Name, sub, ro)
}

func describeEnvFrom(sources []corev1.EnvFromSource) []string {
	var out []string
	for _, ef := range sources {
		// The prefix is prepended to every imported key, so a "benign" source can
		// still create behaviour-changing vars (AWS_*, KUBECONFIG_*) — show it.
		prefix := ""
		if ef.Prefix != "" {
			prefix = " prefix=" + ef.Prefix
		}
		switch {
		case ef.SecretRef != nil:
			out = append(out, "secret/"+ef.SecretRef.Name+prefix)
		case ef.ConfigMapRef != nil:
			out = append(out, "configMap/"+ef.ConfigMapRef.Name+prefix)
		}
	}
	return out
}

// keyPathItems renders a secret/configMap volume's key->path projections, so the
// reviewer sees e.g. that the key "admin-token" is mounted at an innocuous path,
// not just the object name. Empty when the volume projects all keys at default
// paths.
func keyPathItems(items []corev1.KeyToPath) string {
	if len(items) == 0 {
		return ""
	}
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = it.Key + "->" + it.Path
	}
	return " [" + strings.Join(parts, ", ") + "]"
}

// shellJoin renders an argv slice so the reviewer can see token boundaries — a
// multi-word `sh -c '<script>'` argument stays visibly one token rather than
// blurring into the surrounding flags. Tokens with shell-significant characters
// are single-quoted.
func shellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '/' || r == ':' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// describeVolumeSource is the single source of truth for the reviewable volume
// allowlist: it returns a human description of v's source and whether that source
// is permitted. validateVolumeSource gates on the bool; the approve page renders
// the string. Keeping them in one function means a source can never be permitted
// by validation yet shown to the reviewer as "unknown".
func describeVolumeSource(v corev1.Volume) (desc string, allowed bool) {
	switch {
	case v.EmptyDir != nil:
		// Show an explicit requester-provided sizeLimit. Without one, make the
		// absence of a sudo-service-specific ceiling equally explicit.
		size := "no sizeLimit"
		if v.EmptyDir.SizeLimit != nil {
			size = v.EmptyDir.SizeLimit.String()
		}
		return "emptyDir (" + size + ")", true
	case v.Secret != nil:
		return "secret/" + v.Secret.SecretName + keyPathItems(v.Secret.Items), true
	case v.ConfigMap != nil:
		return "configMap/" + v.ConfigMap.Name + keyPathItems(v.ConfigMap.Items), true
	case v.PersistentVolumeClaim != nil:
		ro := ""
		if v.PersistentVolumeClaim.ReadOnly {
			ro = " (ro)"
		}
		return "pvc/" + v.PersistentVolumeClaim.ClaimName + ro, true
	case v.Projected != nil:
		// Not permitted: a projected volume can include a serviceAccountToken
		// source, which would mint an API/cloud-capable token for the pod's
		// namespace default SA — bypassing the "no privileges" guarantee for
		// cross-namespace Jobs. Excluded until it has an explicit privilege toggle.
		return "projected", false
	case v.HostPath != nil:
		return "hostPath:" + v.HostPath.Path, false
	default:
		return "unknown", false
	}
}

// specExtrasText is the plain-text rendering of the same information, appended to
// the Pushover approval push and handed to the AI summarizer for context. Empty
// when the request is a plain command (hasSpecExtras, the same predicate that
// excludes it from auto-approve and routes it to a human).
func specExtrasText(sr *SudoRequest, redactEnv bool) string {
	if !hasSpecExtras(sr) {
		return ""
	}
	v := newSpecExtrasView(sr, redactEnv)
	var b strings.Builder
	fmt.Fprintf(&b, "namespace: %s\n", v.Namespace)
	if v.ClusterAdmin {
		b.WriteString("privileges: cluster-admin\n")
	} else {
		b.WriteString("privileges: none (namespace default ServiceAccount)\n")
	}
	if v.Stdin {
		b.WriteString("stdin: provided\n")
	}
	if len(v.Volumes) > 0 {
		fmt.Fprintf(&b, "volumes: %s\n", strings.Join(v.Volumes, ", "))
	}
	if len(v.Mounts) > 0 {
		fmt.Fprintf(&b, "mounts: %s\n", strings.Join(v.Mounts, ", "))
	}
	if len(v.EnvFrom) > 0 {
		fmt.Fprintf(&b, "envFrom: %s\n", strings.Join(v.EnvFrom, ", "))
	}
	if len(v.Env) > 0 {
		fmt.Fprintf(&b, "env: %s\n", strings.Join(v.Env, ", "))
	}
	if len(v.ImagePullSecrets) > 0 {
		fmt.Fprintf(&b, "imagePullSecrets: %s\n", strings.Join(v.ImagePullSecrets, ", "))
	}
	for _, ic := range v.InitContainers {
		fmt.Fprintf(&b, "initContainer %s (%s): %s\n", ic.Name, ic.Image, ic.Command)
		if len(ic.Mounts) > 0 {
			fmt.Fprintf(&b, "  mounts: %s\n", strings.Join(ic.Mounts, ", "))
		}
		if len(ic.EnvFrom) > 0 {
			fmt.Fprintf(&b, "  envFrom: %s\n", strings.Join(ic.EnvFrom, ", "))
		}
		if len(ic.Env) > 0 {
			fmt.Fprintf(&b, "  env: %s\n", strings.Join(ic.Env, ", "))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
