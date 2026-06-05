package main

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	GroupName    = "sudo.andrewgarrett.dev"
	GroupVersion = "v1alpha1"

	PhasePending  = "Pending"
	PhaseApproved = "Approved"
	PhaseDenied   = "Denied"
	PhaseExecuted = "Executed"
	PhaseFailed   = "Failed"
	PhaseExpired  = "Expired"

	// Namespace and resource constants.
	ControllerNamespace = "sudo-service"
	ControllerSAName    = "sudo-service-controller-sa"
	ExecutorSAName      = "sudo-service-executor-sa"

	AppLabelKey             = "app"
	RoleLabelKey            = "role"
	ControllerAppLabelValue = "sudo-service"
	ExecutorAppLabelValue   = "sudo-service-executor"
	ExecutorRoleLabelValue  = "executor"

	// Default image used when SudoRequest.spec.image is empty. The executor
	// invokes `sh -c <command>`, so the image needs both a POSIX shell and
	// kubectl. alpine/k8s bundles both for the right minor; rancher/kubectl
	// would be leaner but is distroless and has no /bin/sh.
	DefaultExecutorImage = "alpine/k8s:1.35.5"

	// Audience required on requester service account tokens for HTTP API auth.
	RequesterTokenAudience = "sudo-service.andrewgarrett.dev"

	// Token TTLs.
	ApprovalTokenTTL    = 15 * 60 // 15 minutes (seconds)
	PendingRequestTTL   = 60 * 60 // 1 hour (seconds)
	OutputSecretTTL     = 60 * 60 // 1 hour (seconds)
	ExecutorJobTTL      = 60 * 60 // 1 hour (seconds)
	DefaultPostApproval = 600     // ttlSecondsAfterApproval default
)

var GroupVersionResource = schema.GroupVersionResource{
	Group:    GroupName,
	Version:  GroupVersion,
	Resource: "sudorequests",
}

// SudoRequestSpec is the desired state described by the requester.
type SudoRequestSpec struct {
	// Requester is the logical caller identity, e.g. system:serviceaccount:k8s-agent:k8s-agent-sa.
	// Enforced server-side by ValidatingAdmissionPolicy against request.userInfo.username.
	Requester string `json:"requester"`

	// Reason is free-text shown to the human reviewer on the Pushbullet push and approve page.
	Reason string `json:"reason"`

	// Command is the exact argv to run, e.g. "kubectl rollout restart deployment/foo -n bar".
	Command string `json:"command"`

	// Image is the container image the executor Job runs. Defaults to DefaultExecutorImage.
	// The human reviewer is the trust boundary: the approve page shows the image
	// prominently so the human notices suspicious image+command pairings.
	Image string `json:"image,omitempty"`

	// TTLSecondsAfterApproval defaults to 600 seconds.
	TTLSecondsAfterApproval *int32 `json:"ttlSecondsAfterApproval,omitempty"`
}

// SudoRequestStatus is owned by the controller.
type SudoRequestStatus struct {
	Phase string `json:"phase,omitempty"`

	ApprovedBy   string       `json:"approvedBy,omitempty"`
	ApprovedAt   *metav1.Time `json:"approvedAt,omitempty"`
	DeniedBy     string       `json:"deniedBy,omitempty"`
	DeniedAt     *metav1.Time `json:"deniedAt,omitempty"`
	DenialReason string       `json:"denialReason,omitempty"`

	ExitCode        *int32 `json:"exitCode,omitempty"`
	OutputSecretRef string `json:"outputSecretRef,omitempty"`

	// Summary is an optional, AI-generated human-readable review aid for the
	// command, produced once when the request enters Pending and cached here
	// (the object is the natural machine-readable cache). Empty when the
	// summarizer is disabled or generation failed; it is never a substitute for
	// the human reviewer reading the command.
	Summary string `json:"summary,omitempty"`

	// ExecutorJobName is the name of the Job that was (or is being) run for
	// this request. Recorded as soon as the Job is created so that, if the
	// Job is GC'd by ttlSecondsAfterFinished before the controller observes
	// its completion, the next reconcile fails the request instead of
	// silently recreating the Job and re-running the privileged command.
	ExecutorJobName string `json:"executorJobName,omitempty"`

	// PushoverRequestID is the Pushover API's per-request UUID, for audit-trail
	// correlation with the Pushover dashboard.
	PushoverRequestID string `json:"pushoverRequestID,omitempty"`

	// ApprovalTokenHash is the SHA-256 hex digest of the one-time approval token.
	// The plaintext token is only ever sent in the Pushbullet push URL.
	ApprovalTokenHash      string       `json:"approvalTokenHash,omitempty"`
	ApprovalTokenExpiresAt *metav1.Time `json:"approvalTokenExpiresAt,omitempty"`
}

// SudoRequest is the CRD root object.
type SudoRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SudoRequestSpec   `json:"spec,omitempty"`
	Status SudoRequestStatus `json:"status,omitempty"`
}

// SudoRequestList is the list type for SudoRequest.
type SudoRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SudoRequest `json:"items"`
}

// DeepCopyInto / DeepCopyObject — hand-written for runtime.Object compatibility.
func (in *SudoRequest) DeepCopyInto(out *SudoRequest) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *SudoRequest) DeepCopy() *SudoRequest {
	if in == nil {
		return nil
	}
	out := new(SudoRequest)
	in.DeepCopyInto(out)
	return out
}

func (in *SudoRequest) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *SudoRequestList) DeepCopyInto(out *SudoRequestList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]SudoRequest, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *SudoRequestList) DeepCopy() *SudoRequestList {
	if in == nil {
		return nil
	}
	out := new(SudoRequestList)
	in.DeepCopyInto(out)
	return out
}

func (in *SudoRequestList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *SudoRequestSpec) DeepCopyInto(out *SudoRequestSpec) {
	*out = *in
	if in.TTLSecondsAfterApproval != nil {
		v := *in.TTLSecondsAfterApproval
		out.TTLSecondsAfterApproval = &v
	}
}

func (in *SudoRequestStatus) DeepCopyInto(out *SudoRequestStatus) {
	*out = *in
	if in.ApprovedAt != nil {
		t := *in.ApprovedAt
		out.ApprovedAt = &t
	}
	if in.DeniedAt != nil {
		t := *in.DeniedAt
		out.DeniedAt = &t
	}
	if in.ExitCode != nil {
		v := *in.ExitCode
		out.ExitCode = &v
	}
	if in.ApprovalTokenExpiresAt != nil {
		t := *in.ApprovalTokenExpiresAt
		out.ApprovalTokenExpiresAt = &t
	}
}

// SchemeBuilder registers our types with a runtime.Scheme so controller-runtime
// can codec-encode/decode them.
var (
	SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: GroupVersion}
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion, &SudoRequest{}, &SudoRequestList{})
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
