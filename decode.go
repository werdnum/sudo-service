package main

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// podExtras holds the decoded, concrete forms of the widened pod fields. The
// spec stores them as runtime.RawExtension (see SudoRequestSpec) so a malformed
// object can't break the controller's typed List decode; decodePodExtras turns
// them into corev1 types, returning an error for any item that doesn't decode —
// which callers surface as a per-request rejection rather than a cache-wide DoS.
type podExtras struct {
	Env              []corev1.EnvVar
	EnvFrom          []corev1.EnvFromSource
	Volumes          []corev1.Volume
	VolumeMounts     []corev1.VolumeMount
	InitContainers   []corev1.Container
	ImagePullSecrets []corev1.LocalObjectReference
}

func decodePodExtras(sr *SudoRequest) (*podExtras, error) {
	e := &podExtras{}
	if err := decodeRawList(sr.Spec.Env, &e.Env); err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	if err := decodeRawList(sr.Spec.EnvFrom, &e.EnvFrom); err != nil {
		return nil, fmt.Errorf("envFrom: %w", err)
	}
	if err := decodeRawList(sr.Spec.Volumes, &e.Volumes); err != nil {
		return nil, fmt.Errorf("volumes: %w", err)
	}
	if err := decodeRawList(sr.Spec.VolumeMounts, &e.VolumeMounts); err != nil {
		return nil, fmt.Errorf("volumeMounts: %w", err)
	}
	if err := decodeRawList(sr.Spec.InitContainers, &e.InitContainers); err != nil {
		return nil, fmt.Errorf("initContainers: %w", err)
	}
	if err := decodeRawList(sr.Spec.ImagePullSecrets, &e.ImagePullSecrets); err != nil {
		return nil, fmt.Errorf("imagePullSecrets: %w", err)
	}
	return e, nil
}

// decodeRawList strictly unmarshals each raw item into T. A type-confused item
// (e.g. a numeric string field) yields an error naming the index.
func decodeRawList[T any](raw []runtime.RawExtension, out *[]T) error {
	if len(raw) == 0 {
		return nil
	}
	res := make([]T, 0, len(raw))
	for i := range raw {
		if len(raw[i].Raw) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(raw[i].Raw, &v); err != nil {
			return fmt.Errorf("item %d: %w", i, err)
		}
		res = append(res, v)
	}
	*out = res
	return nil
}
