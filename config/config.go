// Package config loads and validates incidentd configuration.
//
// Non-secret settings live in a YAML file. Secrets (API keys, tokens,
// signing secrets, the database DSN) are never read from the file: each is
// read from an environment variable NAME or from a file whose path is in
// NAME_FILE (Kubernetes Secret / Vault agent mounts). Every integration is
// off unless enabled, so keys can be supplied at deployment time.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Secret names (env var, or NAME_FILE pointing at a file).
const (
	SecretDatabaseURL      = "DATABASE_URL"
	SecretSlackBotToken    = "SLACK_BOT_TOKEN"
	SecretSlackSigning     = "SLACK_SIGNING_SECRET"
	SecretTeamsAppPassword = "TEAMS_APP_PASSWORD"
	SecretPagerDutyToken   = "PAGERDUTY_API_TOKEN"
	SecretPagerDutyWebhook = "PAGERDUTY_WEBHOOK_SECRET"
	SecretDatadogAPIKey    = "DATADOG_API_KEY"
	SecretDatadogAppKey    = "DATADOG_APP_KEY"
	SecretJiraAPIToken     = "JIRA_API_TOKEN"
	SecretMultiplexerToken = "MULTIPLEXER_TOKEN"
	SecretAnthropicAPIKey  = "ANTHROPIC_API_KEY"
	SecretInternalAPIToken = "INCIDENTD_ADMIN_TOKEN"
	EnvRemediationEnabled  = "REMEDIATION_ENABLED"
)

// Duration is a YAML-friendly time.Duration ("90s", "15m").
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is the full configuration.
type Config struct {
	Server             Server      `yaml:"server"`
	LogLevel           string      `yaml:"logLevel"`
	Database           Database    `yaml:"database"`
	Incident           Incident    `yaml:"incident"`
	SeverityPolicyFile string      `yaml:"severityPolicyFile"`
	Slack              Slack       `yaml:"slack"`
	Teams              Teams       `yaml:"teams"`
	PagerDuty          PagerDuty   `yaml:"pagerduty"`
	Datadog            Datadog     `yaml:"datadog"`
	Jira               Jira        `yaml:"jira"`
	Kubernetes         Kubernetes  `yaml:"kubernetes"`
	Multiplexer        Multiplexer `yaml:"multiplexer"`
	LLM                LLM         `yaml:"llm"`
	Redaction          Redaction   `yaml:"redaction"`
	Remediation        Remediation `yaml:"remediation"`

	secrets map[string]string
}

// Server settings.
type Server struct {
	Listen string `yaml:"listen"`
	// MetricsListen serves Prometheus /metrics on a separate, internal-only
	// port (not exposed through the ingress). Empty disables it.
	MetricsListen string `yaml:"metricsListen"`
}

// Database settings. The DSN is the DATABASE_URL secret.
type Database struct {
	// Driver: postgres | memory (memory = no persistence; local dev/tests only).
	Driver string `yaml:"driver"`
	// Schema is the dedicated Postgres schema (Q3).
	Schema string `yaml:"schema"`
}

// Incident coordination settings.
type Incident struct {
	DefaultPlatform  string           `yaml:"defaultPlatform"`
	WarRoomPrefix    string           `yaml:"warRoomPrefix"`
	PrivateByDefault bool             `yaml:"privateByDefault"`
	PageAtOrAbove    int              `yaml:"pageAtOrAbove"`
	TicketAtOrAbove  int              `yaml:"ticketAtOrAbove"`
	StatusCadence    map[int]Duration `yaml:"statusCadence"`
	Admins           []string         `yaml:"admins"`
}

// Slack settings. Secrets: SLACK_BOT_TOKEN, SLACK_SIGNING_SECRET.
type Slack struct {
	Enabled           bool   `yaml:"enabled"`
	AnnounceChannelID string `yaml:"announceChannelId"`
	APIURL            string `yaml:"apiUrl"` // override for tests; default https://slack.com/api/
}

// Teams settings. Secret: TEAMS_APP_PASSWORD.
type Teams struct {
	Enabled bool   `yaml:"enabled"`
	AppID   string `yaml:"appId"`
	// TenantID selects the single-tenant token endpoint; empty = multi-tenant
	// (login.microsoftonline.com/botframework.com).
	TenantID               string `yaml:"tenantId"`
	AnnounceConversationID string `yaml:"announceConversationId"`
	AnnounceServiceURL     string `yaml:"announceServiceUrl"`
}

// PagerDuty settings. Secrets: PAGERDUTY_API_TOKEN, PAGERDUTY_WEBHOOK_SECRET.
type PagerDuty struct {
	Enabled bool   `yaml:"enabled"`
	APIURL  string `yaml:"apiUrl"`
	// FromEmail is required by the PagerDuty REST API for write calls.
	FromEmail string `yaml:"fromEmail"`
	// Services maps our service names to PagerDuty service IDs.
	Services map[string]string `yaml:"services"`
	// DefaultService is used when an incident has no mapped service.
	DefaultService string `yaml:"defaultService"`
}

// Datadog settings. Secrets: DATADOG_API_KEY, DATADOG_APP_KEY.
type Datadog struct {
	Enabled bool   `yaml:"enabled"`
	Site    string `yaml:"site"`   // datadoghq.com, datadoghq.eu, us3.datadoghq.com …
	APIURL  string `yaml:"apiUrl"` // override; default https://api.<site>
}

// Jira settings (Q6: configurable). Secret: JIRA_API_TOKEN.
type Jira struct {
	Enabled             bool   `yaml:"enabled"`
	BaseURL             string `yaml:"baseUrl"`
	Email               string `yaml:"email"`
	ProjectKey          string `yaml:"projectKey"`
	IssueType           string `yaml:"issueType"`
	ActionItemIssueType string `yaml:"actionItemIssueType"`
}

// Kubernetes settings (Q5: configurable at deployment).
type Kubernetes struct {
	Enabled  bool      `yaml:"enabled"`
	Clusters []Cluster `yaml:"clusters"`
}

// Cluster is one cluster the bot may read.
type Cluster struct {
	Name           string   `yaml:"name"`
	InCluster      bool     `yaml:"inCluster"`
	KubeconfigPath string   `yaml:"kubeconfigPath"`
	Context        string   `yaml:"context"`
	Namespaces     []string `yaml:"namespaces"` // allow-list; glob suffix * allowed
}

// Multiplexer settings. Secret: MULTIPLEXER_TOKEN (optional until gap G2).
type Multiplexer struct {
	Enabled      bool     `yaml:"enabled"`
	BaseURL      string   `yaml:"baseUrl"`
	PollInterval Duration `yaml:"pollInterval"`
}

// LLM settings. Secret: ANTHROPIC_API_KEY (provider anthropic).
type LLM struct {
	Enabled   bool   `yaml:"enabled"`
	Provider  string `yaml:"provider"` // anthropic | bedrock | vertex
	Model     string `yaml:"model"`
	Effort    string `yaml:"effort"` // low | medium | high | xhigh | max
	MaxTokens int    `yaml:"maxTokens"`
	// Fallbacks enables server-side refusal fallbacks ("default" | "off").
	Fallbacks string `yaml:"fallbacks"`
	BaseURL   string `yaml:"baseUrl"` // optional private endpoint / gateway
	Triage    Triage `yaml:"triage"`
}

// Triage budgets.
type Triage struct {
	MaxToolCalls       int      `yaml:"maxToolCalls"`
	Timeout            Duration `yaml:"timeout"`
	MaxToolOutputBytes int      `yaml:"maxToolOutputBytes"`
	LookbackWindow     Duration `yaml:"lookbackWindow"`
}

// Redaction settings.
type Redaction struct {
	HashEmails    bool     `yaml:"hashEmails"`
	ExtraPatterns []string `yaml:"extraPatterns"`
}

// Remediation settings. Execution additionally requires env
// REMEDIATION_ENABLED=true AND the runtime DB flag (two-key kill switch).
type Remediation struct {
	CatalogDir    string            `yaml:"catalogDir"`
	ApprovalTTL   Duration          `yaml:"approvalTTL"`
	ServiceOwners map[string]string `yaml:"serviceOwners"` // service -> owner email
}

// Load reads path, applies defaults, reads secrets and validates.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(b, os.Getenv)
}

// Parse parses YAML with an injectable env lookup (for tests).
func Parse(b []byte, getenv func(string) string) (*Config, error) {
	c := Defaults()
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.secrets = map[string]string{}
	for _, name := range []string{SecretDatabaseURL, SecretSlackBotToken, SecretSlackSigning, SecretTeamsAppPassword,
		SecretPagerDutyToken, SecretPagerDutyWebhook, SecretDatadogAPIKey, SecretDatadogAppKey, SecretJiraAPIToken,
		SecretMultiplexerToken, SecretAnthropicAPIKey, SecretInternalAPIToken} {
		v, err := readSecret(name, getenv)
		if err != nil {
			return nil, err
		}
		c.secrets[name] = v
	}
	c.secrets[EnvRemediationEnabled] = getenv(EnvRemediationEnabled)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func readSecret(name string, getenv func(string) string) (string, error) {
	if v := getenv(name); v != "" {
		return strings.TrimSpace(v), nil
	}
	if p := getenv(name + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read secret %s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

// Secret returns a loaded secret ("" if unset). Never log the result.
func (c *Config) Secret(name string) string { return c.secrets[name] }

// RemediationEnvEnabled reports the env half of the kill switch.
func (c *Config) RemediationEnvEnabled() bool {
	return strings.EqualFold(c.secrets[EnvRemediationEnabled], "true")
}

// Defaults returns a config with safe defaults.
func Defaults() *Config {
	return &Config{
		Server:   Server{Listen: ":8080", MetricsListen: ":9090"},
		LogLevel: "info",
		Database: Database{Driver: "postgres", Schema: "incident_bot"},
		Incident: Incident{
			DefaultPlatform: "slack",
			WarRoomPrefix:   "inc-",
			PageAtOrAbove:   2,
			TicketAtOrAbove: 3,
			StatusCadence:   map[int]Duration{1: Duration(30 * time.Minute), 2: Duration(time.Hour)},
		},
		PagerDuty:   PagerDuty{APIURL: "https://api.pagerduty.com"},
		Datadog:     Datadog{Site: "datadoghq.com"},
		Jira:        Jira{IssueType: "Incident", ActionItemIssueType: "Task"},
		Multiplexer: Multiplexer{PollInterval: Duration(60 * time.Second)},
		LLM: LLM{
			Provider: "anthropic", Model: "claude-opus-5-5", Effort: "high", MaxTokens: 16000, Fallbacks: "default",
			Triage: Triage{MaxToolCalls: 15, Timeout: Duration(90 * time.Second), MaxToolOutputBytes: 8192, LookbackWindow: Duration(2 * time.Hour)},
		},
		Redaction:   Redaction{HashEmails: true},
		Remediation: Remediation{CatalogDir: "remediation/catalog", ApprovalTTL: Duration(15 * time.Minute)},
	}
}

// Validate collects every problem so operators fix them in one pass.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	need := func(enabled bool, section, secret string) {
		if enabled && c.secrets[secret] == "" {
			add("%s is enabled but secret %s (or %s_FILE) is not set", section, secret, secret)
		}
	}
	urlOK := func(field, v string) {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
			add("%s must be an absolute http(s) URL, got %q", field, v)
		}
	}

	switch c.Database.Driver {
	case "postgres":
		need(true, "database (postgres)", SecretDatabaseURL)
		if c.Database.Schema == "" {
			add("database.schema is required")
		}
	case "memory":
	default:
		add("database.driver must be postgres or memory")
	}
	if c.Incident.PageAtOrAbove < 0 || c.Incident.PageAtOrAbove > 4 || c.Incident.TicketAtOrAbove < 0 || c.Incident.TicketAtOrAbove > 4 {
		add("incident.pageAtOrAbove / ticketAtOrAbove must be 0 (off) or 1..4")
	}
	if !c.Slack.Enabled && !c.Teams.Enabled {
		add("at least one chat platform (slack or teams) must be enabled")
	}
	switch c.Incident.DefaultPlatform {
	case "slack":
		if !c.Slack.Enabled {
			add("incident.defaultPlatform is slack but slack is disabled")
		}
	case "teams":
		if !c.Teams.Enabled {
			add("incident.defaultPlatform is teams but teams is disabled")
		}
	default:
		add("incident.defaultPlatform must be slack or teams")
	}
	need(c.Slack.Enabled, "slack", SecretSlackBotToken)
	need(c.Slack.Enabled, "slack", SecretSlackSigning)
	if c.Teams.Enabled {
		need(true, "teams", SecretTeamsAppPassword)
		if c.Teams.AppID == "" {
			add("teams.appId is required")
		}
	}
	if c.PagerDuty.Enabled {
		need(true, "pagerduty", SecretPagerDutyToken)
		need(true, "pagerduty", SecretPagerDutyWebhook)
		urlOK("pagerduty.apiUrl", c.PagerDuty.APIURL)
		if c.PagerDuty.FromEmail == "" {
			add("pagerduty.fromEmail is required (PagerDuty requires a From header on writes)")
		}
	}
	if c.Datadog.Enabled {
		need(true, "datadog", SecretDatadogAPIKey)
		need(true, "datadog", SecretDatadogAppKey)
		if c.Datadog.APIURL != "" {
			urlOK("datadog.apiUrl", c.Datadog.APIURL)
		}
	}
	if c.Jira.Enabled {
		need(true, "jira", SecretJiraAPIToken)
		urlOK("jira.baseUrl", c.Jira.BaseURL)
		if c.Jira.Email == "" || c.Jira.ProjectKey == "" || c.Jira.IssueType == "" {
			add("jira.email, jira.projectKey and jira.issueType are required")
		}
	}
	if c.Kubernetes.Enabled {
		if len(c.Kubernetes.Clusters) == 0 {
			add("kubernetes.clusters must list at least one cluster")
		}
		for i, cl := range c.Kubernetes.Clusters {
			if cl.Name == "" {
				add("kubernetes.clusters[%d].name is required", i)
			}
			if !cl.InCluster && cl.KubeconfigPath == "" {
				add("kubernetes.clusters[%d] needs inCluster: true or kubeconfigPath", i)
			}
			if len(cl.Namespaces) == 0 {
				add("kubernetes.clusters[%d].namespaces allow-list is required", i)
			}
		}
	}
	if c.Multiplexer.Enabled {
		urlOK("multiplexer.baseUrl", c.Multiplexer.BaseURL)
		if c.Multiplexer.PollInterval.D() < 10*time.Second {
			add("multiplexer.pollInterval must be >= 10s")
		}
	}
	if c.LLM.Enabled {
		switch c.LLM.Provider {
		case "anthropic":
			need(true, "llm (anthropic)", SecretAnthropicAPIKey)
		case "bedrock", "vertex":
			// Stub adapters: they fail loudly at call time (planned v2).
		default:
			add("llm.provider must be anthropic, bedrock or vertex")
		}
		switch c.LLM.Effort {
		case "low", "medium", "high", "xhigh", "max":
		default:
			add("llm.effort must be low|medium|high|xhigh|max")
		}
		if c.LLM.Fallbacks != "default" && c.LLM.Fallbacks != "off" {
			add("llm.fallbacks must be default or off")
		}
		t := c.LLM.Triage
		if t.MaxToolCalls < 1 || t.Timeout.D() <= 0 || t.MaxToolOutputBytes < 512 {
			add("llm.triage budgets must be positive (maxToolOutputBytes >= 512)")
		}
	}
	if c.Remediation.ApprovalTTL.D() <= 0 {
		add("remediation.approvalTTL must be positive")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}
