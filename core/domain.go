// Package core holds the incident domain model, lifecycle, severity policy,
// timeline and the ports that adapters implement. It must never import a
// vendor SDK or a package under providers/ or chat/.
package core

import (
	"encoding/json"
	"time"
)

// Severity is SEV1 (worst) .. SEV4.
type Severity int

const (
	SevUnknown Severity = 0
	Sev1       Severity = 1
	Sev2       Severity = 2
	Sev3       Severity = 3
	Sev4       Severity = 4
)

func (s Severity) String() string {
	switch s {
	case Sev1, Sev2, Sev3, Sev4:
		return "SEV" + string(rune('0'+int(s)))
	default:
		return "SEV?"
	}
}

// Valid reports whether s is SEV1..SEV4.
func (s Severity) Valid() bool { return s >= Sev1 && s <= Sev4 }

// SloStatus mirrors the SLO source's status vocabulary (Multiplexer: ok |
// at_risk | breached | no_data | timeout).
type SloStatus string

const (
	SloOK       SloStatus = "ok"
	SloAtRisk   SloStatus = "at_risk"
	SloBreached SloStatus = "breached"
	SloNoData   SloStatus = "no_data"
	SloTimeout  SloStatus = "timeout"
)

// TenantTier mirrors Multiplexer's tenant_tier enum.
type TenantTier string

const (
	TierEnterprise TenantTier = "enterprise"
	TierBusiness   TenantTier = "business"
	TierStarter    TenantTier = "starter"
	TierUnknown    TenantTier = "unknown"
)

// Role is an incident role.
type Role string

const (
	RoleIC     Role = "ic"
	RoleComms  Role = "comms"
	RoleOps    Role = "ops"
	RoleScribe Role = "scribe"
)

// ParseRole validates a role name.
func ParseRole(s string) (Role, bool) {
	switch Role(s) {
	case RoleIC, RoleComms, RoleOps, RoleScribe:
		return Role(s), true
	}
	return "", false
}

// UserRef identifies a human across systems. Email is the join key between
// chat, paging and directory (see docs/architecture.md §0).
type UserRef struct {
	Platform string `json:"platform"` // slack | teams | pagerduty | system
	ID       string `json:"id"`
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
}

// ChannelRef identifies a chat channel/conversation.
type ChannelRef struct {
	Platform string `json:"platform"`
	ID       string `json:"id"`
	// ServiceURL is required by Teams to post into the conversation.
	ServiceURL string `json:"serviceUrl,omitempty"`
	Name       string `json:"name,omitempty"`
}

// Incident is the aggregate root.
type Incident struct {
	ID              string          `json:"id"`
	Key             string          `json:"key"`
	Title           string          `json:"title"`
	Severity        Severity        `json:"severity"`
	State           State           `json:"state"`
	Services        []string        `json:"services"`
	Tenants         []string        `json:"tenants"`
	Roles           map[Role]UserRef `json:"roles"`
	WarRoom         *ChannelRef     `json:"warRoom,omitempty"`
	PageRefs        []string        `json:"pageRefs"`
	TicketRef       string          `json:"ticketRef,omitempty"`
	SloRefs         []string        `json:"sloRefs"`
	DedupKeys       []string        `json:"dedupKeys"`
	AutoDeclared    bool            `json:"autoDeclared"`
	ImpactStartedAt *time.Time      `json:"impactStartedAt,omitempty"`
	DetectedAt      time.Time       `json:"detectedAt"`
	DeclaredAt      *time.Time      `json:"declaredAt,omitempty"`
	AcknowledgedAt  *time.Time      `json:"acknowledgedAt,omitempty"`
	MitigatedAt     *time.Time      `json:"mitigatedAt,omitempty"`
	ResolvedAt      *time.Time      `json:"resolvedAt,omitempty"`
	ClosedAt        *time.Time      `json:"closedAt,omitempty"`
	LastStatusAt    *time.Time      `json:"lastStatusAt,omitempty"`
	LastNudgeAt     *time.Time      `json:"lastNudgeAt,omitempty"`
	CorrelationID   string          `json:"correlationId"`
}

// IC returns the incident commander, if assigned.
func (i *Incident) IC() (UserRef, bool) {
	u, ok := i.Roles[RoleIC]
	return u, ok && u.ID != ""
}

// Signal is a normalised alert/SLO event from any source.
type Signal struct {
	ID           string     `json:"id"`
	Source       string     `json:"source"` // pagerduty | multiplexer | datadog | manual
	ExternalID   string     `json:"externalId"`
	DedupKey     string     `json:"dedupKey"`
	Title        string     `json:"title"`
	Service      string     `json:"service"`
	Tenant       string     `json:"tenant,omitempty"`
	Tier         TenantTier `json:"tier"`
	SloID        string     `json:"sloId,omitempty"`
	SloStatus    SloStatus  `json:"sloStatus,omitempty"`
	SeverityHint Severity   `json:"severityHint,omitempty"`
	Resolved     bool       `json:"resolved"`
	// Acknowledged is set when a paging source reports a human ack.
	Acknowledged bool `json:"acknowledged"`
	ObservedAt   time.Time  `json:"observedAt"`
	// Raw is the redacted source payload, kept for audit.
	Raw        json.RawMessage `json:"raw,omitempty"`
	IncidentID string          `json:"incidentId,omitempty"`
}

// ActorType distinguishes who caused a timeline event.
type ActorType string

const (
	ActorHuman  ActorType = "human"
	ActorBot    ActorType = "bot"
	ActorSystem ActorType = "system"
)

// Actor is who caused a timeline event.
type Actor struct {
	Type  ActorType `json:"type"`
	ID    string    `json:"id"`
	Email string    `json:"email,omitempty"`
}

// SystemActor is used for automated events.
var SystemActor = Actor{Type: ActorSystem, ID: "incidentd"}

// HumanActor builds an actor from a user.
func HumanActor(u UserRef) Actor { return Actor{Type: ActorHuman, ID: u.Platform + ":" + u.ID, Email: u.Email} }

// EventKind enumerates timeline event types.
type EventKind string

const (
	EvDetected        EventKind = "detected"
	EvDeclared        EventKind = "declared"
	EvDismissed       EventKind = "dismissed"
	EvTransition      EventKind = "transition"
	EvSevChanged      EventKind = "sev_changed"
	EvRoleAssigned    EventKind = "role_assigned"
	EvStatusPosted    EventKind = "status_posted"
	EvSignalAttached  EventKind = "signal_attached"
	EvPaged           EventKind = "paged"
	EvAcknowledged    EventKind = "acknowledged"
	EvTicketCreated   EventKind = "ticket_created"
	EvWarRoomCreated  EventKind = "war_room_created"
	EvTriageRequested EventKind = "triage_requested"
	EvTriageResult    EventKind = "triage_result"
	EvActionRequested EventKind = "action_requested"
	EvActionDryRun    EventKind = "action_dry_run"
	EvActionApproved  EventKind = "action_approved"
	EvActionDenied    EventKind = "action_denied"
	EvActionExpired   EventKind = "action_expired"
	EvActionExecuted  EventKind = "action_executed"
	EvActionFailed    EventKind = "action_failed"
	EvActionVerified  EventKind = "action_verified"
	EvPostmortemDraft EventKind = "postmortem_drafted"
	EvNote            EventKind = "note"
	EvError           EventKind = "error"
)

// TimelineEvent is one append-only, hash-chained audit record.
type TimelineEvent struct {
	Seq        int64           `json:"seq"`
	IncidentID string          `json:"incidentId"`
	At         time.Time       `json:"at"`
	Actor      Actor           `json:"actor"`
	Kind       EventKind       `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	PrevHash   string          `json:"prevHash"`
	Hash       string          `json:"hash"`
}

// Risk is the remediation risk tier.
type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

// ActionStatus is the lifecycle of an ActionRequest.
type ActionStatus string

const (
	ActionPending  ActionStatus = "pending"
	ActionApproved ActionStatus = "approved"
	ActionDenied   ActionStatus = "denied"
	ActionExpired  ActionStatus = "expired"
	ActionExecuted ActionStatus = "executed"
	ActionFailed   ActionStatus = "failed"
	ActionVerified ActionStatus = "verified"
	ActionDryRun   ActionStatus = "dry_run_only"
)

// Approval is one recorded approval.
type Approval struct {
	By   UserRef   `json:"by"`
	Role string    `json:"role"` // ic | service_owner | approver
	At   time.Time `json:"at"`
}

// ActionRequest is a human-initiated (or human-accepted) catalog action.
type ActionRequest struct {
	ID          string            `json:"id"`
	IncidentID  string            `json:"incidentId"`
	ActionID    string            `json:"actionId"`
	Params      map[string]string `json:"params"`
	Risk        Risk              `json:"risk"`
	RequestedBy UserRef           `json:"requestedBy"`
	Source      string            `json:"source"` // human | triage
	DryRunDiff  string            `json:"dryRunDiff"`
	Approvals   []Approval        `json:"approvals"`
	CreatedAt   time.Time         `json:"createdAt"`
	ExpiresAt   time.Time         `json:"expiresAt"`
	Status      ActionStatus      `json:"status"`
	Result      string            `json:"result,omitempty"`
}

// Postmortem is the learning artefact for an incident.
type Postmortem struct {
	IncidentID       string            `json:"incidentId"`
	Status           string            `json:"status"` // draft | in_review | published
	Sections         map[string]string `json:"sections"`
	Metrics          IncidentMetrics   `json:"metrics"`
	ActionItems      []ActionItem      `json:"actionItems"`
	SimilarIncidents []SimilarIncident `json:"similarIncidents"`
	UpdatedAt        time.Time         `json:"updatedAt"`
}

// ActionItem is a postmortem follow-up.
type ActionItem struct {
	Title     string `json:"title"`
	Owner     string `json:"owner"`
	TicketRef string `json:"ticketRef,omitempty"`
	Due       string `json:"due,omitempty"`
}

// SimilarIncident is a retrieval hit.
type SimilarIncident struct {
	IncidentID string  `json:"incidentId"`
	Key        string  `json:"key"`
	Title      string  `json:"title"`
	Score      float64 `json:"score"`
}

// IncidentMetrics are the per-incident timing metrics. Durations are nil when
// the underlying timestamps are missing.
type IncidentMetrics struct {
	TTD            *time.Duration `json:"ttd,omitempty"`
	TTA            *time.Duration `json:"tta,omitempty"`
	TimeToDeclare  *time.Duration `json:"timeToDeclare,omitempty"`
	TTM            *time.Duration `json:"ttm,omitempty"`
	TTR            *time.Duration `json:"ttr,omitempty"`
	BudgetBurned   *float64       `json:"budgetBurnedPct,omitempty"`
	BudgetQuality  string         `json:"budgetQuality,omitempty"` // exact | stale | unavailable
}
