package main

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// specExtrasView is the reviewer-facing summary of a request's widened pod
// fields: where it runs, what privilege it holds, and what it mounts. The
// approve page renders it as named rows so the human sees exactly what power is
// being handed over, rather than having to infer it from a command string.
type specExtrasView struct {
	Namespace      string
	ClusterAdmin   bool
	Stdin          bool
	Volumes        []string
	Mounts         []string
	Env            []string
	EnvFrom        []string
	InitContainers []string
}

// HasAny reports whether the request touches any widened field worth surfacing.
// (Namespace and ClusterAdmin are always meaningful, so this is true whenever
// the request is anything other than a plain in-namespace cluster-admin command.)
func (v specExtrasView) HasAny() bool {
	return v.Namespace != ControllerNamespace || !v.ClusterAdmin || v.Stdin ||
		len(v.Volumes) > 0 || len(v.Mounts) > 0 || len(v.Env) > 0 ||
		len(v.EnvFrom) > 0 || len(v.InitContainers) > 0
}

func newSpecExtrasView(sr *SudoRequest) specExtrasView {
	v := specExtrasView{
		Namespace:    executorNamespace(sr),
		ClusterAdmin: clusterAdminEnabled(sr),
		Stdin:        sr.Spec.Stdin != "",
	}
	for _, vol := range sr.Spec.Volumes {
		v.Volumes = append(v.Volumes, fmt.Sprintf("%s: %s", vol.Name, volumeSourceDesc(vol)))
	}
	for _, m := range sr.Spec.VolumeMounts {
		ro := ""
		if m.ReadOnly {
			ro = " (ro)"
		}
		sub := ""
		if m.SubPath != "" {
			sub = " [" + m.SubPath + "]"
		}
		v.Mounts = append(v.Mounts, fmt.Sprintf("%s <- %s%s%s", m.MountPath, m.Name, sub, ro))
	}
	for _, e := range sr.Spec.Env {
		v.Env = append(v.Env, e.Name)
	}
	for _, ef := range sr.Spec.EnvFrom {
		switch {
		case ef.SecretRef != nil:
			v.EnvFrom = append(v.EnvFrom, "secret/"+ef.SecretRef.Name)
		case ef.ConfigMapRef != nil:
			v.EnvFrom = append(v.EnvFrom, "configMap/"+ef.ConfigMapRef.Name)
		}
	}
	for _, c := range sr.Spec.InitContainers {
		v.InitContainers = append(v.InitContainers, fmt.Sprintf("%s (%s)", c.Name, c.Image))
	}
	return v
}

func volumeSourceDesc(v corev1.Volume) string {
	switch {
	case v.Secret != nil:
		return "secret/" + v.Secret.SecretName
	case v.ConfigMap != nil:
		return "configMap/" + v.ConfigMap.Name
	case v.PersistentVolumeClaim != nil:
		ro := ""
		if v.PersistentVolumeClaim.ReadOnly {
			ro = " (ro)"
		}
		return "pvc/" + v.PersistentVolumeClaim.ClaimName + ro
	case v.EmptyDir != nil:
		return "emptyDir"
	case v.Projected != nil:
		return "projected"
	default:
		return "unknown"
	}
}

// specExtrasText is the plain-text rendering of the same information, appended to
// the Pushover approval push and handed to the AI summarizer for context. Empty
// when the request is a plain in-namespace cluster-admin command.
func specExtrasText(sr *SudoRequest) string {
	v := newSpecExtrasView(sr)
	if !v.HasAny() {
		return ""
	}
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
	if len(v.InitContainers) > 0 {
		fmt.Fprintf(&b, "initContainers: %s\n", strings.Join(v.InitContainers, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}
