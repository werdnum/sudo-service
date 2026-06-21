package main

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// specExtrasView is the reviewer-facing summary of a request's widened pod
// fields: where it runs, what privilege it holds, and what it mounts and runs.
// The approve page renders it as named rows so the human sees exactly what power
// is being handed over — including each init container's command and mounts —
// rather than having to infer it from a command string.
type specExtrasView struct {
	Namespace      string
	ClusterAdmin   bool
	Stdin          bool
	Volumes        []string
	Mounts         []string
	Env            []string
	EnvFrom        []string
	InitContainers []initContainerView
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

func newSpecExtrasView(sr *SudoRequest) specExtrasView {
	v := specExtrasView{
		Namespace:    executorNamespace(sr),
		ClusterAdmin: clusterAdminEnabled(sr),
		Stdin:        sr.Spec.Stdin != "",
	}
	for _, vol := range sr.Spec.Volumes {
		desc, _ := describeVolumeSource(vol)
		v.Volumes = append(v.Volumes, fmt.Sprintf("%s: %s", vol.Name, desc))
	}
	for _, m := range sr.Spec.VolumeMounts {
		v.Mounts = append(v.Mounts, describeMount(m))
	}
	for _, e := range sr.Spec.Env {
		v.Env = append(v.Env, e.Name)
	}
	v.EnvFrom = describeEnvFrom(sr.Spec.EnvFrom)
	for _, c := range sr.Spec.InitContainers {
		icv := initContainerView{
			Name:    c.Name,
			Image:   c.Image,
			Command: strings.TrimSpace(strings.Join(append(append([]string{}, c.Command...), c.Args...), " ")),
			EnvFrom: describeEnvFrom(c.EnvFrom),
		}
		for _, m := range c.VolumeMounts {
			icv.Mounts = append(icv.Mounts, describeMount(m))
		}
		for _, e := range c.Env {
			icv.Env = append(icv.Env, e.Name)
		}
		v.InitContainers = append(v.InitContainers, icv)
	}
	return v
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
		switch {
		case ef.SecretRef != nil:
			out = append(out, "secret/"+ef.SecretRef.Name)
		case ef.ConfigMapRef != nil:
			out = append(out, "configMap/"+ef.ConfigMapRef.Name)
		}
	}
	return out
}

// describeVolumeSource is the single source of truth for the reviewable volume
// allowlist: it returns a human description of v's source and whether that source
// is permitted. validateVolumeSource gates on the bool; the approve page renders
// the string. Keeping them in one function means a source can never be permitted
// by validation yet shown to the reviewer as "unknown".
func describeVolumeSource(v corev1.Volume) (desc string, allowed bool) {
	switch {
	case v.EmptyDir != nil:
		return "emptyDir", true
	case v.Secret != nil:
		return "secret/" + v.Secret.SecretName, true
	case v.ConfigMap != nil:
		return "configMap/" + v.ConfigMap.Name, true
	case v.PersistentVolumeClaim != nil:
		ro := ""
		if v.PersistentVolumeClaim.ReadOnly {
			ro = " (ro)"
		}
		return "pvc/" + v.PersistentVolumeClaim.ClaimName + ro, true
	case v.Projected != nil:
		return "projected", true
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
func specExtrasText(sr *SudoRequest) string {
	if !hasSpecExtras(sr) {
		return ""
	}
	v := newSpecExtrasView(sr)
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
