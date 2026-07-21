package main

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func managedLifecycleRequest(state string) *SudoRequest {
	deadline := int32(3600)
	return &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: "managed", Namespace: ControllerNamespace, UID: types.UID("request-uid"),
			Finalizers: []string{executorCleanupFinalizer},
		},
		Spec: SudoRequestSpec{
			Requester: "alice", Reason: "long drift run", Command: "ansible-playbook drift.yaml",
			Execution: SudoRequestExecution{
				Mode: ExecutionModeManagedJob, ResourceClass: ResourceClassLongRunning,
				ActiveDeadlineSeconds: &deadline,
			},
		},
		Status: SudoRequestStatus{
			Phase: PhaseApproved, ExecutorJobName: "sudo-exec-managed",
			ExecutorJobUID: "job-uid", ExecutorJobLifecycle: state,
		},
	}
}

func managedLifecycleJob(sr *SudoRequest) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: sr.Status.ExecutorJobName, Namespace: ControllerNamespace,
		UID: types.UID(sr.Status.ExecutorJobUID), CreationTimestamp: metav1.Now(),
		OwnerReferences: []metav1.OwnerReference{ownerRef(sr)},
	}}
}

func managedLifecycleReconciler(t *testing.T, objects ...client.Object) (*SudoRequestReconciler, client.WithWatch) {
	t.Helper()
	scheme := raceTestScheme(t)
	base := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&SudoRequest{}).WithObjects(objects...).Build()
	return &SudoRequestReconciler{
		Client: base, APIReader: base, Scheme: scheme,
		Recorder: record.NewFakeRecorder(32), Broadcaster: NewBroadcaster(),
	}, base
}

func getLifecycleRequest(t *testing.T, c client.Client) SudoRequest {
	t.Helper()
	var got SudoRequest
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ControllerNamespace, Name: "managed"}, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestManagedJobCreatedAndRunningStatesAreRestartSafe(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{JobLifecycleCreated, JobLifecycleRunning} {
		t.Run(state, func(t *testing.T) {
			sr := managedLifecycleRequest(state)
			job := managedLifecycleJob(sr)
			objects := []client.Object{sr, job}
			if state == JobLifecycleRunning {
				controller := true
				objects = append(objects, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "executor-pod", Namespace: ControllerNamespace,
						Labels: map[string]string{"job-name": job.Name},
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: "batch/v1", Kind: "Job", Name: job.Name,
							UID: job.UID, Controller: &controller,
						}},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
						Name: "executor", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
					}}},
				})
			}
			r, base := managedLifecycleReconciler(t, objects...)
			// Recreate the reconciler between calls to model a controller restart.
			if _, err := r.handleApproved(ctx, sr.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			r2 := *r
			current := getLifecycleRequest(t, base)
			if _, err := r2.handleApproved(ctx, current.DeepCopy()); err != nil {
				t.Fatal(err)
			}
			got := getLifecycleRequest(t, base)
			if got.Status.Phase != PhaseApproved || got.Status.ExecutorJobLifecycle != state {
				t.Fatalf("after restart phase/lifecycle=%s/%s, want Approved/%s", got.Status.Phase, got.Status.ExecutorJobLifecycle, state)
			}
		})
	}
}

func TestManagedJobResultSurvivesStuckCleanupAndRestart(t *testing.T) {
	ctx := context.Background()
	sr := managedLifecycleRequest(JobLifecycleResultCaptured)
	exit := int32(0)
	total, retained := int64(42), int64(42)
	sr.Status.ExitCode = &exit
	sr.Status.OutputSecretRef = "sudo-out-managed"
	sr.Status.OutputCaptureState = OutputCaptureComplete
	sr.Status.OutputDeliveryState = OutputDeliveryAvailable
	sr.Status.OutputTotalBytes = &total
	sr.Status.OutputRetainedBytes = &retained
	sr.Status.OutputSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	job := managedLifecycleJob(sr)
	r, base := managedLifecycleReconciler(t, sr, job)

	// Restart from ResultCaptured: first persist CleanupRequested without touching
	// the Job or any captured-output field.
	if _, err := r.handleApproved(ctx, sr.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	current := getLifecycleRequest(t, base)
	if current.Status.ExecutorJobLifecycle != JobLifecycleCleanupRequested || current.Status.OutputSecretRef != "sudo-out-managed" {
		t.Fatalf("result was not durably preserved before cleanup: %+v", current.Status)
	}

	// A delete outage must leave the request Approved with its result intact.
	deleteErr := errors.New("apiserver delete unavailable")
	stuck := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return deleteErr
		},
	})
	rStuck := *r
	rStuck.Client = stuck
	if _, err := rStuck.handleApproved(ctx, current.DeepCopy()); !errors.Is(err, deleteErr) {
		t.Fatalf("cleanup error=%v, want %v", err, deleteErr)
	}
	current = getLifecycleRequest(t, base)
	if current.Status.Phase != PhaseApproved || current.Status.OutputSecretRef != "sudo-out-managed" || current.Status.ExitCode == nil {
		t.Fatalf("stuck deletion lost or terminalized result: %+v", current.Status)
	}

	// A fresh controller retries the UID-preconditioned foreground delete. The
	// following restart observes NotFound and only then records terminal success.
	rRestart := *r
	if _, err := rRestart.handleApproved(ctx, current.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	current = getLifecycleRequest(t, base)
	if _, err := rRestart.handleApproved(ctx, current.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	got := getLifecycleRequest(t, base)
	if got.Status.Phase != PhaseExecuted || got.Status.ExecutorJobLifecycle != JobLifecycleCleaned || got.Status.OutputSecretRef != "sudo-out-managed" {
		t.Fatalf("final managed result = %+v", got.Status)
	}
}

func TestManagedJobPendingFailureCleansBeforeTerminal(t *testing.T) {
	ctx := context.Background()
	sr := managedLifecycleRequest(JobLifecycleRunning)
	job := managedLifecycleJob(sr)
	r, base := managedLifecycleReconciler(t, sr, job)
	if _, err := r.failApproved(ctx, sr.DeepCopy(), "executor start timed out"); err != nil {
		t.Fatal(err)
	}
	current := getLifecycleRequest(t, base)
	if current.Status.Phase != PhaseApproved || current.Status.ExecutorJobLifecycle != JobLifecycleCleanupRequested {
		t.Fatalf("failure became terminal before cleanup: %+v", current.Status)
	}
	if current.Status.FailureReason != "executor start timed out" {
		t.Fatalf("failure reason not persisted: %+v", current.Status)
	}
	if _, err := r.handleApproved(ctx, current.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	got := getLifecycleRequest(t, base)
	if got.Status.Phase != PhaseFailed || got.Status.ExecutorJobLifecycle != JobLifecycleCleaned {
		t.Fatalf("cleaned failure = %+v", got.Status)
	}
}

func TestManagedJobForegroundDeleteUsesUIDPrecondition(t *testing.T) {
	sr := managedLifecycleRequest(JobLifecycleCleanupRequested)
	job := managedLifecycleJob(sr)
	scheme := raceTestScheme(t)
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job).Build()
	checked := false
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			var options client.DeleteOptions
			options.ApplyOptions(opts)
			if options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
				t.Fatalf("propagation=%v, want Foreground", options.PropagationPolicy)
			}
			if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != job.UID {
				t.Fatalf("preconditions=%+v, want UID %s", options.Preconditions, job.UID)
			}
			checked = true
			return c.Delete(ctx, obj, opts...)
		},
	})
	r := &SudoRequestReconciler{Client: cl}
	if err := r.stopManagedJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("delete interceptor was not called")
	}
}
