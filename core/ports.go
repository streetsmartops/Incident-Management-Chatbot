package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotSupported is returned by an adapter for a capability it lacks.
var ErrNotSupported = errors.New("not supported by this adapter")

// ErrNotConfigured is returned by stub adapters and disabled integrations.
var ErrNotConfigured = errors.New("adapter not configured")

// ErrExecutorDisabled is returned when remediation execution is switched off.
var ErrExecutorDisabled = errors.New("remediation executor disabled (dry-run only)")

// ---------------------------------------------------------------- Chat

// Button is a platform-neutral interactive control.
type Button struct {
	ActionID string `json:"actionId"` // e.g. "approve", "declare"
	Label    string `json:"label"`
	Value    string `json:"value"` // opaque, e.g. action request ID
	Style    string `json:"style,omitempty"` // primary | danger
}

// Field is a label/value pair rendered in a message.
type Field struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Message is a platform-neutral chat message. Adapters render it as Slack
// Block Kit or a Teams Adaptive Card.
type Message struct {
	Title   string   `json:"title,omitempty"`
	Text    string   `json:"text"`
	Fields  []Field  `json:"fields,omitempty"`
	Buttons []Button `json:"buttons,omitempty"`
	// Ephemeral replies are visible only to the invoking user where supported.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// MessageRef identifies a posted message so it can be updated.
type MessageRef struct {
	Channel ChannelRef `json:"channel"`
	ID      string     `json:"id"`
}

// ChatPort is implemented by chat platform adapters.
type ChatPort interface {
	Platform() string
	PostMessage(ctx context.Context, ch ChannelRef, m Message) (MessageRef, error)
	UpdateMessage(ctx context.Context, ref MessageRef, m Message) error
	// CreateWarRoom returns ErrNotSupported where the platform cannot create
	// channels from a bot (Teams without Graph); callers fall back to the
	// announce channel.
	CreateWarRoom(ctx context.Context, name string, private bool) (ChannelRef, error)
	Invite(ctx context.Context, ch ChannelRef, users []UserRef) error
	SetTopic(ctx context.Context, ch ChannelRef, topic string) error
	LookupUserByEmail(ctx context.Context, email string) (UserRef, error)
}

// ---------------------------------------------------------------- Paging

// PagingProvider is implemented by PagerDuty (v1) / Opsgenie (stub).
type PagingProvider interface {
	Name() string
	WhoIsOnCall(ctx context.Context, service string) ([]UserRef, error)
	// Page opens a page for the incident on the service; returns a page ref.
	Page(ctx context.Context, service string, inc *Incident, urgency string) (string, error)
	Resolve(ctx context.Context, pageRef string) error
	AddNote(ctx context.Context, pageRef, text string) error
}

// ---------------------------------------------------------------- Observability

// Capability names an observability feature, used to hide triage tools.
type Capability string

const (
	CapMetrics  Capability = "metrics"
	CapLogs     Capability = "logs"
	CapMonitors Capability = "monitors"
	CapEvents   Capability = "events"
)

// Series is a metric time series.
type Series struct {
	Metric string            `json:"metric"`
	Scope  string            `json:"scope"`
	Points [][2]float64      `json:"points"` // [unix_ms, value]
	Tags   map[string]string `json:"tags,omitempty"`
}

// LogLine is one log record.
type LogLine struct {
	At      time.Time `json:"at"`
	Service string    `json:"service,omitempty"`
	Status  string    `json:"status,omitempty"`
	Message string    `json:"message"`
	Source  string    `json:"source,omitempty"`
}

// Monitor is an alerting rule and its state.
type Monitor struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	State string   `json:"state"`
	Query string   `json:"query,omitempty"`
	Tags  []string `json:"tags,omitempty"`
}

// ObsEvent is a change/deploy/system event.
type ObsEvent struct {
	At     time.Time `json:"at"`
	Title  string    `json:"title"`
	Text   string    `json:"text,omitempty"`
	Source string    `json:"source,omitempty"`
	Tags   []string  `json:"tags,omitempty"`
}

// ObservabilityProvider is implemented by Datadog (v1).
type ObservabilityProvider interface {
	Name() string
	Capabilities() map[Capability]bool
	QueryMetric(ctx context.Context, query string, from, to time.Time) ([]Series, error)
	SearchLogs(ctx context.Context, query string, from, to time.Time, limit int) ([]LogLine, error)
	MonitorsInState(ctx context.Context, states, tags []string) ([]Monitor, error)
	Events(ctx context.Context, query string, from, to time.Time) ([]ObsEvent, error)
}

// ---------------------------------------------------------------- ITSM

// ItsmProvider is implemented by Jira (v1) / ServiceNow (stub).
type ItsmProvider interface {
	Name() string
	CreateIncidentTicket(ctx context.Context, inc *Incident) (string, error)
	Comment(ctx context.Context, ticketRef, text string) error
	CreateActionItem(ctx context.Context, inc *Incident, item ActionItem) (string, error)
}

// ---------------------------------------------------------------- Cloud

// WorkloadRef identifies a workload.
type WorkloadRef struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"` // Deployment
	Name      string `json:"name"`
}

// Workload is a workload's summary state.
type Workload struct {
	Ref       WorkloadRef `json:"ref"`
	Desired   int32       `json:"desired"`
	Ready     int32       `json:"ready"`
	Updated   int32       `json:"updated"`
	Images    []string    `json:"images"`
	Revision  string      `json:"revision,omitempty"`
	Condition string      `json:"condition,omitempty"`
}

// Revision is one rollout revision.
type Revision struct {
	Revision string    `json:"revision"`
	Images   []string  `json:"images"`
	Created  time.Time `json:"created"`
}

// ActionSpec is a validated catalog action ready for a provider.
type ActionSpec struct {
	ActionID string            `json:"actionId"`
	Verb     string            `json:"verb"`
	Params   map[string]string `json:"params"`
}

// CloudProvider is implemented by Kubernetes (v1, read-only) / AWS etc. (stubs).
type CloudProvider interface {
	Name() string
	// Scopes lists the cluster names this adapter may read.
	Scopes() []string
	ListWorkloads(ctx context.Context, cluster, namespace string) ([]Workload, error)
	WorkloadEvents(ctx context.Context, ref WorkloadRef, since time.Time) ([]ObsEvent, error)
	RolloutHistory(ctx context.Context, ref WorkloadRef) ([]Revision, error)
	PodLogs(ctx context.Context, ref WorkloadRef, since time.Time, tail int) ([]LogLine, error)
	DryRun(ctx context.Context, a ActionSpec) (string, error)
	Execute(ctx context.Context, a ActionSpec) (string, error)
	Verify(ctx context.Context, a ActionSpec) (string, error)
}

// ---------------------------------------------------------------- SLO

// Slo is an SLO/guardrail summary.
type Slo struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Service        string     `json:"service"`
	TargetPercent  float64    `json:"targetPercent"`
	WindowHours    int        `json:"windowHours"`
	Status         SloStatus  `json:"status"`
	LastEvaluated  *time.Time `json:"lastEvaluated,omitempty"`
	Active         bool       `json:"active"`
	ViewServiceID  *int64     `json:"viewServiceId,omitempty"`
}

// SloEvent is a status change record.
type SloEvent struct {
	At                   time.Time `json:"at"`
	PreviousStatus       SloStatus `json:"previousStatus,omitempty"`
	NewStatus            SloStatus `json:"newStatus"`
	UptimePercent        *float64  `json:"uptimePercent,omitempty"`
	BurnRate             *float64  `json:"burnRate,omitempty"`
	BudgetRemainingPct   *float64  `json:"budgetRemainingPct,omitempty"`
	Message              string    `json:"message"`
}

// Tenant is a customer tenant.
type Tenant struct {
	ID   string     `json:"id"`
	Slug string     `json:"slug"`
	Name string     `json:"name"`
	Tier TenantTier `json:"tier"`
}

// SloSource is implemented by Multiplexer (v1).
type SloSource interface {
	Name() string
	ListSlos(ctx context.Context) ([]Slo, error)
	GetSlo(ctx context.Context, id string) (Slo, error)
	SloEvents(ctx context.Context, id string, limit int) ([]SloEvent, error)
	// TenantFor resolves the tenant owning the SLO; ok=false if unknown.
	TenantFor(ctx context.Context, sloID string) (Tenant, bool, error)
	// BudgetBurned returns percentage points of error budget consumed in
	// [from,to] and a quality flag: exact | stale | unavailable.
	BudgetBurned(ctx context.Context, sloID string, from, to time.Time) (float64, string, error)
}

// ---------------------------------------------------------------- LLM

// LlmTool describes a tool offered to the model.
type LlmTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// LlmToolCall is a model request to run a tool.
type LlmToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// LlmToolResult is returned to the model.
type LlmToolResult struct {
	CallID  string `json:"callId"`
	Content string `json:"content"`
	IsError bool   `json:"isError"`
}

// Stop reasons normalised across LLM providers.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
	StopOther     = "other"
)

// LlmTurn is one assistant turn.
type LlmTurn struct {
	Text         string        `json:"text"`
	ToolCalls    []LlmToolCall `json:"toolCalls"`
	StopReason   string        `json:"stopReason"`
	InputTokens  int64         `json:"inputTokens"`
	OutputTokens int64         `json:"outputTokens"`
}

// LlmConversation keeps provider-specific history (including opaque blocks
// such as thinking) inside the adapter so it is replayed unchanged.
type LlmConversation interface {
	Send(ctx context.Context, userText string) (LlmTurn, error)
	SendToolResults(ctx context.Context, results []LlmToolResult) (LlmTurn, error)
}

// LlmPort is implemented by the Anthropic API adapter (v1) and Bedrock /
// Vertex stubs.
type LlmPort interface {
	Name() string
	NewConversation(system string, tools []LlmTool, maxTokens int) LlmConversation
}
