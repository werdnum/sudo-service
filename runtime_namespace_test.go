package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testControllerNamespace = "sudo-service-alt"

func TestLoadConfigUsesPodNamespaceWithDefault(t *testing.T) {
	t.Setenv("PUSHOVER_TOKEN", "token")
	t.Setenv("PUSHOVER_USER_KEY", "user")
	t.Setenv("POD_NAMESPACE", testControllerNamespace)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ControllerNamespace != testControllerNamespace {
		t.Fatalf("controller namespace=%q, want %q", cfg.ControllerNamespace, testControllerNamespace)
	}

	t.Setenv("POD_NAMESPACE", "")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("loadConfig default: %v", err)
	}
	if cfg.ControllerNamespace != DefaultControllerNamespace {
		t.Fatalf("default controller namespace=%q, want %q", cfg.ControllerNamespace, DefaultControllerNamespace)
	}

	t.Setenv("POD_NAMESPACE", "Invalid_Namespace")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "POD_NAMESPACE") {
		t.Fatalf("invalid POD_NAMESPACE error=%v, want clear validation failure", err)
	}
}

func TestNonDefaultNamespaceSubmissionAndAuthorizationStayAligned(t *testing.T) {
	const user = "system:serviceaccount:agents:worker"
	kube := fake.NewSimpleClientset()
	var gotSAR *authorizationv1.SubjectAccessReview
	kube.Fake.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{RequesterTokenAudience},
			User:          authv1.UserInfo{Username: user},
		}}, nil
	})
	kube.Fake.PrependReactor("create", "subjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		gotSAR = action.(ktesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview).DeepCopy()
		return true, &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})

	objects := ctrlfake.NewClientBuilder().WithScheme(scheme).Build()
	api := &APIServer{
		ControllerNamespace: testControllerNamespace,
		Client:              objects,
		TokenReviewer:       &TokenReviewer{cs: kube},
		Authorizer:          &RequesterAuthorizer{cs: kube, ControllerNamespace: testControllerNamespace},
	}
	req := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(`{"reason":"test","command":"kubectl get pods"}`))
	req.Header.Set("Authorization", "Bearer token")
	resp := httptest.NewRecorder()
	api.createRequestHandler(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
	}
	if gotSAR == nil || gotSAR.Spec.ResourceAttributes == nil || gotSAR.Spec.ResourceAttributes.Namespace != testControllerNamespace {
		t.Fatalf("SAR namespace=%v, want %q", gotSAR, testControllerNamespace)
	}
	var list SudoRequestList
	if err := objects.List(context.Background(), &list, client.InNamespace(testControllerNamespace)); err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Namespace != testControllerNamespace {
		t.Fatalf("requests=%+v, want one in %q", list.Items, testControllerNamespace)
	}
	if _, err := api.findByUID(context.Background(), list.Items[0].UID); err != nil {
		t.Fatalf("findByUID in configured namespace: %v", err)
	}
}

func TestNonDefaultNamespaceDrivesExecutorReviewAndOutput(t *testing.T) {
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "request", Namespace: testControllerNamespace, UID: types.UID("request-uid")},
		Spec:       SudoRequestSpec{Requester: "requester", Reason: "test", Command: "true"},
	}
	if err := validateSpecExtras(sr, testControllerNamespace); err != nil {
		t.Fatalf("validate controller-namespace request: %v", err)
	}
	if got := executorNamespace(sr, testControllerNamespace); got != testControllerNamespace {
		t.Fatalf("executor namespace=%q", got)
	}
	if !clusterAdminEnabled(sr, testControllerNamespace) {
		t.Fatal("cluster-admin default disabled in configured controller namespace")
	}
	job := buildExecutorJob(sr, testControllerNamespace, "sudo-exec-test", &podExtras{}, testControllerNamespace)
	if job.Namespace != testControllerNamespace || job.Spec.Template.Spec.ServiceAccountName != ExecutorSAName || len(job.OwnerReferences) != 1 {
		t.Fatalf("executor Job not bound to configured namespace: %+v", job)
	}
	view := newSpecExtrasView(sr, false, testControllerNamespace)
	if view.Namespace != testControllerNamespace || !view.ClusterAdmin {
		t.Fatalf("review view=%+v", view)
	}

	controller := true
	job.UID = "job-uid"
	job.Status.Succeeded = 1
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sudo-exec-test-pod", Namespace: testControllerNamespace,
			Labels: map[string]string{"job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller,
			}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "executor", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}
	objects := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).Build()
	r := &SudoRequestReconciler{
		ControllerNamespace: testControllerNamespace,
		Client:              objects,
		APIReader:           objects,
		PodLogs: func(context.Context, string, string, string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("ok")), nil
		},
	}
	result, err := r.captureJobOutput(context.Background(), sr, &job)
	if err != nil {
		t.Fatalf("capture output: %v", err)
	}
	var output corev1.Secret
	if err := objects.Get(context.Background(), client.ObjectKey{Namespace: testControllerNamespace, Name: result.SecretRef}, &output); err != nil {
		t.Fatalf("output Secret not in configured namespace: %v", err)
	}

	api := &APIServer{ControllerNamespace: testControllerNamespace, Client: objects}
	sr.Status.OutputSecretRef = result.SecretRef
	w := httptest.NewRecorder()
	api.serveOutput(w, httptest.NewRequest(http.MethodGet, "/", nil), sr)
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("serveOutput status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestNonDefaultNamespaceStoresApprovalTokenWithRequest(t *testing.T) {
	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: testControllerNamespace, UID: "pending-uid", CreationTimestamp: metav1.Now()},
		Spec:       SudoRequestSpec{Requester: "requester", Reason: "test", Command: "kubectl get pods"},
	}
	objects := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&SudoRequest{}).WithObjects(sr).Build()
	r := &SudoRequestReconciler{
		ControllerNamespace: testControllerNamespace,
		Client:              objects,
		APIReader:           objects,
		Scheme:              testScheme(t),
		Pushover:            NewPushoverClient("token", "user"),
		Broadcaster:         NewBroadcaster(),
		Recorder:            record.NewFakeRecorder(5),
		PublicBaseURL:       "https://sudo.example",
	}
	if _, err := r.handleNew(context.Background(), sr.DeepCopy()); err != nil {
		t.Fatalf("handleNew: %v", err)
	}
	var pending SudoRequest
	if err := objects.Get(context.Background(), client.ObjectKeyFromObject(sr), &pending); err != nil {
		t.Fatalf("get pending request: %v", err)
	}
	var token corev1.Secret
	if err := objects.Get(context.Background(), client.ObjectKey{Namespace: testControllerNamespace, Name: pending.Status.ApprovalTokenSecretName}, &token); err != nil {
		t.Fatalf("approval token Secret not in configured namespace: %v", err)
	}
	if _, err := r.findByUID(context.Background(), sr.UID); err != nil {
		t.Fatalf("reconciler findByUID in configured namespace: %v", err)
	}
}

func TestGarbageCollectorSweepsOnlyConfiguredNamespace(t *testing.T) {
	expired := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	old := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	configuredSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "configured", Namespace: testControllerNamespace,
		Labels: map[string]string{"app": "sudo-service", "expires-at": expired},
	}}
	defaultSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "default", Namespace: DefaultControllerNamespace,
		Labels: map[string]string{"app": "sudo-service", "expires-at": expired},
	}}
	configuredRequest := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "configured", Namespace: testControllerNamespace, CreationTimestamp: old},
		Status:     SudoRequestStatus{Phase: PhasePending},
	}
	defaultRequest := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: DefaultControllerNamespace, CreationTimestamp: old},
		Status:     SudoRequestStatus{Phase: PhasePending},
	}
	objects := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&SudoRequest{}).
		WithObjects(configuredSecret, defaultSecret, configuredRequest, defaultRequest).Build()
	gc := &GarbageCollector{Client: objects, ControllerNamespace: testControllerNamespace, Broadcaster: NewBroadcaster()}
	if err := gc.sweepSecrets(context.Background()); err != nil {
		t.Fatalf("sweepSecrets: %v", err)
	}
	if err := objects.Get(context.Background(), client.ObjectKeyFromObject(configuredSecret), &corev1.Secret{}); err == nil {
		t.Fatal("expired Secret in configured namespace was not deleted")
	}
	if err := objects.Get(context.Background(), client.ObjectKeyFromObject(defaultSecret), &corev1.Secret{}); err != nil {
		t.Fatalf("GC crossed into default namespace: %v", err)
	}
	if err := gc.expirePending(context.Background()); err != nil {
		t.Fatalf("expirePending: %v", err)
	}
	var gotConfigured, gotDefault SudoRequest
	if err := objects.Get(context.Background(), client.ObjectKeyFromObject(configuredRequest), &gotConfigured); err != nil {
		t.Fatalf("get configured request: %v", err)
	}
	if err := objects.Get(context.Background(), client.ObjectKeyFromObject(defaultRequest), &gotDefault); err != nil {
		t.Fatalf("get default request: %v", err)
	}
	if gotConfigured.Status.Phase != PhaseExpired || gotDefault.Status.Phase != PhasePending {
		t.Fatalf("GC request phases configured=%q default=%q", gotConfigured.Status.Phase, gotDefault.Status.Phase)
	}
}
