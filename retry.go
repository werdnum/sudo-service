package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// executionFingerprint hashes every execution-affecting spec field after JSON
// canonicalization. Requester, reason, attribution, and lineage are excluded;
// stdin and env values contribute only inside the digest and are never emitted.
func executionFingerprint(spec *SudoRequestSpec) (string, error) {
	var effective SudoRequestSpec
	spec.DeepCopyInto(&effective)
	effective.Requester = ""
	effective.Reason = ""
	effective.SubmittedBy = ""
	effective.RetryOfUID = ""
	if effective.Image == "" {
		effective.Image = DefaultExecutorImage
	}
	if effective.Namespace == "" {
		effective.Namespace = ControllerNamespace
	}
	if effective.TTLSecondsAfterApproval == nil {
		value := int32(DefaultPostApproval)
		effective.TTLSecondsAfterApproval = &value
	}
	if effective.Privileges.ClusterAdmin == nil {
		value := effective.Namespace == ControllerNamespace
		effective.Privileges.ClusterAdmin = &value
	}
	raw, err := json.Marshal(effective)
	if err != nil {
		return "", err
	}
	var canonical any
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return "", err
	}
	raw, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (a *APIServer) findPendingDuplicate(ctx context.Context, candidate *SudoRequest) (*SudoRequest, error) {
	want, err := executionFingerprint(&candidate.Spec)
	if err != nil {
		return nil, err
	}
	var list SudoRequestList
	if err := a.Client.List(ctx, &list, client.InNamespace(ControllerNamespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		current := &list.Items[i]
		// Cross-requester matches are deliberately indistinguishable from no match.
		if (current.Status.Phase != "" && current.Status.Phase != PhasePending) || current.Spec.Requester != candidate.Spec.Requester {
			continue
		}
		got, err := executionFingerprint(&current.Spec)
		if err != nil { // one malformed legacy/direct object must not block submissions
			continue
		}
		if got == want {
			return current.DeepCopy(), nil
		}
	}
	return nil, nil
}

func submittedByFor(sr *SudoRequest) string {
	if sr.Spec.SubmittedBy != "" {
		return sr.Spec.SubmittedBy
	}
	return sr.Spec.Requester
}

func retryChildName(uid types.UID) string { return "retry-" + strings.ToLower(string(uid)) }

func requesterRetryable(phase string) bool { return phase == PhaseExpired || phase == PhaseFailed }

func adminRetryable(phase string) bool { return isTerminalPhase(phase) }

// validateRetrySuccessor authenticates an object occupying the deterministic
// retry name before it is adopted as the source's one successor. Lineage and
// requester checks prevent an unrelated request from being linked, while the
// execution fingerprint prevents a same-requester object with forged lineage
// but different execution fields from winning a create race.
func validateRetrySuccessor(source, successor *SudoRequest) error {
	if successor.Spec.RetryOfUID != string(source.UID) || successor.Spec.Requester != source.Spec.Requester {
		return errors.New("deterministic retry name is occupied by an unrelated request")
	}
	want, err := executionFingerprint(&source.Spec)
	if err != nil {
		return fmt.Errorf("fingerprint retry source: %w", err)
	}
	got, err := executionFingerprint(&successor.Spec)
	if err != nil {
		return fmt.Errorf("fingerprint retry successor: %w", err)
	}
	if got != want {
		return errors.New("deterministic retry name is occupied by a request with different execution fields")
	}
	return nil
}

func (a *APIServer) adoptRetrySuccessor(ctx context.Context, source, successor *SudoRequest) (*SudoRequest, bool, error) {
	if err := validateRetrySuccessor(source, successor); err != nil {
		return nil, false, err
	}
	if err := a.ensureSupersededLink(ctx, source.Name, source.UID, successor.UID); err != nil {
		return nil, false, err
	}
	return successor.DeepCopy(), false, nil
}

// retryRequest creates exactly one successor per predecessor. The deterministic
// name is the cross-replica idempotency key. If linking the predecessor status
// fails after creation, a repeated call finds the same child and repairs the link.
func (a *APIServer) retryRequest(ctx context.Context, source *SudoRequest, submittedBy string, admin bool) (*SudoRequest, bool, error) {
	if source.UID == "" {
		return nil, false, errors.New("source request has no UID")
	}
	if (admin && !adminRetryable(source.Status.Phase)) || (!admin && !requesterRetryable(source.Status.Phase)) {
		return nil, false, fmt.Errorf("request phase %q is not eligible for %s resubmission", source.Status.Phase, map[bool]string{true: "administrator", false: "requester"}[admin])
	}

	name := retryChildName(source.UID)
	var existing SudoRequest
	if err := a.Client.Get(ctx, client.ObjectKey{Namespace: ControllerNamespace, Name: name}, &existing); err == nil {
		return a.adoptRetrySuccessor(ctx, source, &existing)
	} else if !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	if source.Status.SupersededByUID != "" {
		return nil, false, fmt.Errorf("source already superseded by %s, but that successor is unavailable", source.Status.SupersededByUID)
	}

	var spec SudoRequestSpec
	source.Spec.DeepCopyInto(&spec)
	spec.RetryOfUID = string(source.UID)
	spec.SubmittedBy = submittedBy
	successor := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ControllerNamespace},
		Spec:       spec,
	}

	if duplicate, err := a.findPendingDuplicate(ctx, successor); err != nil {
		return nil, false, err
	} else if duplicate != nil {
		// A concurrent retry may have created our deterministic child between the
		// initial Get and this List. That is idempotent success, not a generic
		// duplicate conflict; authenticate it and repair the forward link.
		if duplicate.Name == name {
			return a.adoptRetrySuccessor(ctx, source, duplicate)
		}
		return duplicate, false, &pendingDuplicateError{UID: duplicate.UID}
	}
	// A retry is a fresh submission, so re-run current profile resolution and
	// preflight before creating it. Catalog support may have changed since the
	// predecessor was accepted; fail synchronously instead of creating a request
	// that handleNew will immediately deny.
	if _, _, _, err := resolveAndPreflight(successor); err != nil {
		return nil, false, fmt.Errorf("retry preflight: %w", err)
	}
	if err := validateCommandSyntax(successor.Spec.Command); err != nil {
		return nil, false, err
	}
	if err := validateSpecExtras(successor); err != nil {
		return nil, false, err
	}
	created := true
	if err := a.Client.Create(ctx, successor); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, false, err
		}
		created = false
		if err := a.Client.Get(ctx, client.ObjectKey{Namespace: ControllerNamespace, Name: name}, successor); err != nil {
			return nil, false, err
		}
		if err := validateRetrySuccessor(source, successor); err != nil {
			return nil, false, err
		}
	}
	if err := a.ensureSupersededLink(ctx, source.Name, source.UID, successor.UID); err != nil {
		return nil, created, err
	}
	return successor, created, nil
}

type pendingDuplicateError struct{ UID types.UID }

func (e *pendingDuplicateError) Error() string { return "equivalent request is already pending" }

func (a *APIServer) ensureSupersededLink(ctx context.Context, name string, sourceUID, successorUID types.UID) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current SudoRequest
		if err := a.Client.Get(ctx, client.ObjectKey{Namespace: ControllerNamespace, Name: name}, &current); err != nil {
			return err
		}
		if current.UID != sourceUID {
			return fmt.Errorf("source request %q was replaced", name)
		}
		if current.Status.SupersededByUID == string(successorUID) {
			return nil
		}
		if current.Status.SupersededByUID != "" {
			return fmt.Errorf("source already superseded by %s", current.Status.SupersededByUID)
		}
		current.Status.SupersededByUID = string(successorUID)
		return a.Client.Status().Update(ctx, &current)
	})
}

func (a *APIServer) retryRequester(w http.ResponseWriter, r *http.Request, source *SudoRequest, identity authv1.UserInfo) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	allowed, _, err := a.Authorizer.AuthorizeSubmit(r.Context(), identity)
	if err != nil {
		http.Error(w, "request authorization unavailable", http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	a.requestMu.Lock()
	defer a.requestMu.Unlock()
	successor, created, err := a.retryRequest(r.Context(), source, identity.Username, false)
	if err != nil {
		var duplicate *pendingDuplicateError
		if errors.As(err, &duplicate) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "uid": string(duplicate.UID)})
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeCreateResponse(w, successor, !created)
}
