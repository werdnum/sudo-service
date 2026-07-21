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

	NotificationPending   = "Pending"
	NotificationDelivered = "Delivered"

	PermissionAssessmentPending   = "Pending"
	PermissionAssessmentGenerated = "Generated"
	PermissionAssessmentFailed    = "Failed"

	// Namespace and resource constants.
	ControllerNamespace = "sudo-service"
	ControllerSAName    = "sudo-service-controller-sa"
	ExecutorSAName      = "sudo-service-executor-sa"

	AppLabelKey             = "app"
	RoleLabelKey            = "role"
	ControllerAppLabelValue = "sudo-service"
	ExecutorAppLabelValue   = "sudo-service-executor"
	ExecutorRoleLabelValue  = "executor"

	// Audience required on requester service account tokens for HTTP API auth.
	RequesterTokenAudience = "sudo-service.andrewgarrett.dev"

	// Token TTLs.
	PendingRequestTTL = 60 * 60 // 1 hour (seconds)
	// Approval links remain usable for the same period as the Pending request.
	// The link is not an authorization credential: a verified admin identity is
	// still required before the request can be viewed or acted on.
	ApprovalTokenTTL    = PendingRequestTTL
	OutputSecretTTL     = 60 * 60 // 1 hour (seconds)
	ExecutorJobTTL      = 60 * 60 // 1 hour (seconds)
	DefaultPostApproval = 3600    // ttlSecondsAfterApproval default (1 hour)

	// ExecutorStartDeadline bounds how long an approved request waits for its
	// executor pod to start running. A pod stuck in ContainerCreating (an
	// unsatisfiable mount, an unschedulable pod, an image that won't pull) never
	// increments the Job's succeeded/failed counts, so without this the request
	// would sit in Approved forever. Generous, since it includes image-pull time.
	ExecutorStartDeadline = 600 // seconds (10 minutes)

	// ExecutorJobTTLFloor is the minimum lifetime of a finished executor Job,
	// independent of the requester's (output-retention) ttlSecondsAfterApproval.
	// The reconciler polls the Job to capture output; if the requester asks for a
	// tiny TTL (e.g. 0), kube-controller-manager could delete the finished Job
	// before we read its pod logs, losing the output and the exit code. The floor
	// guarantees a capture window. It stays <= the executor VAP's 3600s guard.
	ExecutorJobTTLFloor = 120 // seconds

	// MaxStdinBytes caps spec.stdin so it always fits in the Secret that carries it.
	// Kubernetes limits a Secret's data to 1 MiB total; stdin between that and the
	// (~1.5 MiB) object limit would pass CRD storage and human review, then fail at
	// Secret creation *after* approval. Rejecting oversized stdin at submission keeps
	// the failure in front of the reviewer. Headroom is left for the data key name
	// and any other Secret fields.
	MaxStdinBytes = 1024*1024 - 4096

	// MaxOutputBytes caps the bytes retained in the output Secret. Kubernetes limits
	// a Secret's data to 1 MiB; reserve headroom for the data key and object metadata.
	// The complete log stream is still counted and hashed before this prefix is kept.
	MaxOutputBytes = 1024*1024 - 4096

	OutputCaptureComplete   = "Complete"
	OutputCaptureTruncated  = "Truncated"
	OutputCaptureFailed     = "Failed"
	OutputDeliveryAvailable = "Available"
	OutputDeliveryFailed    = "Failed"
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

	// Profile is a friendly alias resolved by the controller to a digest-pinned
	// image and machine-readable shell/capability metadata. It is mutually
	// exclusive with Image. Empty Profile+Image selects DefaultExecutorProfile.
	Profile string `json:"profile,omitempty"`

	// TTLSecondsAfterApproval defaults to 3600 seconds.
	TTLSecondsAfterApproval *int32 `json:"ttlSecondsAfterApproval,omitempty"`

	// Execution selects a controller-owned, bounded lifecycle. Empty preserves
	// the standard foreground policy. managedJob is the same UID-correlated
	// executor Job with a larger curated resource class and explicit deadline;
	// it does not permit the command to create an untracked child workload.
	Execution SudoRequestExecution `json:"execution,omitempty"`

	// Namespace is the namespace the executor Job runs in. Defaults to the
	// controller namespace (sudo-service). Targeting another namespace lets the
	// command mount that namespace's Secrets/PVCs as files (pods cannot mount
	// cross-namespace) — but such a Job runs under that namespace's default
	// ServiceAccount with no API privileges, so cluster-admin is only available
	// in the controller namespace. See validateSpecExtras.
	Namespace string `json:"namespace,omitempty"`

	// Stdin, if set, is fed to the command's standard input. It removes the need
	// to smuggle a multi-line payload (a manifest piped to `kubectl apply -f -`,
	// a heredoc, ...) through the single-string Command field and the layers of
	// shell quoting that implies. The bytes are materialised into a short-TTL
	// Secret mounted into the executor pod, never interpolated into the shell.
	Stdin string `json:"stdin,omitempty"`

	// Env / EnvFrom / Volumes / VolumeMounts / InitContainers are the widened pod
	// fields. They are stored as raw JSON (runtime.RawExtension) rather than the
	// concrete corev1 types on purpose: the CRD schema uses
	// x-kubernetes-preserve-unknown-fields, so a malformed-but-schema-valid object
	// (e.g. an integer env[].name) written directly to the CRD must NOT make the
	// controller's typed List decode fail — that would wedge the informer for every
	// request. Each item is decoded per-object in decodePodExtras; a decode failure
	// becomes a per-request Denied (via validateSpecExtras), not a cache-wide DoS.
	//
	// Env / EnvFrom: extra environment for the executor container (e.g. a secretRef
	// for credentials). Volumes / VolumeMounts: read Secrets/ConfigMaps/PVCs and
	// scratch space as files; sources are narrowed to a reviewable allowlist in
	// validateSpecExtras (hostPath/projected rejected). InitContainers run before
	// the executor with a controller-stamped securityContext and resource bounds.
	Env            []runtime.RawExtension `json:"env,omitempty"`
	EnvFrom        []runtime.RawExtension `json:"envFrom,omitempty"`
	Volumes        []runtime.RawExtension `json:"volumes,omitempty"`
	VolumeMounts   []runtime.RawExtension `json:"volumeMounts,omitempty"`
	InitContainers []runtime.RawExtension `json:"initContainers,omitempty"`

	// ImagePullSecrets names registry-credential Secrets (in the executor's
	// namespace) the kubelet uses to pull spec.image and any initContainer images
	// from a private registry. Stored as raw JSON like the other widened fields
	// (each item is a corev1.LocalObjectReference: {"name": ...}) for the same
	// decode-safety reason. Unlike a mounted or env-exposed Secret these are never
	// surfaced inside the container — the kubelet consumes them purely for registry
	// auth — so they grant the command no capability and don't need the SA-token
	// rejection that volume/env Secret references do; the names are still rendered
	// on the approve page for the reviewer.
	ImagePullSecrets []runtime.RawExtension `json:"imagePullSecrets,omitempty"`

	// Privileges holds the explicit, approval-surfaced capability toggles. Each
	// flag widens what the executor pod may do; the human reviewer sees them on
	// the approve page. Only ClusterAdmin is wired up today — privileged, runAsRoot,
	// hostPath, etc. are the planned extensions.
	Privileges SudoRequestPrivileges `json:"privileges,omitempty"`
}

// SudoRequestExecution contains only curated lifecycle choices. Arbitrary
// resource requests/limits would make review inconsistent and could starve the
// cluster, so callers select a controller-owned resource class instead.
type SudoRequestExecution struct {
	Mode                  string `json:"mode,omitempty"`
	ResourceClass         string `json:"resourceClass,omitempty"`
	ActiveDeadlineSeconds *int32 `json:"activeDeadlineSeconds,omitempty"`
}

// SudoRequestPrivileges is the set of explicit capability toggles a request may
// flip. They default to a safe value and are rendered individually on the
// approve page so the reviewer can see exactly what power is being handed over.
type SudoRequestPrivileges struct {
	// ClusterAdmin runs the executor under the cluster-admin-bound executor SA.
	// It is only available when the Job runs in the controller namespace (that is
	// where the cluster-admin SA lives), and defaults to true there to preserve
	// the historical "every request is fully privileged" behaviour. When the Job
	// targets another namespace it is unavailable (nil/false) and the request runs
	// under that namespace's unprivileged default SA. nil means "use the default
	// for the chosen namespace".
	ClusterAdmin *bool `json:"clusterAdmin,omitempty"`
}

// SudoRequestStatus is owned by the controller.
type SudoRequestStatus struct {
	Phase string `json:"phase,omitempty"`

	ApprovedBy   string       `json:"approvedBy,omitempty"`
	ApprovedAt   *metav1.Time `json:"approvedAt,omitempty"`
	DeniedBy     string       `json:"deniedBy,omitempty"`
	DeniedAt     *metav1.Time `json:"deniedAt,omitempty"`
	DenialReason string       `json:"denialReason,omitempty"`

	// FailureReason explains a Failed request whose command outcome is unknown,
	// e.g. the executor Job disappeared before completion. A command that ran is
	// conveyed independently by ExitCode even if its output could not be captured.
	FailureReason string `json:"failureReason,omitempty"`

	// ResolvedProfile and ResolvedImage are controller-owned approval state. The
	// exact digest reviewed by the human is the exact image later executed.
	ResolvedProfile   string   `json:"resolvedProfile,omitempty"`
	ResolvedImage     string   `json:"resolvedImage,omitempty"`
	PreflightWarnings []string `json:"preflightWarnings,omitempty"`

	ExitCode        *int32 `json:"exitCode,omitempty"`
	OutputSecretRef string `json:"outputSecretRef,omitempty"`

	// OutputCaptureState describes reading and bounding the terminated container's
	// logs: Complete, Truncated, or Failed. OutputDeliveryState separately records
	// whether the retained bytes were made Available in OutputSecretRef or Secret
	// creation Failed. Empty fields identify records written by older controllers.
	OutputCaptureState  string `json:"outputCaptureState,omitempty"`
	OutputDeliveryState string `json:"outputDeliveryState,omitempty"`
	OutputFailureReason string `json:"outputFailureReason,omitempty"`
	OutputTotalBytes    *int64 `json:"outputTotalBytes,omitempty"`
	OutputRetainedBytes *int64 `json:"outputRetainedBytes,omitempty"`
	// OutputSHA256 is the lowercase SHA-256 digest of the complete observed stream,
	// including bytes that were not retained after truncation.
	OutputSHA256 string `json:"outputSHA256,omitempty"`

	// StdinSecretName is the controller-minted, unguessable name of the Secret
	// holding spec.stdin. It is generated randomly (not derived from the request
	// UID, which the requester learns at create time) and recorded before the
	// executor Job is created, so a requester who can create Secrets in the target
	// namespace cannot pre-create it to swap in an unapproved payload. Empty when
	// the request has no stdin.
	StdinSecretName string `json:"stdinSecretName,omitempty"`

	// Summary is an optional, AI-generated human-readable review aid for the
	// command, produced once when the request enters Pending and cached here
	// (the object is the natural machine-readable cache). Empty when the
	// summarizer is disabled or generation failed; it is never a substitute for
	// the human reviewer reading the command.
	Summary string `json:"summary,omitempty"`

	// PermissionAssessment is the optional, structured review aid generated once
	// when the request enters Pending. It answers only what pressing Approve
	// permits; the command and effective Pod spec remain the ground truth. The
	// legacy Summary field is retained for records written by older controllers.
	PermissionAssessment *PermissionAssessment `json:"permissionAssessment,omitempty"`
	// PermissionAssessmentState separates optional model generation from the
	// approval and notification lifecycles. Empty identifies older records.
	PermissionAssessmentState string `json:"permissionAssessmentState,omitempty"`

	// ExecutorJobName is the controller-minted, unguessable name of the executor
	// Job. Like StdinSecretName it is a random token (not derived from the request
	// UID), recorded before the Job is created so a requester who can create Jobs
	// in the target namespace can't pre-create a predictable sudo-exec-<uid> Job
	// for the controller to adopt as the (unapproved) workload.
	ExecutorJobName string `json:"executorJobName,omitempty"`

	// ExecutorJobUID is the UID of the Job the controller created, recorded after
	// creation. It distinguishes "not created yet" (empty) from "created then GC'd"
	// (set, but the Job is now gone → fail rather than replay), and lets the
	// reconciler reject a Job that was deleted and replaced at the same name.
	ExecutorJobUID string `json:"executorJobUID,omitempty"`

	// ExecutorJobLifecycle is the durable managed-job state machine. Result and
	// output fields are persisted before CleanupRequested; terminal request phase
	// is not recorded until foreground deletion reaches Cleaned.
	ExecutorJobLifecycle  string       `json:"executorJobLifecycle,omitempty"`
	ExecutorJobStartedAt  *metav1.Time `json:"executorJobStartedAt,omitempty"`
	ExecutorJobFinishedAt *metav1.Time `json:"executorJobFinishedAt,omitempty"`

	// PushoverRequestID is the Pushover API's per-request UUID, for audit-trail
	// correlation with the Pushover dashboard.
	PushoverRequestID string `json:"pushoverRequestID,omitempty"`

	// Notification delivery is tracked separately from the approval phase so a
	// failed external call can retry without reminting approval state.
	NotificationState         string       `json:"notificationState,omitempty"`
	NotificationAttempts      int32        `json:"notificationAttempts,omitempty"`
	NotificationLastAttemptAt *metav1.Time `json:"notificationLastAttemptAt,omitempty"`
	NotificationNextAttemptAt *metav1.Time `json:"notificationNextAttemptAt,omitempty"`
	NotificationDeliveredAt   *metav1.Time `json:"notificationDeliveredAt,omitempty"`
	NotificationLastError     string       `json:"notificationLastError,omitempty"`

	// ApprovalTokenHash is the SHA-256 hex digest of the one-time approval token.
	// The plaintext token is stored in a controller-owned Secret and sent only
	// in the Pushover URL.
	ApprovalTokenHash      string       `json:"approvalTokenHash,omitempty"`
	ApprovalTokenExpiresAt *metav1.Time `json:"approvalTokenExpiresAt,omitempty"`
	// ApprovalTokenSecretName points at the controller-owned Secret containing
	// the plaintext token needed to retry the same notification link.
	ApprovalTokenSecretName string `json:"approvalTokenSecretName,omitempty"`
}

// PermissionEffect is a closed, factual vocabulary used for compact badges on
// approval surfaces. Values are validated after every model response; arbitrary
// model-produced labels are never persisted or rendered.
type PermissionEffect string

const (
	EffectReadOnly         PermissionEffect = "READ_ONLY"
	EffectChangesCluster   PermissionEffect = "CHANGES_CLUSTER"
	EffectCreatesResource  PermissionEffect = "CREATES_RESOURCE"
	EffectRestartsWorkload PermissionEffect = "RESTARTS_WORKLOAD"
	EffectDeletesResource  PermissionEffect = "DELETES_RESOURCE"
	EffectExportsData      PermissionEffect = "EXPORTS_DATA"
	EffectReadsSecret      PermissionEffect = "READS_SECRET"
	EffectUsesCredentials  PermissionEffect = "USES_CREDENTIALS"
	EffectExternalEgress   PermissionEffect = "EXTERNAL_EGRESS"
	EffectHostAccess       PermissionEffect = "HOST_ACCESS"
	EffectSecurityConfig   PermissionEffect = "SECURITY_CONFIG"
	EffectBroadScope       PermissionEffect = "BROAD_SCOPE"
	EffectCleanupIncluded  PermissionEffect = "CLEANUP_INCLUDED"
	EffectNonDefaultImage  PermissionEffect = "NON_DEFAULT_IMAGE"
)

// PermissionAssessment is versioned independently of the CRD so prompt and
// schema changes remain attributable in the audit record.
type PermissionAssessment struct {
	Request       string             `json:"request"`
	Effects       []PermissionEffect `json:"effects"`
	SchemaVersion string             `json:"schemaVersion"`
	PromptVersion string             `json:"promptVersion"`
	Model         string             `json:"model"`
	GeneratedAt   metav1.Time        `json:"generatedAt"`
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
	if in.Execution.ActiveDeadlineSeconds != nil {
		v := *in.Execution.ActiveDeadlineSeconds
		out.Execution.ActiveDeadlineSeconds = &v
	}
	copyRaw := func(in []runtime.RawExtension) []runtime.RawExtension {
		if in == nil {
			return nil
		}
		out := make([]runtime.RawExtension, len(in))
		for i := range in {
			in[i].DeepCopyInto(&out[i])
		}
		return out
	}
	out.Env = copyRaw(in.Env)
	out.EnvFrom = copyRaw(in.EnvFrom)
	out.Volumes = copyRaw(in.Volumes)
	out.VolumeMounts = copyRaw(in.VolumeMounts)
	out.InitContainers = copyRaw(in.InitContainers)
	out.ImagePullSecrets = copyRaw(in.ImagePullSecrets)
	if in.Privileges.ClusterAdmin != nil {
		v := *in.Privileges.ClusterAdmin
		out.Privileges.ClusterAdmin = &v
	}
}

func (in *SudoRequestStatus) DeepCopyInto(out *SudoRequestStatus) {
	*out = *in
	if in.PreflightWarnings != nil {
		out.PreflightWarnings = append([]string(nil), in.PreflightWarnings...)
	}
	if in.PermissionAssessment != nil {
		out.PermissionAssessment = new(PermissionAssessment)
		*out.PermissionAssessment = *in.PermissionAssessment
		if in.PermissionAssessment.Effects != nil {
			out.PermissionAssessment.Effects = append([]PermissionEffect(nil), in.PermissionAssessment.Effects...)
		}
	}
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
	if in.ExecutorJobStartedAt != nil {
		t := *in.ExecutorJobStartedAt
		out.ExecutorJobStartedAt = &t
	}
	if in.ExecutorJobFinishedAt != nil {
		t := *in.ExecutorJobFinishedAt
		out.ExecutorJobFinishedAt = &t
	}
	if in.OutputTotalBytes != nil {
		v := *in.OutputTotalBytes
		out.OutputTotalBytes = &v
	}
	if in.OutputRetainedBytes != nil {
		v := *in.OutputRetainedBytes
		out.OutputRetainedBytes = &v
	}
	if in.ApprovalTokenExpiresAt != nil {
		t := *in.ApprovalTokenExpiresAt
		out.ApprovalTokenExpiresAt = &t
	}
	if in.NotificationLastAttemptAt != nil {
		t := *in.NotificationLastAttemptAt
		out.NotificationLastAttemptAt = &t
	}
	if in.NotificationNextAttemptAt != nil {
		t := *in.NotificationNextAttemptAt
		out.NotificationNextAttemptAt = &t
	}
	if in.NotificationDeliveredAt != nil {
		t := *in.NotificationDeliveredAt
		out.NotificationDeliveredAt = &t
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
