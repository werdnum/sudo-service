package main

import (
	"context"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
)

// streamPodLogs opens the named container's log stream from the apiserver.
// We build a typed clientset on the fly because controller-runtime's client
// doesn't expose Pod log subresources.
func streamPodLogs(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	req := cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
	})
	return req.Stream(ctx)
}
