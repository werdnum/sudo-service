package main

import (
	"context"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

// getPodLogs streams the named container's logs out of the apiserver.
// We build a typed clientset on the fly because controller-runtime's client
// doesn't expose Pod log subresources.
func getPodLogs(ctx context.Context, namespace, pod, container string) (string, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return "", err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", err
	}
	req := cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	buf, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}
