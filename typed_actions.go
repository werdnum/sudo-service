package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

const (
	TypedActionVersion             = "v1"
	OperationJobDelete             = "job.delete"
	OperationCronJobRun            = "cronjob.run"
	OperationWorkloadRestart       = "workload.restart"
	OperationSecretRead            = "secret.read"
	CronJobRunCleanupSeconds int32 = 24 * 60 * 60
)

// TypedAction is a narrow semantic request which the controller compiles into
// both the command and the reviewer-facing permission request. Requesters never
// get to provide those two representations independently.
type TypedAction struct {
	Version   string             `json:"version"`
	Operation string             `json:"operation"`
	Resources []TypedResourceRef `json:"resources"`
	Key       string             `json:"key,omitempty"`
	JobName   string             `json:"jobName,omitempty"`
}

type TypedResourceRef struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

type TypedActionPlan struct {
	Command           string
	PermissionRequest string
}

func compileTypedAction(action *TypedAction) (*TypedActionPlan, error) {
	if action == nil {
		return nil, nil
	}
	if action.Version != TypedActionVersion {
		return nil, fmt.Errorf("action.version must be %q", TypedActionVersion)
	}
	if len(action.Resources) == 0 {
		return nil, fmt.Errorf("action.resources must contain at least one exact resource")
	}
	resources := append([]TypedResourceRef(nil), action.Resources...)
	seen := map[string]bool{}
	for _, ref := range resources {
		if errs := utilvalidation.IsDNS1123Label(ref.Namespace); len(errs) > 0 {
			return nil, fmt.Errorf("invalid action resource namespace %q: %s", ref.Namespace, strings.Join(errs, "; "))
		}
		if errs := utilvalidation.IsDNS1123Subdomain(ref.Name); len(errs) > 0 {
			return nil, fmt.Errorf("invalid action resource name %q: %s", ref.Name, strings.Join(errs, "; "))
		}
		identity := ref.Namespace + "/" + ref.Kind + "/" + ref.Name
		if seen[identity] {
			return nil, fmt.Errorf("duplicate action resource %s", identity)
		}
		seen[identity] = true
	}
	sort.Slice(resources, func(i, j int) bool {
		if resources[i].Namespace != resources[j].Namespace {
			return resources[i].Namespace < resources[j].Namespace
		}
		if resources[i].Kind != resources[j].Kind {
			return resources[i].Kind < resources[j].Kind
		}
		return resources[i].Name < resources[j].Name
	})

	switch action.Operation {
	case OperationJobDelete:
		if action.Key != "" || action.JobName != "" {
			return nil, fmt.Errorf("job.delete does not accept key or jobName")
		}
		byNamespace := map[string][]string{}
		var targets []string
		for _, ref := range resources {
			if ref.Kind != "Job" {
				return nil, fmt.Errorf("job.delete resources must have kind Job, got %q", ref.Kind)
			}
			byNamespace[ref.Namespace] = append(byNamespace[ref.Namespace], ref.Name)
			targets = append(targets, ref.Namespace+"/"+ref.Name)
		}
		var commands []string
		for _, ref := range resources {
			if _, done := byNamespace[ref.Namespace]; !done {
				continue
			}
			names := byNamespace[ref.Namespace]
			delete(byNamespace, ref.Namespace)
			quoted := make([]string, len(names))
			for i, name := range names {
				quoted[i] = shellQuote(name)
			}
			commands = append(commands, fmt.Sprintf("kubectl delete job --namespace %s --ignore-not-found=true --wait=true -- %s", shellQuote(ref.Namespace), strings.Join(quoted, " ")))
		}
		return &TypedActionPlan{
			Command:           "set -eu\n" + strings.Join(commands, "\n"),
			PermissionRequest: "Delete the exact Jobs " + englishList(targets) + ".",
		}, nil

	case OperationCronJobRun:
		if len(resources) != 1 || resources[0].Kind != "CronJob" {
			return nil, fmt.Errorf("cronjob.run requires exactly one CronJob resource")
		}
		if action.Key != "" {
			return nil, fmt.Errorf("cronjob.run does not accept key")
		}
		if len(action.JobName) > 63 {
			return nil, fmt.Errorf("cronjob.run jobName must be at most 63 characters")
		}
		if errs := utilvalidation.IsDNS1123Subdomain(action.JobName); len(errs) > 0 {
			return nil, fmt.Errorf("invalid cronjob.run jobName %q: %s", action.JobName, strings.Join(errs, "; "))
		}
		ref := resources[0]
		suffixLength := len("-manual-") + len("20060102150405")
		prefix := strings.TrimRight(ref.Name[:min(len(ref.Name), 63-suffixLength)], "-.")
		jobNameRE := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-manual-([0-9]{14})$`)
		match := jobNameRE.FindStringSubmatch(action.JobName)
		if len(match) != 2 {
			return nil, fmt.Errorf("cronjob.run jobName must use the deterministic form %s-manual-<UTC YYYYMMDDhhmmss>", prefix)
		}
		if _, err := time.Parse("20060102150405", match[1]); err != nil {
			return nil, fmt.Errorf("cronjob.run jobName has invalid UTC timestamp: %w", err)
		}
		command := fmt.Sprintf(`set -eu
kubectl create job %s --from=cronjob/%s --namespace %s --dry-run=client --output=json >/tmp/typed-cronjob.json
kubectl patch --local --filename=/tmp/typed-cronjob.json --type=merge --patch='{"spec":{"ttlSecondsAfterFinished":%d}}' --output=json >/tmp/typed-cronjob-patched.json
kubectl create --filename=/tmp/typed-cronjob-patched.json`, shellQuote(action.JobName), shellQuote(ref.Name), shellQuote(ref.Namespace), CronJobRunCleanupSeconds)
		return &TypedActionPlan{
			Command:           command,
			PermissionRequest: fmt.Sprintf("Create Job %s/%s from CronJob %s/%s and delete it 24 hours after it finishes.", ref.Namespace, action.JobName, ref.Namespace, ref.Name),
		}, nil

	case OperationWorkloadRestart:
		if len(resources) != 1 || action.Key != "" || action.JobName != "" {
			return nil, fmt.Errorf("workload.restart requires exactly one resource and does not accept key or jobName")
		}
		ref := resources[0]
		kindArg := map[string]string{"Deployment": "deployment", "StatefulSet": "statefulset", "DaemonSet": "daemonset"}[ref.Kind]
		if kindArg == "" {
			return nil, fmt.Errorf("workload.restart kind must be Deployment, StatefulSet, or DaemonSet, got %q", ref.Kind)
		}
		return &TypedActionPlan{
			Command:           fmt.Sprintf("kubectl rollout restart %s/%s --namespace %s", kindArg, shellQuote(ref.Name), shellQuote(ref.Namespace)),
			PermissionRequest: fmt.Sprintf("Restart %s %s/%s.", ref.Kind, ref.Namespace, ref.Name),
		}, nil

	case OperationSecretRead:
		if len(resources) != 1 || resources[0].Kind != "Secret" || action.JobName != "" {
			return nil, fmt.Errorf("secret.read requires exactly one Secret resource and does not accept jobName")
		}
		if errs := utilvalidation.IsConfigMapKey(action.Key); len(errs) > 0 {
			return nil, fmt.Errorf("invalid secret.read key %q: %s", action.Key, strings.Join(errs, "; "))
		}
		ref := resources[0]
		template := fmt.Sprintf(`{{ index .data %q | base64decode }}`, action.Key)
		return &TypedActionPlan{
			Command:           fmt.Sprintf("kubectl get secret %s --namespace %s --output=go-template=%s", shellQuote(ref.Name), shellQuote(ref.Namespace), shellQuote(template)),
			PermissionRequest: fmt.Sprintf("Read key %s from Secret %s/%s.", action.Key, ref.Namespace, ref.Name),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported action.operation %q", action.Operation)
	}
}

func englishList(values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	case 2:
		return values[0] + " and " + values[1]
	default:
		return strings.Join(values[:len(values)-1], ", ") + ", and " + values[len(values)-1]
	}
}

// validateTypedActionBinding proves that the free-form execution fields stored
// in the CR are exactly the server compiler's expansion of spec.action.
func validateTypedActionBinding(sr *SudoRequest) (*TypedActionPlan, error) {
	plan, err := compileTypedAction(sr.Spec.Action)
	if err != nil || plan == nil {
		return plan, err
	}
	if sr.Spec.Command != plan.Command {
		return nil, fmt.Errorf("command does not match the canonical expansion of spec.action")
	}
	if sr.Spec.Profile != DefaultExecutorProfile || sr.Spec.Image != "" {
		return nil, fmt.Errorf("typed actions must use profile %q and cannot set image", DefaultExecutorProfile)
	}
	if sr.Spec.Namespace != "" || sr.Spec.Stdin != "" || len(sr.Spec.Env) != 0 || len(sr.Spec.EnvFrom) != 0 || len(sr.Spec.Volumes) != 0 || len(sr.Spec.VolumeMounts) != 0 || len(sr.Spec.InitContainers) != 0 || len(sr.Spec.ImagePullSecrets) != 0 || sr.Spec.Privileges.ClusterAdmin != nil {
		return nil, fmt.Errorf("typed actions cannot set executor namespace, stdin, pod fields, image pull secrets, or privilege overrides")
	}
	return plan, nil
}
