package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func completedJobAndPod() (*batchv1.Job, *corev1.Pod) {
	controller := true
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "sudo-exec-test", Namespace: DefaultControllerNamespace, UID: "job-uid"},
		Status:     batchv1.JobStatus{Succeeded: 1},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sudo-exec-test-pod", Namespace: DefaultControllerNamespace,
			Labels: map[string]string{"job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "executor", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}
	return job, pod
}

func TestCaptureJobOutputBoundsLargeOutput(t *testing.T) {
	ctx := context.Background()
	job, pod := completedJobAndPod()
	sr := &SudoRequest{ObjectMeta: metav1.ObjectMeta{Name: "request", Namespace: DefaultControllerNamespace, UID: "request-uid"}}
	large := strings.Repeat("x", 1024*1024+12345)
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).Build()
	r := &SudoRequestReconciler{
		Client: cl, APIReader: cl,
		PodLogs: func(context.Context, string, string, string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(large)), nil
		},
	}

	result, err := r.captureJobOutput(ctx, sr, job)
	if err != nil {
		t.Fatalf("captureJobOutput: %v", err)
	}
	if result.ExitCode != 0 || result.CaptureState != OutputCaptureTruncated || result.DeliveryState != OutputDeliveryAvailable {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.TotalBytes == nil || *result.TotalBytes != int64(len(large)) {
		t.Fatalf("total bytes = %v, want %d", result.TotalBytes, len(large))
	}
	if result.RetainedBytes == nil || *result.RetainedBytes != MaxOutputBytes {
		t.Fatalf("retained bytes = %v, want %d", result.RetainedBytes, MaxOutputBytes)
	}
	digest := sha256.Sum256([]byte(large))
	if result.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest does not cover complete output")
	}
	var secret corev1.Secret
	if err := cl.Get(ctx, client.ObjectKey{Namespace: DefaultControllerNamespace, Name: result.SecretRef}, &secret); err != nil {
		t.Fatalf("get output Secret: %v", err)
	}
	if got := len(secret.Data["output"]); got != MaxOutputBytes {
		t.Fatalf("Secret output is %d bytes, want %d", got, MaxOutputBytes)
	}
}

func TestCaptureFailurePreservesExitZero(t *testing.T) {
	ctx := context.Background()
	job, pod := completedJobAndPod()
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "request", Namespace: DefaultControllerNamespace, UID: "request-uid"},
		Status:     SudoRequestStatus{Phase: PhaseApproved},
	}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, pod).Build()
	r := &SudoRequestReconciler{
		Client: cl, APIReader: cl, Broadcaster: NewBroadcaster(), Recorder: record.NewFakeRecorder(5),
		PodLogs: func(context.Context, string, string, string) (io.ReadCloser, error) {
			return nil, errors.New("logs unavailable")
		},
	}

	result, err := r.captureJobOutput(ctx, sr, job)
	if err != nil {
		t.Fatalf("captureJobOutput: %v", err)
	}
	if result.ExitCode != 0 || result.CaptureState != OutputCaptureFailed || result.SecretRef != "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	applyCapturedOutputStatus(sr, result)
	if _, err := r.finalizeFromRecordedResult(ctx, sr); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var got SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &got); err != nil {
		t.Fatalf("get request: %v", err)
	}
	if got.Status.Phase != PhaseExecuted || got.Status.ExitCode == nil || *got.Status.ExitCode != 0 {
		t.Fatalf("capture failure changed command outcome: %+v", got.Status)
	}
	if got.Status.OutputCaptureState != OutputCaptureFailed || !strings.Contains(got.Status.OutputFailureReason, "logs unavailable") {
		t.Fatalf("capture failure not recorded: %+v", got.Status)
	}
}

func TestOutputSecretCreationFailureIsDeliveryFailure(t *testing.T) {
	ctx := context.Background()
	job, pod := completedJobAndPod()
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "request", Namespace: DefaultControllerNamespace, UID: "request-uid"},
		Status:     SudoRequestStatus{Phase: PhaseApproved},
	}
	base := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&SudoRequest{}).WithObjects(sr, pod).Build()
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, obj.GetName(), errors.New("denied"))
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	r := &SudoRequestReconciler{
		Client: cl, APIReader: cl, Broadcaster: NewBroadcaster(), Recorder: record.NewFakeRecorder(5),
		PodLogs: func(context.Context, string, string, string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("complete output")), nil
		},
	}

	result, err := r.captureJobOutput(ctx, sr, job)
	if err != nil {
		t.Fatalf("captureJobOutput: %v", err)
	}
	if result.ExitCode != 0 || result.CaptureState != OutputCaptureComplete || result.DeliveryState != OutputDeliveryFailed {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.SecretRef != "" || !strings.Contains(result.FailureReason, "denied") {
		t.Fatalf("delivery failure not recorded: %+v", result)
	}
	applyCapturedOutputStatus(sr, result)
	if _, err := r.finalizeFromRecordedResult(ctx, sr); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	var got SudoRequest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sr), &got); err != nil {
		t.Fatalf("get request: %v", err)
	}
	if got.Status.Phase != PhaseExecuted || got.Status.ExitCode == nil || *got.Status.ExitCode != 0 {
		t.Fatalf("delivery failure changed command outcome: %+v", got.Status)
	}
}

func TestSidecarHeldJobRetriesTransientCaptureFailure(t *testing.T) {
	job, pod := completedJobAndPod()
	job.Status.Succeeded = 0 // executor is terminated, but a sidecar keeps the Job open
	sr := &SudoRequest{ObjectMeta: metav1.ObjectMeta{Name: "request", Namespace: DefaultControllerNamespace, UID: "request-uid"}}
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).Build()
	r := &SudoRequestReconciler{
		Client: cl, APIReader: cl,
		PodLogs: func(context.Context, string, string, string) (io.ReadCloser, error) {
			return nil, errors.New("temporary apiserver error")
		},
	}

	if _, err := r.captureJobOutput(context.Background(), sr, job); err == nil || !strings.Contains(err.Error(), "temporary apiserver error") {
		t.Fatalf("captureJobOutput error = %v, want retriable log error", err)
	}
}

func TestLegacyStatusWithoutOutputMetadataStillDecodes(t *testing.T) {
	var sr SudoRequest
	if err := json.Unmarshal([]byte(`{"status":{"phase":"Executed","exitCode":0,"outputSecretRef":"sudo-out-old"}}`), &sr); err != nil {
		t.Fatalf("decode legacy record: %v", err)
	}
	if sr.Status.ExitCode == nil || *sr.Status.ExitCode != 0 || sr.Status.OutputSecretRef != "sudo-out-old" {
		t.Fatalf("legacy result fields changed: %+v", sr.Status)
	}
	if sr.Status.OutputCaptureState != "" || sr.Status.OutputDeliveryState != "" || sr.Status.OutputTotalBytes != nil {
		t.Fatalf("legacy record should leave new metadata empty: %+v", sr.Status)
	}
}

func TestStatusAPIExposesIndependentOutputState(t *testing.T) {
	total, retained := int64(2_000_000), int64(MaxOutputBytes)
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "request", UID: "request-uid"},
		Status: SudoRequestStatus{
			Phase:               PhaseExecuted,
			OutputSecretRef:     "sudo-out-request",
			OutputCaptureState:  OutputCaptureTruncated,
			OutputDeliveryState: OutputDeliveryAvailable,
			OutputTotalBytes:    &total,
			OutputRetainedBytes: &retained,
			OutputSHA256:        strings.Repeat("a", 64),
		},
	}
	rw := httptest.NewRecorder()
	(&APIServer{}).serveStatus(rw, sr)

	var got requestStatusResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if got.OutputCaptureState != OutputCaptureTruncated || got.OutputDeliveryState != OutputDeliveryAvailable {
		t.Fatalf("output states missing from status response: %+v", got)
	}
	if got.OutputTotalBytes == nil || *got.OutputTotalBytes != total || got.OutputRetainedBytes == nil || *got.OutputRetainedBytes != retained {
		t.Fatalf("output byte counts missing from status response: %+v", got)
	}
}
