package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = addKnownTypes(scheme)
}

// Config bundles env-derived runtime settings.
type Config struct {
	BindAddress           string // e.g. "0.0.0.0:8080" or "127.0.0.1:8080"
	PublicBaseURL         string // e.g. "https://sudo.andrewgarrett.dev"
	PushoverToken         string // Pushover application token
	PushoverUserKey       string // Pushover user or group key (`user` field on the API)
	KeycloakIssuer        string // e.g. "https://id.andrewgarrett.dev/realms/master"
	KeycloakAudience      string // expected aud claim
	AdminGroup            string // e.g. "cluster-admins"
	CookieSigningKey      string // for DIY OIDC sessions; optional in oauth2-proxy mode
	OAuth2ProxyMode       bool   // true: trust forwarded ID token after JWKS check
	AutoApproveConfigPath string // path to auto-approve YAML

	// Optional AI command-summary feature. Enabled only when OpenAIAPIKey is set.
	OpenAIAPIKey  string // OPENAI_API_KEY; empty disables the feature
	OpenAIBaseURL string // OPENAI_BASE_URL; defaults to the OpenAI public API
	OpenAIModel   string // OPENAI_MODEL; defaults to DefaultOpenAIModel
}

func loadConfig() (*Config, error) {
	cfg := &Config{
		BindAddress:           getenv("BIND_ADDRESS", "127.0.0.1:8080"),
		PublicBaseURL:         getenv("PUBLIC_BASE_URL", "https://sudo.andrewgarrett.dev"),
		PushoverToken:         os.Getenv("PUSHOVER_TOKEN"),
		PushoverUserKey:       os.Getenv("PUSHOVER_USER_KEY"),
		KeycloakIssuer:        getenv("KEYCLOAK_ISSUER", "https://id.andrewgarrett.dev/realms/master"),
		KeycloakAudience:      getenv("KEYCLOAK_AUDIENCE", "sudo-service"),
		AdminGroup:            getenv("ADMIN_GROUP", "cluster-admins"),
		CookieSigningKey:      os.Getenv("COOKIE_SIGNING_KEY"),
		OAuth2ProxyMode:       strings.EqualFold(os.Getenv("OAUTH2_PROXY_MODE"), "true"),
		AutoApproveConfigPath: os.Getenv("AUTO_APPROVE_CONFIG"),
		OpenAIAPIKey:          os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:         getenv("OPENAI_BASE_URL", DefaultOpenAIBaseURL),
		OpenAIModel:           getenv("OPENAI_MODEL", DefaultOpenAIModel),
	}
	if cfg.PushoverToken == "" || cfg.PushoverUserKey == "" {
		return nil, fmt.Errorf("PUSHOVER_TOKEN and PUSHOVER_USER_KEY are required")
	}
	return cfg, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	logger := ctrl.Log.WithName("sudo-service")

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if err := LoadAutoApproveConfig(cfg.AutoApproveConfigPath); err != nil {
		logger.Error(err, "failed to load auto-approve config")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			// Watching only our own namespace keeps RBAC scoped and avoids needing
			// cluster-wide list/watch on the resources we manage.
			DefaultNamespaces: map[string]cache.Config{ControllerNamespace: {}},
		},
		// Disable the manager's separate metrics listener — the default is :8080,
		// which collides with our main HTTP server. We mount /metrics onto the
		// same mux below using ctrlmetrics.Registry.
		Metrics:        metricsserver.Options{BindAddress: "0"},
		LeaderElection: false,
	})
	if err != nil {
		logger.Error(err, "unable to start manager")
		os.Exit(1)
	}

	verifier, err := NewJWTVerifier(context.Background(), cfg.KeycloakIssuer, cfg.KeycloakAudience)
	if err != nil {
		logger.Error(err, "init JWT verifier")
		os.Exit(1)
	}

	tokenReviewer, err := NewTokenReviewer()
	if err != nil {
		logger.Error(err, "init TokenReviewer")
		os.Exit(1)
	}

	po := NewPushoverClient(cfg.PushoverToken, cfg.PushoverUserKey)

	// Optional: AI command summaries. nil when OPENAI_API_KEY is unset.
	summarizer := NewSummarizer(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL, cfg.OpenAIModel)
	if summarizer != nil {
		logger.Info("AI command summaries enabled", "baseURL", cfg.OpenAIBaseURL, "model", cfg.OpenAIModel)
	}

	broadcaster := NewBroadcaster()

	reconciler := &SudoRequestReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Pushover:      po,
		Summarizer:    summarizer,
		Broadcaster:   broadcaster,
		PublicBaseURL: cfg.PublicBaseURL,
		Recorder:      mgr.GetEventRecorderFor("sudo-service"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "setup reconciler")
		os.Exit(1)
	}

	// Background GC for output Secrets and finished Jobs.
	gc := &GarbageCollector{Client: mgr.GetClient(), Broadcaster: broadcaster}
	if err := mgr.Add(gc); err != nil {
		logger.Error(err, "add GC")
		os.Exit(1)
	}

	tmpls := template.Must(template.New("base").Funcs(template.FuncMap{
		"Lower": strings.ToLower,
	}).ParseFS(templatesFS, "templates/*.html"))

	api := &APIServer{
		Client:        mgr.GetClient(),
		Verifier:      verifier,
		TokenReviewer: tokenReviewer,
		Broadcaster:   broadcaster,
		Reconciler:    reconciler,
		Config:        cfg,
		Templates:     tmpls,
	}

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	// Serve controller-runtime metrics on the same listener as the rest of the
	// API instead of letting the manager spin up its own listener on :8080
	// (which would race with ours). Scraped in-cluster via NetworkPolicy +
	// ServiceMonitor — not exposed through oauth2-proxy.
	mux.Handle("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))

	srv := &http.Server{
		Addr:              cfg.BindAddress,
		Handler:           withLogging(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("HTTP listening", "addr", cfg.BindAddress)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "http server")
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("starting manager")
		if err := mgr.Start(ctx); err != nil {
			logger.Error(err, "manager exited")
			cancel()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	wg.Wait()
}

// withLogging is a tiny request logger.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lrw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, lrw.status, time.Since(start))
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (l *loggingResponseWriter) WriteHeader(code int) {
	l.status = code
	l.ResponseWriter.WriteHeader(code)
}

// Convenience: caller wants the manager's client typed.
var _ client.Client = (client.Client)(nil)
