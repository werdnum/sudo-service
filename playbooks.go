package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const terminalJobCleanupPlaybook = "job.cleanup-terminal/v1"

// PlaybookConfig is intentionally a closed schema. Adding another
// auto-approved action requires code, validation and tests rather than a broad
// command prefix in configuration.
type PlaybookConfig struct {
	TerminalJobCleanup []TerminalJobCleanupRule `json:"terminalJobCleanup,omitempty"`
}

type TerminalJobCleanupRule struct {
	Requester              string   `json:"requester"`
	Namespace              string   `json:"namespace"`
	CronJobs               []string `json:"cronJobs,omitempty"`
	StandaloneNamePrefixes []string `json:"standaloneNamePrefixes,omitempty"`
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

func terminalJobCleanupParameters(sr *SudoRequest) (namespace, name string, err error) {
	if sr.Spec.Playbook == nil || sr.Spec.Playbook.Name != terminalJobCleanupPlaybook {
		return "", "", fmt.Errorf("unsupported playbook %q", playbookName(sr))
	}
	params := sr.Spec.Playbook.Parameters
	if len(params) != 2 {
		keys := make([]string, 0, len(params))
		for key := range params {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return "", "", fmt.Errorf("playbook %s requires exactly namespace and name parameters (got %v)", terminalJobCleanupPlaybook, keys)
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

func playbookName(sr *SudoRequest) string {
	if sr.Spec.Playbook == nil {
		return ""
	}
	return sr.Spec.Playbook.Name
}

func terminalJobCleanupCommand(namespace, name string) string {
	return fmt.Sprintf("kubectl delete job %s -n %s --ignore-not-found=true --wait=false", name, namespace)
}

func validatePlaybookSpec(sr *SudoRequest) error {
	if sr.Spec.Playbook == nil {
		return nil
	}
	namespace, name, err := terminalJobCleanupParameters(sr)
	if err != nil {
		return err
	}
	if sr.Spec.Command != terminalJobCleanupCommand(namespace, name) {
		return fmt.Errorf("playbook command must equal canonical rendering %q", terminalJobCleanupCommand(namespace, name))
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

func (r *SudoRequestReconciler) prepareAutoApprovedPlaybook(ctx context.Context, sr *SudoRequest) (*preparedPlaybook, string, error) {
	if sr.Spec.Playbook == nil {
		return nil, "", nil
	}
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

func (r *SudoRequestReconciler) executeAutoApprovedPlaybook(ctx context.Context, sr *SudoRequest) (bool, ctrl.Result, error) {
	if sr.Spec.Playbook == nil || !sr.Status.PlaybookAutoApproved {
		return false, ctrl.Result{}, nil
	}
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
