package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Integrations are the adapters the service drives. Any may be nil when not
// configured; the service degrades and records the gap in the timeline.
type Integrations struct {
	Chats  map[string]ChatPort // keyed by platform: slack | teams
	Paging PagingProvider
	Itsm   ItsmProvider
	Slo    SloSource
}

// ServiceConfig tunes coordination behaviour.
type ServiceConfig struct {
	DefaultPlatform  string
	AnnounceChannels map[string]ChannelRef // per platform, where proposals/announcements go
	WarRoomPrefix    string                // e.g. "inc-"
	PrivateByDefault bool                  // Q4: public by default (false)
	PageAtOrAbove    Severity              // page when severity <= this (e.g. SEV2)
	TicketAtOrAbove  Severity              // create ITSM ticket when severity <= this
	StatusCadence    map[Severity]time.Duration
	Admins           []string // emails allowed to act as IC on any incident
	AsyncTimeout     time.Duration
}

// Triager runs AI triage. Implemented in triage/.
type Triager interface {
	Triage(ctx context.Context, inc *Incident, focus string) (Message, any, error)
}

// Remediator manages action requests. Implemented in remediation/.
type Remediator interface {
	Request(ctx context.Context, inc *Incident, actor UserRef, actionID string, params map[string]string, source string) (*ActionRequest, Message, error)
	Approve(ctx context.Context, requestID string, approver UserRef) (*ActionRequest, Message, error)
	Deny(ctx context.Context, requestID string, actor UserRef) (*ActionRequest, Message, error)
}

// PostmortemEngine drafts and publishes postmortems. Implemented in postmortem/.
type PostmortemEngine interface {
	Draft(ctx context.Context, inc *Incident) (*Postmortem, Message, error)
	Publish(ctx context.Context, inc *Incident) (*Postmortem, Message, error)
}

// Service is the incident core: state machine + timeline + coordination.
type Service struct {
	store  Store
	ints   Integrations
	cfg    ServiceConfig
	policy SeverityPolicy
	log    *slog.Logger

	Triage     Triager
	Remediate  Remediator
	Postmortem PostmortemEngine

	Now   func() time.Time
	NewID func() string

	locks sync.Map // incidentID -> *sync.Mutex
	wg    sync.WaitGroup
}

// NewService builds a Service.
func NewService(store Store, ints Integrations, cfg ServiceConfig, policy SeverityPolicy, log *slog.Logger) *Service {
	if cfg.WarRoomPrefix == "" {
		cfg.WarRoomPrefix = "inc-"
	}
	if cfg.AsyncTimeout == 0 {
		cfg.AsyncTimeout = 3 * time.Minute
	}
	if ints.Chats == nil {
		ints.Chats = map[string]ChatPort{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, ints: ints, cfg: cfg, policy: policy, log: log, Now: time.Now, NewID: NewULIDish}
}

// Store exposes the store for engines.
func (s *Service) Store() Store { return s.store }

// Policy returns the active severity policy.
func (s *Service) Policy() SeverityPolicy { return s.policy }

// Wait blocks until async work (triage, drafts) finishes. Used by tests and shutdown.
func (s *Service) Wait() { s.wg.Wait() }

// NewULIDish returns a time-ordered random ID (26 hex chars, ULID-like ordering).
func NewULIDish() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%013x%s", time.Now().UnixMilli(), hex.EncodeToString(b[:])[:13])
}

func (s *Service) lock(id string) func() {
	m, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Record appends a hash-chained timeline event. It retries on append races
// (another replica wrote the same seq).
func (s *Service) Record(ctx context.Context, incidentID string, actor Actor, kind EventKind, payload any) (TimelineEvent, error) {
	for attempt := 0; attempt < 5; attempt++ {
		prev, err := s.store.LastEvent(ctx, incidentID)
		if err != nil {
			return TimelineEvent{}, err
		}
		ev, err := NewEvent(incidentID, prev, s.Now(), actor, kind, payload)
		if err != nil {
			return TimelineEvent{}, err
		}
		err = s.store.AppendEvent(ctx, ev)
		if errors.Is(err, ErrConflict) {
			continue
		}
		if err != nil {
			return TimelineEvent{}, err
		}
		s.log.Info("timeline", "incident_id", incidentID, "seq", ev.Seq, "kind", kind, "actor", actor.ID)
		return ev, nil
	}
	return TimelineEvent{}, fmt.Errorf("timeline append for %s: too many conflicts", incidentID)
}

func (s *Service) recordErr(ctx context.Context, inc *Incident, step string, err error) {
	s.log.Warn("side effect failed", "incident_id", inc.ID, "correlation_id", inc.CorrelationID, "step", step, "err", err)
	_, _ = s.Record(ctx, inc.ID, SystemActor, EvError, map[string]string{"step": step, "error": err.Error()})
}

// ---------------------------------------------------------------- Declare

// DeclareRequest declares an incident.
type DeclareRequest struct {
	Title       string
	Severity    Severity
	Services    []string
	Tenants     []string
	Actor       UserRef // zero value => system (auto-declare)
	Platform    string  // where to open the war room; default config
	Private     *bool
	Signals     []Signal
	Auto        bool
	Reason      string
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string, max int) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > max {
		s = strings.Trim(s[:max], "-")
	}
	return s
}

// Declare creates and declares an incident, opens the war room, pages and
// files a ticket. Side-effect failures are recorded, never fatal.
func (s *Service) Declare(ctx context.Context, req DeclareRequest) (*Incident, error) {
	if strings.TrimSpace(req.Title) == "" {
		return nil, errors.New("title is required")
	}
	if !req.Severity.Valid() {
		req.Severity = Sev3
	}
	now := s.Now()
	n, err := s.store.NextIncidentNumber(ctx, now.Year())
	if err != nil {
		return nil, err
	}
	inc := &Incident{
		ID:            s.NewID(),
		Key:           fmt.Sprintf("INC-%d-%04d", now.Year(), n),
		Title:         req.Title,
		Severity:      req.Severity,
		State:         StateDetected,
		Services:      req.Services,
		Tenants:       req.Tenants,
		Roles:         map[Role]UserRef{},
		DetectedAt:    now,
		AutoDeclared:  req.Auto,
		CorrelationID: s.NewID(),
	}
	for _, sig := range req.Signals {
		if sig.DedupKey != "" && !slices.Contains(inc.DedupKeys, sig.DedupKey) {
			inc.DedupKeys = append(inc.DedupKeys, sig.DedupKey)
		}
		if sig.SloID != "" && !slices.Contains(inc.SloRefs, sig.SloID) {
			inc.SloRefs = append(inc.SloRefs, sig.SloID)
		}
		if !sig.ObservedAt.IsZero() && sig.ObservedAt.Before(inc.DetectedAt) {
			inc.DetectedAt = sig.ObservedAt
		}
		if sig.Service != "" && !slices.Contains(inc.Services, sig.Service) {
			inc.Services = append(inc.Services, sig.Service)
		}
		if sig.Tenant != "" && !slices.Contains(inc.Tenants, sig.Tenant) {
			inc.Tenants = append(inc.Tenants, sig.Tenant)
		}
	}
	if len(req.Signals) > 0 {
		t := inc.DetectedAt
		inc.ImpactStartedAt = &t
	}
	actor := SystemActor
	if req.Actor.ID != "" {
		actor = HumanActor(req.Actor)
		inc.Roles[RoleIC] = req.Actor
	}
	unlock := s.lock(inc.ID)
	defer unlock()

	if err := s.store.CreateIncident(ctx, inc); err != nil {
		return nil, err
	}
	if _, err := s.Record(ctx, inc.ID, SystemActor, EvDetected, map[string]any{"title": inc.Title, "signals": len(req.Signals)}); err != nil {
		return nil, err
	}
	for _, sig := range req.Signals {
		sig.IncidentID = inc.ID
		_ = s.store.SaveSignal(ctx, sig)
		_, _ = s.Record(ctx, inc.ID, SystemActor, EvSignalAttached, sigPayload(sig))
	}
	ApplyTransition(inc, StateDeclared, s.Now())
	if _, err := s.Record(ctx, inc.ID, actor, EvDeclared, map[string]any{
		"key": inc.Key, "severity": inc.Severity.String(), "auto": req.Auto, "reason": req.Reason, "policy": s.policy.Hash,
	}); err != nil {
		return nil, err
	}
	if ic, ok := inc.IC(); ok {
		_, _ = s.Record(ctx, inc.ID, actor, EvRoleAssigned, map[string]any{"role": RoleIC, "user": ic})
	}

	s.openWarRoom(ctx, inc, req)
	s.page(ctx, inc)
	s.fileTicket(ctx, inc)

	if err := s.store.UpdateIncident(ctx, inc); err != nil {
		return nil, err
	}
	s.announce(ctx, inc, req)
	return inc, nil
}

func sigPayload(sig Signal) map[string]any {
	return map[string]any{
		"source": sig.Source, "externalId": sig.ExternalID, "dedupKey": sig.DedupKey, "title": sig.Title,
		"service": sig.Service, "tenant": sig.Tenant, "tier": sig.Tier, "sloStatus": sig.SloStatus, "resolved": sig.Resolved,
	}
}

func (s *Service) platformFor(req DeclareRequest) string {
	if req.Platform != "" {
		if _, ok := s.ints.Chats[req.Platform]; ok {
			return req.Platform
		}
	}
	return s.cfg.DefaultPlatform
}

func (s *Service) openWarRoom(ctx context.Context, inc *Incident, req DeclareRequest) {
	platform := s.platformFor(req)
	chat, ok := s.ints.Chats[platform]
	if !ok {
		s.recordErr(ctx, inc, "war_room", fmt.Errorf("no chat adapter for platform %q", platform))
		return
	}
	private := s.cfg.PrivateByDefault
	if req.Private != nil {
		private = *req.Private
	}
	name := s.cfg.WarRoomPrefix + strings.ToLower(strings.TrimPrefix(inc.Key, "INC-"))
	if sl := slug(inc.Title, 40); sl != "" {
		name += "-" + sl
	}
	if len(name) > 80 {
		name = name[:80]
	}
	ch, err := chat.CreateWarRoom(ctx, name, private)
	if errors.Is(err, ErrNotSupported) {
		// Platforms without bot channel creation (Teams without Graph) run the
		// incident in the announce channel; commands then need --incident <key>.
		_, _ = s.Record(ctx, inc.ID, SystemActor, EvNote, map[string]string{"note": "war room not supported on " + platform + "; using announce channel"})
		return
	}
	if err != nil {
		s.recordErr(ctx, inc, "war_room", err)
		return
	}
	inc.WarRoom = &ch
	_, _ = s.Record(ctx, inc.ID, SystemActor, EvWarRoomCreated, map[string]any{"channel": ch, "private": private})
	if ic, ok := inc.IC(); ok && ic.Platform == platform {
		if err := chat.Invite(ctx, ch, []UserRef{ic}); err != nil {
			s.recordErr(ctx, inc, "invite_ic", err)
		}
	}
	if err := chat.SetTopic(ctx, ch, fmt.Sprintf("%s %s | %s | state: %s", inc.Key, inc.Severity, inc.Title, inc.State)); err != nil {
		s.recordErr(ctx, inc, "set_topic", err)
	}
}

func (s *Service) page(ctx context.Context, inc *Incident) {
	if s.ints.Paging == nil || s.cfg.PageAtOrAbove == SevUnknown || inc.Severity > s.cfg.PageAtOrAbove {
		return
	}
	urgency := "low"
	if inc.Severity <= Sev2 {
		urgency = "high"
	}
	services := inc.Services
	if len(services) == 0 {
		services = []string{""}
	}
	for _, svc := range services {
		ref, err := s.ints.Paging.Page(ctx, svc, inc, urgency)
		if err != nil {
			s.recordErr(ctx, inc, "page:"+svc, err)
			continue
		}
		inc.PageRefs = append(inc.PageRefs, ref)
		_, _ = s.Record(ctx, inc.ID, SystemActor, EvPaged, map[string]string{"service": svc, "pageRef": ref, "urgency": urgency})
		if inc.WarRoom != nil {
			if oncall, err := s.ints.Paging.WhoIsOnCall(ctx, svc); err == nil {
				s.inviteByEmail(ctx, inc, oncall)
			}
		}
	}
}

func (s *Service) inviteByEmail(ctx context.Context, inc *Incident, users []UserRef) {
	chat, ok := s.ints.Chats[inc.WarRoom.Platform]
	if !ok {
		return
	}
	var refs []UserRef
	for _, u := range users {
		if u.Email == "" {
			continue
		}
		cu, err := chat.LookupUserByEmail(ctx, u.Email)
		if err != nil {
			s.recordErr(ctx, inc, "lookup_user", fmt.Errorf("%s: %w", u.Email, err))
			continue
		}
		refs = append(refs, cu)
	}
	if len(refs) > 0 {
		if err := chat.Invite(ctx, *inc.WarRoom, refs); err != nil {
			s.recordErr(ctx, inc, "invite_oncall", err)
		}
	}
}

func (s *Service) fileTicket(ctx context.Context, inc *Incident) {
	if s.ints.Itsm == nil || s.cfg.TicketAtOrAbove == SevUnknown || inc.Severity > s.cfg.TicketAtOrAbove {
		return
	}
	ref, err := s.ints.Itsm.CreateIncidentTicket(ctx, inc)
	if err != nil {
		s.recordErr(ctx, inc, "itsm_ticket", err)
		return
	}
	inc.TicketRef = ref
	_, _ = s.Record(ctx, inc.ID, SystemActor, EvTicketCreated, map[string]string{"ticket": ref})
}

func (s *Service) summary(inc *Incident) Message {
	ic := "_unassigned_"
	if u, ok := inc.IC(); ok {
		ic = displayUser(u)
	}
	fields := []Field{
		{"Severity", inc.Severity.String()},
		{"State", string(inc.State)},
		{"IC", ic},
	}
	if len(inc.Services) > 0 {
		fields = append(fields, Field{"Services", strings.Join(inc.Services, ", ")})
	}
	if len(inc.Tenants) > 0 {
		fields = append(fields, Field{"Tenants", strings.Join(inc.Tenants, ", ")})
	}
	if inc.TicketRef != "" {
		fields = append(fields, Field{"Ticket", inc.TicketRef})
	}
	if inc.WarRoom != nil {
		fields = append(fields, Field{"War room", "#" + inc.WarRoom.Name})
	}
	return Message{Title: inc.Key + ": " + inc.Title, Text: "", Fields: fields}
}

func displayUser(u UserRef) string {
	switch u.Platform {
	case "slack":
		return "<@" + u.ID + ">"
	}
	if u.Name != "" {
		return u.Name
	}
	if u.Email != "" {
		return u.Email
	}
	return u.ID
}

func (s *Service) announce(ctx context.Context, inc *Incident, req DeclareRequest) {
	msg := s.summary(inc)
	if req.Auto {
		msg.Text = "Auto-declared: " + req.Reason
	} else {
		msg.Text = "Incident declared."
	}
	if inc.WarRoom != nil {
		if chat, ok := s.ints.Chats[inc.WarRoom.Platform]; ok {
			m := msg
			m.Text += "\nUse `/incident help` for commands."
			if _, err := chat.PostMessage(ctx, *inc.WarRoom, m); err != nil {
				s.recordErr(ctx, inc, "post_war_room", err)
			}
		}
	}
	platform := s.platformFor(req)
	if ann, ok := s.cfg.AnnounceChannels[platform]; ok {
		if chat, ok := s.ints.Chats[platform]; ok {
			if _, err := chat.PostMessage(ctx, ann, msg); err != nil {
				s.recordErr(ctx, inc, "post_announce", err)
			}
		}
	}
}

// post sends to the war room (or announce channel when there is none).
func (s *Service) post(ctx context.Context, inc *Incident, m Message) {
	ch, chat := s.incidentChannel(inc)
	if chat == nil {
		return
	}
	if _, err := chat.PostMessage(ctx, ch, m); err != nil {
		s.recordErr(ctx, inc, "post", err)
	}
}

func (s *Service) incidentChannel(inc *Incident) (ChannelRef, ChatPort) {
	if inc.WarRoom != nil {
		if chat, ok := s.ints.Chats[inc.WarRoom.Platform]; ok {
			return *inc.WarRoom, chat
		}
	}
	if ann, ok := s.cfg.AnnounceChannels[s.cfg.DefaultPlatform]; ok {
		if chat, ok := s.ints.Chats[s.cfg.DefaultPlatform]; ok {
			return ann, chat
		}
	}
	return ChannelRef{}, nil
}

// ---------------------------------------------------------------- Signals

// SignalOutcome describes what HandleSignal did.
type SignalOutcome string

const (
	OutcomeAttached     SignalOutcome = "attached"
	OutcomeAutoDeclared SignalOutcome = "auto_declared"
	OutcomeProposed     SignalOutcome = "proposed"
	OutcomeIgnored      SignalOutcome = "ignored"
)

// HandleSignal dedupes, enriches and classifies a signal, then attaches it,
// auto-declares or posts a proposal (docs/architecture.md §5.2).
func (s *Service) HandleSignal(ctx context.Context, sig Signal) (SignalOutcome, *Incident, error) {
	if sig.ID == "" {
		sig.ID = s.NewID()
	}
	if sig.ObservedAt.IsZero() {
		sig.ObservedAt = s.Now()
	}
	s.enrich(ctx, &sig)
	if err := s.store.SaveSignal(ctx, sig); err != nil {
		return "", nil, err
	}
	if sig.DedupKey != "" {
		inc, err := s.store.FindOpenIncidentByDedupKey(ctx, sig.DedupKey)
		if err == nil {
			return OutcomeAttached, inc, s.attach(ctx, inc, sig)
		}
		if !errors.Is(err, ErrNotFound) {
			return "", nil, err
		}
	}
	if sig.Resolved || sig.SloStatus == SloOK {
		if sig.DedupKey != "" && sig.SloStatus != "" {
			_, _ = s.store.RecordObservation(ctx, sig.DedupKey, sig.SloStatus)
		}
		return OutcomeIgnored, nil, nil
	}
	consecutive := s.policy.AutoDeclare.ConsecutiveEvaluations // non-SLO sources are already debounced upstream
	if sig.SloStatus != "" && sig.DedupKey != "" {
		c, err := s.store.RecordObservation(ctx, sig.DedupKey, sig.SloStatus)
		if err != nil {
			return "", nil, err
		}
		consecutive = c
	}
	d := s.policy.Decide(sig, 1, consecutive)
	if d.AutoDeclare {
		inc, err := s.Declare(ctx, DeclareRequest{
			Title:    sig.Title,
			Severity: d.Severity,
			Signals:  []Signal{sig},
			Auto:     true,
			Reason:   d.Reason,
		})
		return OutcomeAutoDeclared, inc, err
	}
	if !d.Severity.Valid() {
		return OutcomeIgnored, nil, nil
	}
	// Propose once per status streak so 60s polling doesn't spam.
	if sig.SloStatus != "" && consecutive != 1 {
		return OutcomeIgnored, nil, nil
	}
	s.propose(ctx, sig, d)
	return OutcomeProposed, nil, nil
}

func (s *Service) enrich(ctx context.Context, sig *Signal) {
	if s.ints.Slo == nil || sig.SloID == "" {
		return
	}
	if sig.SloStatus == "" && !sig.Resolved {
		if slo, err := s.ints.Slo.GetSlo(ctx, sig.SloID); err == nil {
			sig.SloStatus = slo.Status
			if sig.Service == "" {
				sig.Service = slo.Service
			}
		}
	}
	if sig.Tier == "" || sig.Tier == TierUnknown {
		t, ok, err := s.ints.Slo.TenantFor(ctx, sig.SloID)
		switch {
		case err != nil:
			s.log.Warn("tenant lookup failed", "slo", sig.SloID, "err", err)
			sig.Tier = TierUnknown
		case ok:
			sig.Tier, sig.Tenant = t.Tier, t.Slug
		default:
			sig.Tier = TierUnknown
		}
	}
}

func (s *Service) attach(ctx context.Context, inc *Incident, sig Signal) error {
	unlock := s.lock(inc.ID)
	defer unlock()
	inc, err := s.store.GetIncident(ctx, inc.ID)
	if err != nil {
		return err
	}
	sig.IncidentID = inc.ID
	if _, err := s.Record(ctx, inc.ID, SystemActor, EvSignalAttached, sigPayload(sig)); err != nil {
		return err
	}
	if sig.Acknowledged && inc.AcknowledgedAt == nil {
		t := sig.ObservedAt
		inc.AcknowledgedAt = &t
		_, _ = s.Record(ctx, inc.ID, SystemActor, EvAcknowledged, map[string]string{"source": sig.Source})
	}
	if sig.Resolved {
		s.post(ctx, inc, Message{Text: fmt.Sprintf("Source `%s` reports recovery for `%s`. Consider `status monitoring`.", sig.Source, sig.DedupKey)})
	}
	return s.store.UpdateIncident(ctx, inc)
}

func (s *Service) propose(ctx context.Context, sig Signal, d DeclareDecision) {
	platform := s.cfg.DefaultPlatform
	ann, ok := s.cfg.AnnounceChannels[platform]
	chat, ok2 := s.ints.Chats[platform]
	if !ok || !ok2 {
		s.log.Warn("cannot post proposal: no announce channel", "platform", platform, "signal", sig.ID)
		return
	}
	msg := Message{
		Title: fmt.Sprintf("Proposed %s: %s", d.Severity, sig.Title),
		Text:  d.Reason,
		Fields: []Field{
			{"Source", sig.Source}, {"Service", orDash(sig.Service)}, {"Tenant", orDash(sig.Tenant)},
			{"Tier", string(sig.Tier)}, {"SLO status", orDash(string(sig.SloStatus))},
		},
		Buttons: []Button{
			{ActionID: "declare_signal", Label: "Declare " + d.Severity.String(), Value: sig.ID + "|" + fmt.Sprint(int(d.Severity)), Style: "danger"},
			{ActionID: "dismiss_signal", Label: "Dismiss", Value: sig.ID},
		},
	}
	if _, err := chat.PostMessage(ctx, ann, msg); err != nil {
		s.log.Warn("post proposal failed", "err", err)
	}
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// ---------------------------------------------------------------- Cadence

// Tick nudges ICs whose status update is overdue. Call periodically.
func (s *Service) Tick(ctx context.Context) {
	incs, err := s.store.ListIncidents(ctx, IncidentFilter{States: []State{StateDeclared, StateInvestigating, StateIdentified, StateMitigating, StateMonitoring}})
	if err != nil {
		s.log.Warn("tick list failed", "err", err)
		return
	}
	now := s.Now()
	for _, inc := range incs {
		cad, ok := s.cfg.StatusCadence[inc.Severity]
		if !ok || cad <= 0 {
			continue
		}
		last := inc.DeclaredAt
		for _, t := range []*time.Time{inc.LastStatusAt, inc.LastNudgeAt} {
			if t != nil && (last == nil || t.After(*last)) {
				last = t
			}
		}
		if last == nil || now.Sub(*last) < cad {
			continue
		}
		who := "IC"
		if ic, ok := inc.IC(); ok {
			who = displayUser(ic)
		}
		s.post(ctx, inc, Message{Text: fmt.Sprintf("%s: status update overdue (%s cadence is %s). Use `update \"…\"`.", who, inc.Severity, cad)})
		t := now
		inc.LastNudgeAt = &t
		_ = s.store.UpdateIncident(ctx, inc)
	}
}
