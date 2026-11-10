// Command incidentd is the incident management bot.
//
//	incidentd [serve]            run the HTTP service (default)
//	incidentd config-check       validate configuration and exit
//	incidentd verify-timeline K  verify the hash chain of incident K
//	incidentd eval ...           run the triage evaluation (see eval.go)
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	slackchat "github.com/streetsmartops/incident-management-chatbot/chat/slack"
	teamschat "github.com/streetsmartops/incident-management-chatbot/chat/teams"
	"github.com/streetsmartops/incident-management-chatbot/config"
	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/db"
	"github.com/streetsmartops/incident-management-chatbot/ingest"
	"github.com/streetsmartops/incident-management-chatbot/postmortem"
	anthropicllm "github.com/streetsmartops/incident-management-chatbot/providers/anthropic"
	"github.com/streetsmartops/incident-management-chatbot/providers/datadog"
	"github.com/streetsmartops/incident-management-chatbot/providers/jira"
	"github.com/streetsmartops/incident-management-chatbot/providers/kubernetes"
	"github.com/streetsmartops/incident-management-chatbot/providers/multiplexer"
	"github.com/streetsmartops/incident-management-chatbot/providers/pagerduty"
	"github.com/streetsmartops/incident-management-chatbot/providers/stubs"
	"github.com/streetsmartops/incident-management-chatbot/remediation"
	"github.com/streetsmartops/incident-management-chatbot/triage"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "config-check":
		err = configCheck(args)
	case "verify-timeline":
		err = verifyTimeline(args)
	case "eval":
		err = runEval(args)
	default:
		err = fmt.Errorf("unknown command %q (serve | config-check | verify-timeline | eval)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "incidentd:", err)
		os.Exit(1)
	}
}

func configPath(fs *flag.FlagSet, args []string) (string, error) {
	p := fs.String("config", envOr("INCIDENTD_CONFIG", "config/config.yaml"), "path to config YAML")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return *p, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// App holds wired components.
type App struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Store    core.Store
	Service  *core.Service
	Slack    *slackchat.Handler
	Teams    *teamschat.Handler
	PDHook   *ingest.PagerDutyWebhook
	Poller   *ingest.Poller
	Remedy   *remediation.Engine
	ping     func(context.Context) error
	closeFns []func()
}

// Build wires everything from config. Exported for tests.
func Build(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	app := &App{Cfg: cfg, Log: log, ping: func(context.Context) error { return nil }}
	switch cfg.Database.Driver {
	case "postgres":
		s, err := db.Open(ctx, cfg.Secret(config.SecretDatabaseURL), cfg.Database.Schema)
		if err != nil {
			return nil, fmt.Errorf("database: %w", err)
		}
		app.Store, app.ping = s, s.Ping
		app.closeFns = append(app.closeFns, s.Close)
	default:
		log.Warn("using in-memory store: data is lost on restart (local development only)")
		app.Store = core.NewMemStore()
	}

	redactor, err := core.NewRedactor(cfg.Redaction.HashEmails, cfg.Redaction.ExtraPatterns)
	if err != nil {
		return nil, fmt.Errorf("redaction.extraPatterns: %w", err)
	}
	policy := core.DefaultSeverityPolicy()
	if cfg.SeverityPolicyFile != "" {
		if policy, err = core.LoadSeverityPolicy(cfg.SeverityPolicyFile); err != nil {
			return nil, err
		}
	}
	log.Info("severity policy loaded", "hash", policy.Hash, "file", cfg.SeverityPolicyFile)

	ints := core.Integrations{Chats: map[string]core.ChatPort{}}
	announce := map[string]core.ChannelRef{}
	var slackClient *slackchat.Client
	if cfg.Slack.Enabled {
		slackClient = slackchat.New(cfg.Secret(config.SecretSlackBotToken), cfg.Slack.APIURL, nil)
		ints.Chats["slack"] = slackClient
		if cfg.Slack.AnnounceChannelID != "" {
			announce["slack"] = core.ChannelRef{Platform: "slack", ID: cfg.Slack.AnnounceChannelID}
		}
	}
	var teamsClient *teamschat.Client
	if cfg.Teams.Enabled {
		teamsClient = teamschat.New(cfg.Teams.AppID, cfg.Secret(config.SecretTeamsAppPassword), cfg.Teams.TenantID, nil)
		ints.Chats["teams"] = teamsClient
		if cfg.Teams.AnnounceConversationID != "" {
			announce["teams"] = core.ChannelRef{Platform: "teams", ID: cfg.Teams.AnnounceConversationID, ServiceURL: cfg.Teams.AnnounceServiceURL}
		}
	}
	var pd *pagerduty.Client
	if cfg.PagerDuty.Enabled {
		pd = pagerduty.New(pagerduty.Config{
			APIURL: cfg.PagerDuty.APIURL, Token: cfg.Secret(config.SecretPagerDutyToken), WebhookSecret: cfg.Secret(config.SecretPagerDutyWebhook),
			FromEmail: cfg.PagerDuty.FromEmail, Services: cfg.PagerDuty.Services, DefaultService: cfg.PagerDuty.DefaultService,
		}, nil)
		ints.Paging = pd
	}
	if cfg.Jira.Enabled {
		ints.Itsm = jira.New(jira.Config{BaseURL: cfg.Jira.BaseURL, Email: cfg.Jira.Email, APIToken: cfg.Secret(config.SecretJiraAPIToken),
			ProjectKey: cfg.Jira.ProjectKey, IssueType: cfg.Jira.IssueType, ActionItemIssueType: cfg.Jira.ActionItemIssueType}, nil)
	}
	var mpx *multiplexer.Client
	if cfg.Multiplexer.Enabled {
		if mpx, err = multiplexer.New(cfg.Multiplexer.BaseURL, cfg.Secret(config.SecretMultiplexerToken), nil); err != nil {
			return nil, err
		}
		ints.Slo = mpx
	}
	var obs core.ObservabilityProvider
	if cfg.Datadog.Enabled {
		obs = datadog.New(cfg.Datadog.Site, cfg.Datadog.APIURL, cfg.Secret(config.SecretDatadogAPIKey), cfg.Secret(config.SecretDatadogAppKey), nil)
	}
	var k8s *kubernetes.Provider
	if cfg.Kubernetes.Enabled {
		var cls []kubernetes.ClusterConfig
		for _, c := range cfg.Kubernetes.Clusters {
			cls = append(cls, kubernetes.ClusterConfig{Name: c.Name, InCluster: c.InCluster, KubeconfigPath: c.KubeconfigPath, Context: c.Context, Namespaces: c.Namespaces})
		}
		if k8s, err = kubernetes.Connect(cls); err != nil {
			return nil, err
		}
	}

	cadence := map[core.Severity]time.Duration{}
	for sev, d := range cfg.Incident.StatusCadence {
		cadence[core.Severity(sev)] = d.D()
	}
	svc := core.NewService(app.Store, ints, core.ServiceConfig{
		DefaultPlatform: cfg.Incident.DefaultPlatform, AnnounceChannels: announce, WarRoomPrefix: cfg.Incident.WarRoomPrefix,
		PrivateByDefault: cfg.Incident.PrivateByDefault, PageAtOrAbove: core.Severity(cfg.Incident.PageAtOrAbove),
		TicketAtOrAbove: core.Severity(cfg.Incident.TicketAtOrAbove), StatusCadence: cadence, Admins: cfg.Incident.Admins,
	}, policy, log)
	app.Service = svc

	// Remediation (catalog + approvals always on; execution behind the two-key kill switch).
	catalog, err := remediation.LoadCatalog(cfg.Remediation.CatalogDir)
	if err != nil {
		return nil, fmt.Errorf("remediation catalog: %w", err)
	}
	owners := func(ctx context.Context, service string) (string, bool) {
		if o, ok := cfg.Remediation.ServiceOwners[service]; ok {
			return o, true
		}
		if mpx != nil {
			return mpx.ServiceOwner(ctx, service)
		}
		return "", false
	}
	providers := map[string]core.CloudProvider{}
	if k8s != nil {
		providers["kubernetes"] = k8s
	}
	providers["aws"] = stubs.Cloud{Kind: "aws"}
	app.Remedy = &remediation.Engine{Catalog: catalog, Providers: providers, Store: app.Store, Recorder: svc, Itsm: ints.Itsm,
		Owners: owners, TTL: cfg.Remediation.ApprovalTTL.D(), EnvEnabled: cfg.RemediationEnvEnabled(), Now: time.Now, NewID: core.NewULIDish}
	if k8s != nil {
		k8s.ExecutorEnabled = app.Remedy.ExecutorEnabled
	}
	svc.Remediate = app.Remedy
	log.Info("remediation", "catalog", catalog.IDs(), "env_enabled", cfg.RemediationEnvEnabled())

	// LLM-backed engines.
	var llm core.LlmPort
	if cfg.LLM.Enabled {
		switch cfg.LLM.Provider {
		case "anthropic":
			llm = anthropicllm.New(anthropicllm.Config{APIKey: cfg.Secret(config.SecretAnthropicAPIKey), Model: cfg.LLM.Model, Effort: cfg.LLM.Effort,
				Fallbacks: cfg.LLM.Fallbacks == "default", BaseURL: cfg.LLM.BaseURL})
		default:
			llm = stubs.LLM{Kind: cfg.LLM.Provider}
		}
		var cloud core.CloudProvider
		if k8s != nil {
			cloud = k8s
		}
		svc.Triage = &triage.Engine{
			LLM: llm, Catalog: catalog, MaxToolCalls: cfg.LLM.Triage.MaxToolCalls, Timeout: cfg.LLM.Triage.Timeout.D(),
			MaxOutputBytes: cfg.LLM.Triage.MaxToolOutputBytes, MaxTokens: cfg.LLM.MaxTokens, Now: time.Now,
			Exec: &triage.LiveExecutor{Store: app.Store, Obs: obs, Cloud: cloud, Slo: ints.Slo, Paging: ints.Paging,
				Similar: &postmortem.Retriever{Store: app.Store}, Redactor: redactor, Lookback: cfg.LLM.Triage.LookbackWindow.D(), Now: time.Now},
		}
	}
	svc.Postmortem = &postmortem.Engine{Store: app.Store, Slo: ints.Slo, LLM: llm, Itsm: ints.Itsm, Redactor: redactor, Now: time.Now}

	if slackClient != nil {
		app.Slack = &slackchat.Handler{Client: slackClient, Service: svc, SigningSecret: cfg.Secret(config.SecretSlackSigning), Log: log, Now: time.Now}
	}
	if teamsClient != nil {
		app.Teams = &teamschat.Handler{Client: teamsClient, Validator: teamschat.NewValidator(cfg.Teams.AppID, nil), Service: svc, Log: log}
	}
	if pd != nil {
		app.PDHook = &ingest.PagerDutyWebhook{PD: pd, Slo: ints.Slo, Service: svc, Log: log}
	}
	if mpx != nil {
		app.Poller = &ingest.Poller{Slo: mpx, Store: app.Store, Service: svc, Interval: cfg.Multiplexer.PollInterval.D(), Log: log, Now: time.Now}
	}
	return app, nil
}

// Close releases resources.
func (a *App) Close() {
	a.Service.Wait()
	for _, f := range a.closeFns {
		f()
	}
}

// Routes builds the HTTP mux.
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := a.ping(ctx); err != nil {
			http.Error(w, "db not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	if a.Slack != nil {
		mux.HandleFunc("POST /slack/commands", a.Slack.ServeCommand)
		mux.HandleFunc("POST /slack/interactivity", a.Slack.ServeInteractivity)
	}
	if a.Teams != nil {
		mux.Handle("POST /teams/messages", a.Teams)
	}
	if a.PDHook != nil {
		mux.Handle("POST /webhooks/pagerduty", a.PDHook)
	}
	mux.HandleFunc("POST /admin/remediation", a.admin(a.setRemediation))
	mux.HandleFunc("GET /admin/timeline/verify", a.admin(a.verifyHandler))
	return a.logRequests(mux)
}

func (a *App) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := a.Cfg.Secret(config.SecretInternalAPIToken)
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *App) setRemediation(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query().Get("enabled")
	if v != "true" && v != "false" {
		http.Error(w, "enabled=true|false", http.StatusBadRequest)
		return
	}
	if err := a.Store.SetFlag(r.Context(), remediation.FlagRemediationEnabled, v == "true"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.Log.Warn("remediation runtime flag changed", "enabled", v, "remote", r.RemoteAddr)
	_ = json.NewEncoder(w).Encode(map[string]any{"runtimeFlag": v == "true", "envEnabled": a.Cfg.RemediationEnvEnabled(), "executorEnabled": a.Remedy.ExecutorEnabled(r.Context())})
}

func (a *App) verifyHandler(w http.ResponseWriter, r *http.Request) {
	res, err := verify(r.Context(), a.Store, r.URL.Query().Get("incident"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

func verify(ctx context.Context, store core.Store, key string) (map[string]any, error) {
	inc, err := store.FindIncidentByKey(ctx, strings.ToUpper(key))
	if err != nil {
		return nil, fmt.Errorf("incident %s: %w", key, err)
	}
	evs, err := store.Events(ctx, inc.ID)
	if err != nil {
		return nil, err
	}
	bad, verr := core.VerifyChain(evs)
	res := map[string]any{"incident": inc.Key, "events": len(evs), "intact": verr == nil}
	if verr != nil {
		res["firstBadSeq"], res["error"] = bad, verr.Error()
	}
	return res, nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }

func (a *App) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = core.NewULIDish()
		}
		w.Header().Set("X-Request-Id", rid)
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			a.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration_ms", time.Since(start).Milliseconds(), "request_id", rid)
		}
	})
}

func serve(args []string) error {
	path, err := configPath(flag.NewFlagSet("serve", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := Build(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer app.Close()

	if app.Poller != nil {
		go app.Poller.Run(ctx)
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				app.Service.Tick(ctx)
			}
		}
	}()
	srv := &http.Server{Addr: cfg.Server.Listen, Handler: app.Routes(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("incidentd listening", "addr", cfg.Server.Listen)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	log.Info("shutting down")
	return srv.Shutdown(shCtx)
}

func configCheck(args []string) error {
	path, err := configPath(flag.NewFlagSet("config-check", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if cfg.SeverityPolicyFile != "" {
		if _, err := core.LoadSeverityPolicy(cfg.SeverityPolicyFile); err != nil {
			return err
		}
	}
	if _, err := remediation.LoadCatalog(cfg.Remediation.CatalogDir); err != nil {
		return err
	}
	fmt.Println("configuration OK")
	return nil
}

func verifyTimeline(args []string) error {
	fs := flag.NewFlagSet("verify-timeline", flag.ExitOnError)
	path, err := configPath(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: incidentd verify-timeline [-config path] INC-YYYY-NNNN")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if cfg.Database.Driver != "postgres" {
		return errors.New("verify-timeline needs database.driver: postgres")
	}
	ctx := context.Background()
	s, err := db.Open(ctx, cfg.Secret(config.SecretDatabaseURL), cfg.Database.Schema)
	if err != nil {
		return err
	}
	defer s.Close()
	res, err := verify(ctx, s, fs.Arg(0))
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if res["intact"] != true {
		return errors.New("timeline integrity check FAILED")
	}
	return nil
}
