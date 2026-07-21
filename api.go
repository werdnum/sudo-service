package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed templates/*.html
var templatesFS embed.FS

// APIServer wires up the HTTP handlers. It does NOT own the reconciler lifecycle —
// it borrows the manager's client (cached, so reads are cheap) and pokes the reconciler
// through Approve/Deny helpers.
type APIServer struct {
	Client        client.Client
	Verifier      *JWTVerifier
	TokenReviewer *TokenReviewer
	Authorizer    *RequesterAuthorizer
	Broadcaster   *Broadcaster
	Reconciler    *SudoRequestReconciler // wired up at start time
	Config        *Config
	Templates     *template.Template
}

func (a *APIServer) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", a.healthHandler)
	mux.HandleFunc("/", a.indexHandler)
	mux.HandleFunc("/approve", a.approveHandler)
	mux.HandleFunc("/deny", a.denyHandler)
	mux.HandleFunc("/requests", a.createRequestHandler)
	mux.HandleFunc("/requests/", a.requestSubpathHandler)
	mux.HandleFunc("/profiles", a.profilesHandler)
	mux.HandleFunc("/events", a.globalEventsHandler)
}

func (a *APIServer) healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ---- HTTP API (requester side) ----

type createRequestBody struct {
	Reason                  string `json:"reason"`
	Command                 string `json:"command"`
	Image                   string `json:"image,omitempty"`
	Profile                 string `json:"profile,omitempty"`
	TTLSecondsAfterApproval *int32 `json:"ttlSecondsAfterApproval,omitempty"`

	// Widened pod fields — same shape as the CRD spec, carried as raw JSON so a
	// malformed item is rejected by validateSpecExtras (400) rather than failing
	// the body decode in a way that diverges from the CRD path.
	Namespace        string                 `json:"namespace,omitempty"`
	Stdin            string                 `json:"stdin,omitempty"`
	Env              []runtime.RawExtension `json:"env,omitempty"`
	EnvFrom          []runtime.RawExtension `json:"envFrom,omitempty"`
	Volumes          []runtime.RawExtension `json:"volumes,omitempty"`
	VolumeMounts     []runtime.RawExtension `json:"volumeMounts,omitempty"`
	InitContainers   []runtime.RawExtension `json:"initContainers,omitempty"`
	ImagePullSecrets []runtime.RawExtension `json:"imagePullSecrets,omitempty"`
	Privileges       SudoRequestPrivileges  `json:"privileges,omitempty"`
}

type requestStatusResponse struct {
	UID                       string                `json:"uid"`
	Name                      string                `json:"name"`
	Phase                     string                `json:"phase"`
	Requester                 string                `json:"requester"`
	Command                   string                `json:"command"`
	Image                     string                `json:"image"`
	Profile                   string                `json:"profile,omitempty"`
	PreflightWarnings         []string              `json:"preflightWarnings,omitempty"`
	Namespace                 string                `json:"namespace"`
	ClusterAdmin              bool                  `json:"clusterAdmin"`
	ApprovedBy                string                `json:"approvedBy,omitempty"`
	ApprovedAt                string                `json:"approvedAt,omitempty"`
	DeniedBy                  string                `json:"deniedBy,omitempty"`
	DeniedAt                  string                `json:"deniedAt,omitempty"`
	DenialReason              string                `json:"denialReason,omitempty"`
	FailureReason             string                `json:"failureReason,omitempty"`
	ExitCode                  *int32                `json:"exitCode,omitempty"`
	OutputSecretRef           string                `json:"outputSecretRef,omitempty"`
	OutputCaptureState        string                `json:"outputCaptureState,omitempty"`
	OutputDeliveryState       string                `json:"outputDeliveryState,omitempty"`
	OutputFailureReason       string                `json:"outputFailureReason,omitempty"`
	OutputTotalBytes          *int64                `json:"outputTotalBytes,omitempty"`
	OutputRetainedBytes       *int64                `json:"outputRetainedBytes,omitempty"`
	OutputSHA256              string                `json:"outputSHA256,omitempty"`
	Summary                   string                `json:"summary,omitempty"`
	PermissionAssessment      *PermissionAssessment `json:"permissionAssessment,omitempty"`
	PermissionAssessmentState string                `json:"permissionAssessmentState,omitempty"`
}

// createRequestHandler is POST /requests. The SA bearer token is authenticated
// via TokenReview, then authorized for the dedicated HTTP-submission permission
// via SubjectAccessReview. On success, it returns the CR UID + name.
// spec.requester is server-stamped to the authenticated username, so the
// requester can't spoof another agent's identity via the HTTP path.
func (a *APIServer) createRequestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := a.authenticateBearer(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
	var body createRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Command == "" || body.Reason == "" {
		http.Error(w, "command and reason are required", http.StatusBadRequest)
		return
	}
	if err := validateCommandSyntax(body.Command); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sr := &SudoRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "http-",
			Namespace:    ControllerNamespace,
		},
		Spec: SudoRequestSpec{
			Requester:               identity.Username,
			Reason:                  body.Reason,
			Command:                 body.Command,
			Image:                   body.Image,
			Profile:                 body.Profile,
			TTLSecondsAfterApproval: body.TTLSecondsAfterApproval,
			Namespace:               body.Namespace,
			Stdin:                   body.Stdin,
			Env:                     body.Env,
			EnvFrom:                 body.EnvFrom,
			Volumes:                 body.Volumes,
			VolumeMounts:            body.VolumeMounts,
			InitContainers:          body.InitContainers,
			ImagePullSecrets:        body.ImagePullSecrets,
			Privileges:              body.Privileges,
		},
	}
	profile, resolvedImage, warnings, err := resolveAndPreflight(sr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateSpecExtras(sr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.Client.Create(r.Context(), sr); err != nil {
		http.Error(w, "create: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{
		"uid": string(sr.UID), "name": sr.Name, "image": resolvedImage,
	}
	if profile != nil {
		response["profile"] = profile.Name
	}
	if len(warnings) > 0 {
		response["warnings"] = warnings
	}
	_ = json.NewEncoder(w).Encode(response)
}

// profilesHandler publishes the controller-owned catalog used to resolve
// friendly aliases. Authentication matches request submission; the response is
// machine-readable and includes the exact digest and conservative metadata.
func (a *APIServer) profilesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := a.authenticateBearer(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"default":  DefaultExecutorProfile,
		"profiles": listExecutorProfiles(),
	})
}

// requestSubpathHandler routes GET /requests/{uid}, /requests/{uid}/output, /requests/{uid}/events.
func (a *APIServer) requestSubpathHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/requests/")
	parts := strings.SplitN(rest, "/", 2)
	uid := parts[0]
	if uid == "" {
		http.Error(w, "missing uid", http.StatusBadRequest)
		return
	}

	identity, err := a.authenticateBearer(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sr, err := a.findByUID(r.Context(), types.UID(uid))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if sr.Spec.Requester != identity.Username {
		http.Error(w, "forbidden: not the requester", http.StatusForbidden)
		return
	}

	subpath := ""
	if len(parts) == 2 {
		subpath = parts[1]
	}
	switch subpath {
	case "":
		a.serveStatus(w, sr)
	case "output":
		a.serveOutput(w, r, sr)
	case "events":
		a.serveEvents(w, r, sr)
	default:
		http.NotFound(w, r)
	}
}

func (a *APIServer) serveStatus(w http.ResponseWriter, sr *SudoRequest) {
	resp := requestStatusResponse{
		UID:                       string(sr.UID),
		Name:                      sr.Name,
		Phase:                     sr.Status.Phase,
		Requester:                 sr.Spec.Requester,
		Command:                   sr.Spec.Command,
		Image:                     imageFor(sr),
		Profile:                   profileFor(sr),
		PreflightWarnings:         sr.Status.PreflightWarnings,
		Namespace:                 executorNamespace(sr),
		ClusterAdmin:              clusterAdminEnabled(sr),
		ApprovedBy:                sr.Status.ApprovedBy,
		DeniedBy:                  sr.Status.DeniedBy,
		DenialReason:              sr.Status.DenialReason,
		FailureReason:             sr.Status.FailureReason,
		ExitCode:                  sr.Status.ExitCode,
		OutputSecretRef:           sr.Status.OutputSecretRef,
		OutputCaptureState:        sr.Status.OutputCaptureState,
		OutputDeliveryState:       sr.Status.OutputDeliveryState,
		OutputFailureReason:       sr.Status.OutputFailureReason,
		OutputTotalBytes:          sr.Status.OutputTotalBytes,
		OutputRetainedBytes:       sr.Status.OutputRetainedBytes,
		OutputSHA256:              sr.Status.OutputSHA256,
		Summary:                   sr.Status.Summary,
		PermissionAssessment:      sr.Status.PermissionAssessment,
		PermissionAssessmentState: sr.Status.PermissionAssessmentState,
	}
	if sr.Status.ApprovedAt != nil {
		resp.ApprovedAt = sr.Status.ApprovedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if sr.Status.DeniedAt != nil {
		resp.DeniedAt = sr.Status.DeniedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (a *APIServer) serveOutput(w http.ResponseWriter, r *http.Request, sr *SudoRequest) {
	if sr.Status.OutputSecretRef == "" {
		http.Error(w, "output not available yet", http.StatusConflict)
		return
	}
	var sec corev1.Secret
	if err := a.Client.Get(r.Context(), client.ObjectKey{Namespace: ControllerNamespace, Name: sr.Status.OutputSecretRef}, &sec); err != nil {
		http.Error(w, "fetch output: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(sec.Data["output"])
}

func (a *APIServer) serveEvents(w http.ResponseWriter, r *http.Request, sr *SudoRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Subscribe BEFORE reading state to close the race where a terminal
	// transition happens between snapshot emission and Subscribe(). Any phase
	// change after this point is guaranteed to land in our channel; the
	// re-fetch below is what defines "now" for the snapshot.
	ch, cancel := a.Broadcaster.Subscribe(string(sr.UID))
	defer cancel()

	// Re-read state under the subscription. If the phase transitioned between
	// the caller's findByUID and here, we'll either see it now (and return
	// after emitting a terminal snapshot) or pick it up off the channel.
	if fresh, err := a.findByUID(r.Context(), sr.UID); err == nil {
		sr = fresh
	}

	snap := Event{Type: "snapshot", Phase: sr.Status.Phase, ExitCode: sr.Status.ExitCode, OutputSecretRef: sr.Status.OutputSecretRef, DenialReason: sr.Status.DenialReason, FailureReason: sr.Status.FailureReason, OutputCaptureState: sr.Status.OutputCaptureState, OutputDeliveryState: sr.Status.OutputDeliveryState, OutputFailureReason: sr.Status.OutputFailureReason, OutputTotalBytes: sr.Status.OutputTotalBytes, OutputRetainedBytes: sr.Status.OutputRetainedBytes, OutputSHA256: sr.Status.OutputSHA256}
	writeSSE(w, snap)
	flusher.Flush()

	if isTerminalPhase(sr.Status.Phase) {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			writeSSE(w, ev)
			flusher.Flush()
			if isTerminalPhase(ev.Phase) {
				return
			}
		}
	}
}

func writeSSE(w http.ResponseWriter, ev Event) {
	buf, _ := json.Marshal(ev)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", string(buf))
}

func isTerminalPhase(p string) bool {
	switch p {
	case PhaseExecuted, PhaseFailed, PhaseDenied, PhaseExpired:
		return true
	}
	return false
}

// authenticateBearer extracts and validates a SA bearer token via TokenReview,
// requiring our audience.
func (a *APIServer) authenticateBearer(r *http.Request) (authv1.UserInfo, error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return authv1.UserInfo{}, errors.New("missing bearer token")
	}
	tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if tok == "" {
		return authv1.UserInfo{}, errors.New("empty bearer token")
	}
	return a.TokenReviewer.Review(r.Context(), tok, RequesterTokenAudience)
}

// ---- HTML approve/deny (human side) ----

type indexView struct {
	User      string
	UserEmail string
	Pending   []SudoRequest
	History   []SudoRequest
}

func (a *APIServer) indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	claims, err := a.authenticateHuman(r)
	if err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if !claims.IsInGroup(a.Config.AdminGroup) {
		http.Error(w, fmt.Sprintf("forbidden: requires group %q", a.Config.AdminGroup), http.StatusForbidden)
		return
	}

	var list SudoRequestList
	if err := a.Client.List(r.Context(), &list, client.InNamespace(ControllerNamespace)); err != nil {
		http.Error(w, "list requests: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].CreationTimestamp.After(list.Items[j].CreationTimestamp.Time)
	})

	view := indexView{
		User:      claims.PreferredUsername,
		UserEmail: claims.Email,
	}

	for _, sr := range list.Items {
		if sr.Status.Phase == PhasePending {
			view.Pending = append(view.Pending, sr)
		} else if sr.Status.Phase != "" {
			if len(view.History) < 20 {
				view.History = append(view.History, sr)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = a.Templates.ExecuteTemplate(w, "index.html", view)
}

type approveView struct {
	UID                     string
	Token                   string
	Requester               string
	Reason                  string
	Command                 string
	Image                   string
	Profile                 string
	PreflightWarnings       []string
	Stdin                   string
	Extras                  specExtrasView
	PodTemplate             string
	Summary                 string
	PermissionRequest       string
	PermissionEffects       []string
	AssessmentModel         string
	AssessmentPromptVersion string
	AssessmentSchemaVersion string
	CreatedAt               string
	User                    string
	UserEmail               string
	Error                   string
	CSRFToken               string
}

const csrfCookieName = "__Host-sudo_service_csrf"

// resultView backs result.html, the styled confirmation page shown after an
// approve or deny action.
type resultView struct {
	Title     string
	Message   string
	UID       string
	Variant   string // "success" or "error"
	User      string
	UserEmail string
}

func (a *APIServer) approveHandler(w http.ResponseWriter, r *http.Request) {
	claims, err := a.authenticateHuman(r)
	if err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if !claims.IsInGroup(a.Config.AdminGroup) {
		http.Error(w, fmt.Sprintf("forbidden: requires group %q", a.Config.AdminGroup), http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet:
		a.renderApprovePage(w, r, claims)
	case http.MethodPost:
		a.handleApprovePost(w, r, claims)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *APIServer) renderApprovePage(w http.ResponseWriter, r *http.Request, claims *HumanClaims) {
	id := r.URL.Query().Get("id")
	token := r.URL.Query().Get("t")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	var sr *SudoRequest
	var err error
	if token != "" {
		sr, err = a.Reconciler.VerifyApprovalToken(r.Context(), types.UID(id), token)
		if errors.Is(err, errApprovalTokenExpired) && claims.IsInGroup(a.Config.AdminGroup) {
			sr, err = a.findByUID(r.Context(), types.UID(id))
			if err == nil && sr.Status.Phase == PhasePending {
				token = ""
			}
		}
	} else if claims.IsInGroup(a.Config.AdminGroup) {
		sr, err = a.findByUID(r.Context(), types.UID(id))
	} else {
		http.Error(w, "missing token or not an admin", http.StatusUnauthorized)
		return
	}

	csrfToken, csrfErr := ensureCSRFCookie(w, r)
	view := approveView{
		UID: id, Token: token,
		User: claims.PreferredUsername, UserEmail: claims.Email,
		CSRFToken: csrfToken,
	}
	if csrfErr != nil {
		view.Error = "could not prepare the approval form"
	}
	if err != nil {
		view.Error = err.Error()
	} else {
		view.Requester = sr.Spec.Requester
		view.Reason = sr.Spec.Reason
		view.Command = sr.Spec.Command
		view.Image = imageFor(sr)
		view.Profile = profileFor(sr)
		view.PreflightWarnings = sr.Status.PreflightWarnings
		view.Stdin = sr.Spec.Stdin
		view.Extras = newSpecExtrasView(sr, false)
		// Ground-truth pod spec (raw — the approve page is OIDC-protected). On the
		// off chance it can't render, the curated rows above still stand.
		if tmpl, err := displayPodTemplate(sr, false); err == nil {
			view.PodTemplate = tmpl
		}
		view.Summary = sr.Status.Summary
		if assessment := sr.Status.PermissionAssessment; assessment != nil {
			view.PermissionRequest = assessment.Request
			view.PermissionEffects = make([]string, len(assessment.Effects))
			for i, effect := range assessment.Effects {
				view.PermissionEffects[i] = permissionEffectLabel(effect)
			}
			view.AssessmentModel = assessment.Model
			view.AssessmentPromptVersion = assessment.PromptVersion
			view.AssessmentSchemaVersion = assessment.SchemaVersion
		}
		view.CreatedAt = sr.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z")
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = a.Templates.ExecuteTemplate(w, "approve.html", view)
}

func (a *APIServer) handleApprovePost(w http.ResponseWriter, r *http.Request, claims *HumanClaims) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id := r.Form.Get("id")
	token := r.Form.Get("t")
	if err := validateCSRF(r); err != nil {
		http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
		return
	}

	if token != "" {
		if _, err := a.Reconciler.VerifyApprovalToken(r.Context(), types.UID(id), token); err != nil {
			if !errors.Is(err, errApprovalTokenExpired) || !claims.IsInGroup(a.Config.AdminGroup) {
				http.Error(w, "approval token check failed: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
	} else if !claims.IsInGroup(a.Config.AdminGroup) {
		http.Error(w, "forbidden: admin required for token-less approval", http.StatusForbidden)
		return
	}

	approvedBy := claims.PreferredUsername
	if approvedBy == "" {
		approvedBy = claims.Subject
	}
	if err := a.Reconciler.Approve(r.Context(), types.UID(id), approvedBy); err != nil {
		http.Error(w, "approve: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = a.Templates.ExecuteTemplate(w, "result.html", resultView{
		Title:     "Approved",
		Message:   "This request will execute shortly.",
		UID:       id,
		Variant:   "success",
		User:      claims.PreferredUsername,
		UserEmail: claims.Email,
	})
}

func (a *APIServer) denyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	claims, err := a.authenticateHuman(r)
	if err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if !claims.IsInGroup(a.Config.AdminGroup) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	a.denyHandlerWithClaims(w, r, claims)
}

func (a *APIServer) denyHandlerWithClaims(w http.ResponseWriter, r *http.Request, claims *HumanClaims) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id := r.Form.Get("id")
	token := r.Form.Get("t")
	reason := r.Form.Get("reason")
	if err := validateCSRF(r); err != nil {
		http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
		return
	}

	if token != "" {
		if _, err := a.Reconciler.VerifyApprovalToken(r.Context(), types.UID(id), token); err != nil {
			if !errors.Is(err, errApprovalTokenExpired) || !claims.IsInGroup(a.Config.AdminGroup) {
				http.Error(w, "approval token check failed: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
	} else if !claims.IsInGroup(a.Config.AdminGroup) {
		http.Error(w, "forbidden: admin required for token-less denial", http.StatusForbidden)
		return
	}

	deniedBy := claims.PreferredUsername
	if deniedBy == "" {
		deniedBy = claims.Subject
	}
	if err := a.Reconciler.Deny(r.Context(), types.UID(id), deniedBy, reason); err != nil {
		http.Error(w, "deny: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = a.Templates.ExecuteTemplate(w, "result.html", resultView{
		Title:     "Denied",
		Message:   "The requester has been notified.",
		UID:       id,
		Variant:   "error",
		User:      claims.PreferredUsername,
		UserEmail: claims.Email,
	})
}

func ensureCSRFCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	var token string
	if existing, err := r.Cookie(csrfCookieName); err == nil && existing.Value != "" {
		token = existing.Value
	} else {
		var err error
		token, err = randomToken(32)
		if err != nil {
			return "", err
		}
	}
	// Refresh Max-Age on every rendered form. Reusing the token keeps multiple
	// concurrently-open review tabs valid while giving each render a full review
	// window before the browser expires the cookie.
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: token, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: PendingRequestTTL,
	})
	return token, nil
}

func validateCSRF(r *http.Request) error {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return errors.New("missing CSRF cookie")
	}
	formToken := r.Form.Get("csrf_token")
	if formToken == "" || !constantTimeEqual(cookie.Value, formToken) {
		return errors.New("invalid CSRF token")
	}
	return nil
}

// authenticateHuman extracts the JWT from the request and verifies it against Keycloak's JWKS.
// In oauth2-proxy mode, the JWT is in X-Auth-Request-Id-Token or -Access-Token; we IGNORE
// X-Auth-Request-{User,Groups,Email} (forgeable) and re-derive identity from the verified claims.
// In DIY OIDC mode, the JWT is in our own signed session cookie. (Stub for now — production runs
// with oauth2-proxy.)
//
// Order matters: ID tokens are the canonical OIDC identity source and reliably carry
// aud=<client_id>; Keycloak access tokens require a custom audience mapper to do the same
// (default access-token aud is "account"). Prefer the ID token, fall back to the access token,
// then to a raw Authorization: Bearer (handy for ad-hoc curl testing).
func (a *APIServer) authenticateHuman(r *http.Request) (*HumanClaims, error) {
	candidates := make([]string, 0, 3)
	if v := r.Header.Get("X-Auth-Request-Id-Token"); v != "" {
		candidates = append(candidates, v)
	}
	if v := r.Header.Get("X-Auth-Request-Access-Token"); v != "" {
		candidates = append(candidates, v)
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		candidates = append(candidates, strings.TrimPrefix(auth, "Bearer "))
	}
	if len(candidates) == 0 {
		return nil, errors.New("no JWT in request")
	}
	var lastErr error
	for _, raw := range candidates {
		claims, err := a.Verifier.Verify(r.Context(), raw)
		if err == nil {
			return claims, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("JWT verification failed: %w", lastErr)
}

func (a *APIServer) globalEventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	claims, err := a.authenticateHuman(r)
	if err != nil {
		http.Error(w, "unauthorized: "+err.Error(), http.StatusUnauthorized)
		return
	}
	if !claims.IsInGroup(a.Config.AdminGroup) {
		http.Error(w, fmt.Sprintf("forbidden: requires group %q", a.Config.AdminGroup), http.StatusForbidden)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch, cancel := a.Broadcaster.SubscribeAll()
	defer cancel()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			writeSSE(w, ev)
			flusher.Flush()
		}
	}
}

// findByUID lists SudoRequests in the controller namespace and returns the matching one.
// Cached by the manager, so this is a hashmap lookup after the first reconcile.
func (a *APIServer) findByUID(ctx context.Context, uid types.UID) (*SudoRequest, error) {
	var list SudoRequestList
	if err := a.Client.List(ctx, &list, client.InNamespace(ControllerNamespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].UID == uid {
			return &list.Items[i], nil
		}
	}
	return nil, apierrors.NewNotFound(GroupVersionResource.GroupResource(), string(uid))
}
