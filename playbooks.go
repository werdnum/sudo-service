package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	terminalJobCleanupPlaybook = "job.cleanup-terminal/v1"
	cronJobTriggerPlaybook     = "cronjob.trigger/v1"
	argocdRefreshPlaybook      = "argocd.refresh/v1"

	playbookRequestUIDAnnotation = "sudo.andrewgarrett.dev/request-uid"
)

// PlaybookConfig is intentionally a closed schema. Adding another
// auto-approved action requires code, validation and tests rather than a broad
// command prefix in configuration.
type PlaybookConfig struct {
	TerminalJobCleanup []TerminalJobCleanupRule `json:"terminalJobCleanup,omitempty"`
	CronJobTrigger     []CronJobTriggerRule     `json:"cronJobTrigger,omitempty"`
	ArgoCDRefresh      []ArgoCDRefreshRule      `json:"argocdRefresh,omitempty"`
}

type TerminalJobCleanupRule struct {
	Requester              string   `json:"requester"`
	Namespace              string   `json:"namespace"`
	CronJobs               []string `json:"cronJobs,omitempty"`
	StandaloneNamePrefixes []string `json:"standaloneNamePrefixes,omitempty"`
}

type CronJobTriggerRule struct {
	Requester string   `json:"requester"`
	Namespace string   `json:"namespace"`
	CronJobs  []string `json:"cronJobs"`
}

type ArgoCDRefreshRule struct {
	Requester            string   `json:"requester"`
	Namespace            string   `json:"namespace"`
	Applications         []string `json:"applications,omitempty"`
	AllowAllApplications bool     `json:"allowAllApplications,omitempty"`
}

type preparedPlaybook struct {
	targetUID    types.UID
	targetAbsent bool
}

func LoadPlaybookConfig(path string) (*PlaybookConfig, error) {
	config := &PlaybookConfig{}
	if path == "" {
		return config, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read playbook config %s: %w", path, err)
	}
	if err := yaml.UnmarshalStrict(data, config); err != nil {
		return nil, fmt.Errorf("unmarshal playbook config %s: %w", path, err)
	}
	for i := range config.TerminalJobCleanup {
		if err := validateTerminalJobCleanupRule(config.TerminalJobCleanup[i]); err != nil {
			return nil, fmt.Errorf("terminalJobCleanup[%d]: %w", i, err)
		}
	}
	for i := range config.CronJobTrigger {
		if err := validateCronJobTriggerRule(config.CronJobTrigger[i]); err != nil {
			return nil, fmt.Errorf("cronJobTrigger[%d]: %w", i, err)
		}
	}
	for i := range config.ArgoCDRefresh {
		if err := validateArgoCDRefreshRule(config.ArgoCDRefresh[i]); err != nil {
			return nil, fmt.Errorf("argocdRefresh[%d]: %w", i, err)
		}
	}
	return config, nil
}

func validateTerminalJobCleanupRule(rule TerminalJobCleanupRule) error {
	if rule.Requester == "" {
		return errors.New("requester is required")
	}
	if problems := k8svalidation.IsDNS1123Label(rule.Namespace); len(problems) > 0 {
		return fmt.Errorf("namespace %q is invalid: %s", rule.Namespace, strings.Join(problems, "; "))
	}
	if len(rule.CronJobs) == 0 && len(rule.StandaloneNamePrefixes) == 0 {
		return errors.New("at least one cronJobs or standaloneNamePrefixes entry is required")
	}
	for _, name := range rule.CronJobs {
		if problems := k8svalidation.IsDNS1123Subdomain(name); len(problems) > 0 {
			return fmt.Errorf("CronJob name %q is invalid: %s", name, strings.Join(problems, "; "))
		}
	}
	for _, prefix := range rule.StandaloneNamePrefixes {
		if prefix == "" || len(prefix) > 63 {
			return fmt.Errorf("standalone name prefix %q must contain 1-63 characters", prefix)
		}
		// Validate the prefix as the start of a DNS name without accepting shell
		// metacharacters or a catch-all empty value.
		if problems := k8svalidation.IsDNS1123Subdomain(strings.TrimSuffix(prefix, "-")); len(problems) > 0 {
			return fmt.Errorf("standalone name prefix %q is invalid: %s", prefix, strings.Join(problems, "; "))
		}
	}
	return nil
}

func validateCronJobTriggerRule(rule CronJobTriggerRule) error {
	if rule.Requester == "" {
		return errors.New("requester is required")
	}
	if problems := k8svalidation.IsDNS1123Label(rule.Namespace); len(problems) > 0 {
		return fmt.Errorf("namespace %q is invalid: %s", rule.Namespace, strings.Join(problems, "; "))
	}
	if len(rule.CronJobs) == 0 {
		return errors.New("at least one cronJobs entry is required")
	}
	for _, name := range rule.CronJobs {
		if problems := k8svalidation.IsDNS1123Subdomain(name); len(problems) > 0 {
			return fmt.Errorf("CronJob name %q is invalid: %s", name, strings.Join(problems, "; "))
		}
	}
	return nil
}

func validateArgoCDRefreshRule(rule ArgoCDRefreshRule) error {
	if rule.Requester == "" {
		return errors.New("requester is required")
	}
	if problems := k8svalidation.IsDNS1123Label(rule.Namespace); len(problems) > 0 {
		return fmt.Errorf("namespace %q is invalid: %s", rule.Namespace, strings.Join(problems, "; "))
	}
	if rule.AllowAllApplications && len(rule.Applications) > 0 {
		return errors.New("applications and allowAllApplications are mutually exclusive")
	}
	if !rule.AllowAllApplications && len(rule.Applications) == 0 {
		return errors.New("at least one applications entry or allowAllApplications is required")
	}
	for _, name := range rule.Applications {
		if problems := k8svalidation.IsDNS1123Subdomain(name); len(problems) > 0 {
			return fmt.Errorf("Application name %q is invalid: %s", name, strings.Join(problems, "; "))
		}
	}
	return nil
}

func sortedParameterKeys(params map[string]string) []string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func terminalJobCleanupParameters(sr *SudoRequest) (namespace, name string, err error) {
	if sr.Spec.Playbook == nil || sr.Spec.Playbook.Name != terminalJobCleanupPlaybook {
		return "", "", fmt.Errorf("unsupported playbook %q", playbookName(sr))
	}
	params := sr.Spec.Playbook.Parameters
	if len(params) != 2 {
		return "", "", fmt.Errorf("playbook %s requires exactly namespace and name parameters (got %v)", terminalJobCleanupPlaybook, sortedParameterKeys(params))
	}
	namespace, namespaceOK := params["namespace"]
	name, nameOK := params["name"]
	if !namespaceOK || !nameOK {
		return "", "", fmt.Errorf("playbook %s requires exactly namespace and name parameters", terminalJobCleanupPlaybook)
	}
	if problems := k8svalidation.IsDNS1123Label(namespace); len(problems) > 0 {
		return "", "", fmt.Errorf("playbook namespace %q is invalid: %s", namespace, strings.Join(problems, "; "))
	}
	if problems := k8svalidation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return "", "", fmt.Errorf("playbook Job name %q is invalid: %s", name, strings.Join(problems, "; "))
	}
	return namespace, name, nil
}

func cronJobTriggerParameters(sr *SudoRequest) (namespace, cronJob, name string, err error) {
	if sr.Spec.Playbook == nil || sr.Spec.Playbook.Name != cronJobTriggerPlaybook {
		return "", "", "", fmt.Errorf("unsupported playbook %q", playbookName(sr))
	}
	params := sr.Spec.Playbook.Parameters
	if len(params) != 3 {
		return "", "", "", fmt.Errorf("playbook %s requires exactly namespace, cronJob, and name parameters (got %v)", cronJobTriggerPlaybook, sortedParameterKeys(params))
	}
	namespace, namespaceOK := params["namespace"]
	cronJob, cronJobOK := params["cronJob"]
	name, nameOK := params["name"]
	if !namespaceOK || !cronJobOK || !nameOK {
		return "", "", "", fmt.Errorf("playbook %s requires exactly namespace, cronJob, and name parameters", cronJobTriggerPlaybook)
	}
	if problems := k8svalidation.IsDNS1123Label(namespace); len(problems) > 0 {
		return "", "", "", fmt.Errorf("playbook namespace %q is invalid: %s", namespace, strings.Join(problems, "; "))
	}
	if problems := k8svalidation.IsDNS1123Subdomain(cronJob); len(problems) > 0 {
		return "", "", "", fmt.Errorf("playbook CronJob name %q is invalid: %s", cronJob, strings.Join(problems, "; "))
	}
	// Job names are stamped onto Pod labels as batch.kubernetes.io/job-name, so
	// require a DNS-1123 label (<= 63 chars) rather than an arbitrary subdomain.
	if problems := k8svalidation.IsDNS1123Label(name); len(problems) > 0 {
		return "", "", "", fmt.Errorf("playbook Job name %q is invalid: %s", name, strings.Join(problems, "; "))
	}
	prefix := cronJob + "-manual-"
	if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
		return "", "", "", fmt.Errorf("playbook Job name %q must start with %q followed by a non-empty suffix", name, prefix)
	}
	return namespace, cronJob, name, nil
}

func argocdRefreshParameters(sr *SudoRequest) (namespace, name string, err error) {
	if sr.Spec.Playbook == nil || sr.Spec.Playbook.Name != argocdRefreshPlaybook {
		return "", "", fmt.Errorf("unsupported playbook %q", playbookName(sr))
	}
	params := sr.Spec.Playbook.Parameters
	if len(params) != 2 {
		return "", "", fmt.Errorf("playbook %s requires exactly namespace and name parameters (got %v)", argocdRefreshPlaybook, sortedParameterKeys(params))
	}
	namespace, namespaceOK := params["namespace"]
	name, nameOK := params["name"]
	if !namespaceOK || !nameOK {
		return "", "", fmt.Errorf("playbook %s requires exactly namespace and name parameters", argocdRefreshPlaybook)
	}
	if problems := k8svalidation.IsDNS1123Label(namespace); len(problems) > 0 {
		return "", "", fmt.Errorf("playbook namespace %q is invalid: %s", namespace, strings.Join(problems, "; "))
	}
	if problems := k8svalidation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return "", "", fmt.Errorf("playbook Application name %q is invalid: %s", name, strings.Join(problems, "; "))
	}
	return namespace, name, nil
}

func playbookName(sr *SudoRequest) string {
	if sr.Spec.Playbook == nil {
		return ""
	}
	return sr.Spec.Playbook.Name
}

func terminalJobCleanupCommand(namespace, name string) string {
	return fmt.Sprintf("kubectl delete job %s -n %s --ignore-not-found=true --wait=false", name, namespace)
}

func cronJobTriggerCommand(namespace, cronJob, name string) string {
	return fmt.Sprintf("kubectl create job %s --from=cronjob/%s -n %s", name, cronJob, namespace)
}

func argocdRefreshCommand(namespace, name string) string {
	return fmt.Sprintf("kubectl annotate application %s -n %s argocd.argoproj.io/refresh=hard --overwrite", name, namespace)
}

func validatePlaybookSpec(sr *SudoRequest) error {
	if sr.Spec.Playbook == nil {
		return nil
	}
	var canonical string
	switch sr.Spec.Playbook.Name {
	case terminalJobCleanupPlaybook:
		namespace, name, err := terminalJobCleanupParameters(sr)
		if err != nil {
			return err
		}
		canonical = terminalJobCleanupCommand(namespace, name)
	case cronJobTriggerPlaybook:
		namespace, cronJob, name, err := cronJobTriggerParameters(sr)
		if err != nil {
			return err
		}
		canonical = cronJobTriggerCommand(namespace, cronJob, name)
	case argocdRefreshPlaybook:
		namespace, name, err := argocdRefreshParameters(sr)
		if err != nil {
			return err
		}
		canonical = argocdRefreshCommand(namespace, name)
	default:
		return fmt.Errorf("unsupported playbook %q", sr.Spec.Playbook.Name)
	}
	if sr.Spec.Command != canonical {
		return fmt.Errorf("playbook command must equal canonical rendering %q", canonical)
	}
	if sr.Spec.Image != "" || sr.Spec.Profile != "" || hasPodSpecExtras(sr) {
		return errors.New("playbook requests cannot set image, profile, namespace, stdin, env, volumes, init containers, image pull secrets, or privilege overrides")
	}
	return nil
}

func hasPodSpecExtras(sr *SudoRequest) bool {
	return sr.Spec.Namespace != "" || sr.Spec.Stdin != "" || len(sr.Spec.Env) > 0 ||
		len(sr.Spec.EnvFrom) > 0 || len(sr.Spec.Volumes) > 0 || len(sr.Spec.VolumeMounts) > 0 ||
		len(sr.Spec.InitContainers) > 0 || len(sr.Spec.ImagePullSecrets) > 0 ||
		sr.Spec.Privileges.ClusterAdmin != nil
}

func jobTerminal(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func matchesTerminalJobRule(job *batchv1.Job, rule TerminalJobCleanupRule) bool {
	for _, owner := range job.OwnerReferences {
		if owner.Controller != nil && *owner.Controller {
			return owner.APIVersion == "batch/v1" && owner.Kind == "CronJob" && contains(rule.CronJobs, owner.Name)
		}
	}
	if len(job.OwnerReferences) != 0 {
		return false
	}
	for _, prefix := range rule.StandaloneNamePrefixes {
		if strings.HasPrefix(job.Name, prefix) {
			return true
		}
	}
	return false
}

func cronJobUsesReservedExecutorIdentity(cronJob *batchv1.CronJob) bool {
	return cronJob.Spec.JobTemplate.Spec.Template.Spec.ServiceAccountName == ExecutorSAName ||
		cronJob.Spec.JobTemplate.Labels[RoleLabelKey] == ExecutorRoleLabelValue ||
		cronJob.Spec.JobTemplate.Spec.Template.Labels[RoleLabelKey] == ExecutorRoleLabelValue
}

func jobBelongsToCronJobOrManualFamily(job *batchv1.Job, cronJobName string) bool {
	for _, owner := range job.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.APIVersion == "batch/v1" && owner.Kind == "CronJob" && owner.Name == cronJobName {
			return true
		}
	}
	return strings.HasPrefix(job.Name, cronJobName+"-manual-")
}

func newArgoCDApplicationObject() *unstructured.Unstructured {
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "Application",
	})
	return app
}

func (r *SudoRequestReconciler) prepareAutoApprovedPlaybook(ctx context.Context, sr *SudoRequest) (*preparedPlaybook, string, error) {
	if sr.Spec.Playbook == nil {
		return nil, "", nil
	}
	switch sr.Spec.Playbook.Name {
	case terminalJobCleanupPlaybook:
		return r.prepareTerminalJobCleanup(ctx, sr)
	case cronJobTriggerPlaybook:
		return r.prepareCronJobTrigger(ctx, sr)
	case argocdRefreshPlaybook:
		return r.prepareArgoCDRefresh(ctx, sr)
	default:
		return nil, "", fmt.Errorf("unsupported playbook %q", sr.Spec.Playbook.Name)
	}
}

func (r *SudoRequestReconciler) prepareTerminalJobCleanup(ctx context.Context, sr *SudoRequest) (*preparedPlaybook, string, error) {
	namespace, name, err := terminalJobCleanupParameters(sr)
	if err != nil {
		return nil, "", err
	}
	if r.Playbooks == nil {
		return nil, "playbook auto-approval is not configured", nil
	}
	var matching []TerminalJobCleanupRule
	for _, rule := range r.Playbooks.TerminalJobCleanup {
		if rule.Requester == sr.Spec.Requester && rule.Namespace == namespace {
			matching = append(matching, rule)
		}
	}
	if len(matching) == 0 {
		return nil, "requester and namespace are not allowlisted for this playbook", nil
	}

	var job batchv1.Job
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &job)
	if apierrors.IsNotFound(err) {
		// A checked no-op is safe only when the exact name fits an allowlisted
		// CronJob or standalone family. Record absence so a later replacement is
		// never deleted by this approval.
		for _, rule := range matching {
			for _, cronJob := range rule.CronJobs {
				if strings.HasPrefix(name, cronJob+"-") {
					return &preparedPlaybook{targetAbsent: true}, "", nil
				}
			}
			for _, prefix := range rule.StandaloneNamePrefixes {
				if strings.HasPrefix(name, prefix) {
					return &preparedPlaybook{targetAbsent: true}, "", nil
				}
			}
		}
		return nil, "missing Job name is outside the allowlisted families", nil
	}
	if err != nil {
		return nil, "", err
	}
	if !jobTerminal(&job) {
		return nil, "target Job is not terminal", nil
	}
	for _, rule := range matching {
		if matchesTerminalJobRule(&job, rule) {
			return &preparedPlaybook{targetUID: job.UID}, "", nil
		}
	}
	return nil, "target Job ownership/name is outside the allowlist", nil
}

func (r *SudoRequestReconciler) prepareCronJobTrigger(ctx context.Context, sr *SudoRequest) (*preparedPlaybook, string, error) {
	namespace, cronJobName, jobName, err := cronJobTriggerParameters(sr)
	if err != nil {
		return nil, "", err
	}
	if r.Playbooks == nil {
		return nil, "playbook auto-approval is not configured", nil
	}
	allowed := false
	for _, rule := range r.Playbooks.CronJobTrigger {
		if rule.Requester == sr.Spec.Requester && rule.Namespace == namespace && contains(rule.CronJobs, cronJobName) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, "requester, namespace, and CronJob are not allowlisted for this playbook", nil
	}

	var cronJob batchv1.CronJob
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cronJobName}, &cronJob)
	if apierrors.IsNotFound(err) {
		return nil, "target CronJob does not exist", nil
	}
	if err != nil {
		return nil, "", err
	}
	if cronJobUsesReservedExecutorIdentity(&cronJob) {
		return nil, "target CronJob template uses reserved sudo-service executor identity", nil
	}
	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		return nil, "target CronJob is suspended", nil
	}
	if len(cronJob.Status.Active) > 0 {
		return nil, "target CronJob already has an active Job", nil
	}

	var jobs batchv1.JobList
	if err := r.APIReader.List(ctx, &jobs, client.InNamespace(namespace)); err != nil {
		return nil, "", err
	}
	for i := range jobs.Items {
		existing := &jobs.Items[i]
		if existing.Name == jobName {
			return nil, "target Job name already exists", nil
		}
		if !jobTerminal(existing) && jobBelongsToCronJobOrManualFamily(existing, cronJobName) {
			return nil, "an active Job for this CronJob is already running", nil
		}
	}
	return &preparedPlaybook{targetUID: cronJob.UID}, "", nil
}

func (r *SudoRequestReconciler) prepareArgoCDRefresh(ctx context.Context, sr *SudoRequest) (*preparedPlaybook, string, error) {
	namespace, name, err := argocdRefreshParameters(sr)
	if err != nil {
		return nil, "", err
	}
	if r.Playbooks == nil {
		return nil, "playbook auto-approval is not configured", nil
	}
	allowed := false
	for _, rule := range r.Playbooks.ArgoCDRefresh {
		if rule.Requester == sr.Spec.Requester && rule.Namespace == namespace &&
			(rule.AllowAllApplications || contains(rule.Applications, name)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, "requester, namespace, and Application are not allowlisted for this playbook", nil
	}

	app := newArgoCDApplicationObject()
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, app)
	if apierrors.IsNotFound(err) {
		return nil, "target Application does not exist", nil
	}
	if err != nil {
		return nil, "", err
	}
	return &preparedPlaybook{targetUID: app.GetUID()}, "", nil
}

func (r *SudoRequestReconciler) executeAutoApprovedPlaybook(ctx context.Context, sr *SudoRequest) (bool, ctrl.Result, error) {
	if sr.Spec.Playbook == nil || !sr.Status.PlaybookAutoApproved {
		return false, ctrl.Result{}, nil
	}
	switch sr.Spec.Playbook.Name {
	case terminalJobCleanupPlaybook:
		return r.executeTerminalJobCleanup(ctx, sr)
	case cronJobTriggerPlaybook:
		return r.executeCronJobTrigger(ctx, sr)
	case argocdRefreshPlaybook:
		return r.executeArgoCDRefresh(ctx, sr)
	default:
		return true, ctrl.Result{}, fmt.Errorf("unsupported playbook %q", sr.Spec.Playbook.Name)
	}
}

func (r *SudoRequestReconciler) executeTerminalJobCleanup(ctx context.Context, sr *SudoRequest) (bool, ctrl.Result, error) {
	namespace, name, err := terminalJobCleanupParameters(sr)
	if err != nil {
		return true, ctrl.Result{}, err
	}
	var job batchv1.Job
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &job)
	if sr.Status.PlaybookTargetAbsent {
		if apierrors.IsNotFound(err) {
			return true, ctrl.Result{}, r.markPlaybookExecuted(ctx, sr, "target Job was already absent")
		}
		if err != nil {
			return true, ctrl.Result{}, err
		}
		_, markErr := r.markFailed(ctx, sr, "Job appeared after the playbook was auto-approved; refusing to delete it")
		return true, ctrl.Result{}, markErr
	}
	if apierrors.IsNotFound(err) {
		return true, ctrl.Result{}, r.markPlaybookExecuted(ctx, sr, "target Job was deleted before execution")
	}
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if string(job.UID) != sr.Status.PlaybookTargetUID {
		_, markErr := r.markFailed(ctx, sr, "target Job was replaced by a different object; refusing to delete it")
		return true, ctrl.Result{}, markErr
	}
	if !jobTerminal(&job) {
		_, markErr := r.markFailed(ctx, sr, "target Job no longer satisfies the terminal-state precondition")
		return true, ctrl.Result{}, markErr
	}
	uid := job.UID
	propagation := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, &job, &client.DeleteOptions{Raw: &metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &uid},
		PropagationPolicy: &propagation,
	}}); err != nil && !apierrors.IsNotFound(err) {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, r.markPlaybookExecuted(ctx, sr, "terminal Job deletion accepted")
}

func (r *SudoRequestReconciler) executeCronJobTrigger(ctx context.Context, sr *SudoRequest) (bool, ctrl.Result, error) {
	namespace, cronJobName, jobName, err := cronJobTriggerParameters(sr)
	if err != nil {
		return true, ctrl.Result{}, err
	}

	var cronJob batchv1.CronJob
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cronJobName}, &cronJob)
	if apierrors.IsNotFound(err) {
		_, markErr := r.markFailed(ctx, sr, "target CronJob disappeared before execution")
		return true, ctrl.Result{}, markErr
	}
	if err != nil {
		return true, ctrl.Result{}, err
	}
	if string(cronJob.UID) != sr.Status.PlaybookTargetUID {
		_, markErr := r.markFailed(ctx, sr, "target CronJob was replaced by a different object; refusing to trigger it")
		return true, ctrl.Result{}, markErr
	}
	if cronJobUsesReservedExecutorIdentity(&cronJob) {
		_, markErr := r.markFailed(ctx, sr, "target CronJob template uses reserved sudo-service executor identity")
		return true, ctrl.Result{}, markErr
	}
	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		_, markErr := r.markFailed(ctx, sr, "target CronJob was suspended before execution")
		return true, ctrl.Result{}, markErr
	}

	var existingJob batchv1.Job
	err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: jobName}, &existingJob)
	if err == nil {
		if existingJob.Annotations[playbookRequestUIDAnnotation] == string(sr.UID) {
			return true, ctrl.Result{}, r.markPlaybookExecuted(ctx, sr, "manual Job already created for this request")
		}
		_, markErr := r.markFailed(ctx, sr, "target Job name already exists; refusing to overwrite or adopt it")
		return true, ctrl.Result{}, markErr
	}
	if !apierrors.IsNotFound(err) {
		return true, ctrl.Result{}, err
	}

	if len(cronJob.Status.Active) > 0 {
		_, markErr := r.markFailed(ctx, sr, "target CronJob gained an active Job before execution; refusing concurrent trigger")
		return true, ctrl.Result{}, markErr
	}
	var jobs batchv1.JobList
	if err := r.APIReader.List(ctx, &jobs, client.InNamespace(namespace)); err != nil {
		return true, ctrl.Result{}, err
	}
	for i := range jobs.Items {
		candidate := &jobs.Items[i]
		if !jobTerminal(candidate) && jobBelongsToCronJobOrManualFamily(candidate, cronJobName) {
			_, markErr := r.markFailed(ctx, sr, "an active Job for this CronJob started before execution; refusing concurrent trigger")
			return true, ctrl.Result{}, markErr
		}
	}

	job := buildCronJobManualJob(&cronJob, jobName, sr.UID)
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			var raced batchv1.Job
			if getErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: jobName}, &raced); getErr == nil &&
				raced.Annotations[playbookRequestUIDAnnotation] == string(sr.UID) {
				return true, ctrl.Result{}, r.markPlaybookExecuted(ctx, sr, "manual Job already created for this request")
			}
			_, markErr := r.markFailed(ctx, sr, "target Job name already exists; refusing to overwrite or adopt it")
			return true, ctrl.Result{}, markErr
		}
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			_, markErr := r.markFailed(ctx, sr, fmt.Sprintf("Job creation rejected: %v", err))
			return true, ctrl.Result{}, markErr
		}
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, r.markPlaybookExecuted(ctx, sr, fmt.Sprintf("created manual Job %s/%s from CronJob %s", namespace, jobName, cronJobName))
}

func buildCronJobManualJob(cronJob *batchv1.CronJob, jobName string, requestUID types.UID) *batchv1.Job {
	labels := make(map[string]string, len(cronJob.Spec.JobTemplate.Labels))
	for k, v := range cronJob.Spec.JobTemplate.Labels {
		labels[k] = v
	}
	annotations := make(map[string]string, len(cronJob.Spec.JobTemplate.Annotations)+2)
	for k, v := range cronJob.Spec.JobTemplate.Annotations {
		annotations[k] = v
	}
	annotations["cronjob.kubernetes.io/instantiate"] = "manual"
	annotations[playbookRequestUIDAnnotation] = string(requestUID)

	isController := true
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   cronJob.Namespace,
			Name:        jobName,
			Labels:      labels,
			Annotations: annotations,
			// Omit BlockOwnerDeletion so creating the child Job requires only batch/jobs
			// create + batch/cronjobs get, without needing update on cronjobs/finalizers
			// under OwnerReferencesPermissionEnforcement.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "CronJob",
				Name:       cronJob.Name,
				UID:        cronJob.UID,
				Controller: &isController,
			}},
		},
		Spec: *cronJob.Spec.JobTemplate.Spec.DeepCopy(),
	}
}

func (r *SudoRequestReconciler) executeArgoCDRefresh(ctx context.Context, sr *SudoRequest) (bool, ctrl.Result, error) {
	namespace, name, err := argocdRefreshParameters(sr)
	if err != nil {
		return true, ctrl.Result{}, err
	}

	var failureReason string
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		failureReason = ""
		app := newArgoCDApplicationObject()
		if getErr := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, app); getErr != nil {
			if apierrors.IsNotFound(getErr) {
				failureReason = "target Application disappeared before execution"
				return nil
			}
			return getErr
		}
		if string(app.GetUID()) != sr.Status.PlaybookTargetUID {
			failureReason = "target Application was replaced by a different object; refusing to refresh it"
			return nil
		}
		meta := map[string]any{
			"uid":         string(app.GetUID()),
			"annotations": map[string]string{"argocd.argoproj.io/refresh": "hard"},
		}
		if rv := app.GetResourceVersion(); rv != "" {
			meta["resourceVersion"] = rv
		}
		patchBytes, marshalErr := json.Marshal(map[string]any{"metadata": meta})
		if marshalErr != nil {
			return marshalErr
		}
		return r.Patch(ctx, app, client.RawPatch(types.MergePatchType, patchBytes))
	})
	if failureReason != "" {
		_, markErr := r.markFailed(ctx, sr, failureReason)
		return true, ctrl.Result{}, markErr
	}
	if err != nil {
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			_, markErr := r.markFailed(ctx, sr, fmt.Sprintf("Application refresh patch rejected: %v", err))
			return true, ctrl.Result{}, markErr
		}
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, r.markPlaybookExecuted(ctx, sr, fmt.Sprintf("annotated Application %s/%s with argocd.argoproj.io/refresh=hard", namespace, name))
}

func (r *SudoRequestReconciler) markPlaybookExecuted(ctx context.Context, sr *SudoRequest, message string) error {
	zero := int32(0)
	if err := r.updateStatus(ctx, sr, func(current *SudoRequest) {
		current.Status.Phase = PhaseExecuted
		current.Status.ExitCode = &zero
		current.Status.FailureReason = ""
	}); err != nil {
		return err
	}
	r.Recorder.Eventf(sr, corev1.EventTypeNormal, "Executed", "Playbook %s: %s", playbookName(sr), message)
	r.Broadcaster.Publish(string(sr.UID), Event{
		Type: "phase", Phase: PhaseExecuted, ExitCode: &zero,
		Requester: sr.Spec.Requester, Reason: sr.Spec.Reason, Command: sr.Spec.Command,
		CreatedAt: sr.CreationTimestamp.Format("2006-01-02 15:04:05 UTC"), RetryOfUID: sr.Spec.RetryOfUID,
	})
	return nil
}
