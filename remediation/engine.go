package remediation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// FlagRemediationEnabled is the runtime half of the two-key kill switch.
const FlagRemediationEnabled = "remediation_enabled"

// Recorder appends timeline events (core.Service implements it).
type Recorder interface {
	Record(ctx context.Context, incidentID string, actor core.Actor, kind core.EventKind, payload any) (core.TimelineEvent, error)
}

// OwnerLookup returns a service owner's email.
type OwnerLookup func(ctx context.Context, service string) (string, bool)

// Engine implements core.Remediator.
type Engine struct {
	Catalog   *Catalog
	Providers map[string]core.CloudProvider // by catalog provider name
	Store     core.Store
	Recorder  Recorder
	Itsm      core.ItsmProvider
	Owners    OwnerLookup
	TTL       time.Duration
	// EnvEnabled is the env half of the kill switch (REMEDIATION_ENABLED).
	EnvEnabled bool
	Now        func() time.Time
	NewID      func() string
}

// ExecutorEnabled is true only if BOTH the env switch and the runtime DB flag are on.
func (e *Engine) ExecutorEnabled(ctx context.Context) bool {
	if !e.EnvEnabled {
		return false
	}
	on, err := e.Store.GetFlag(ctx, FlagRemediationEnabled)
	return err == nil && on
}

func userErr(format string, a ...any) error { return &core.UserError{Msg: fmt.Sprintf(format, a...)} }

func (e *Engine) owner(ctx context.Context, inc *core.Incident) string {
	if e.Owners == nil {
		return ""
	}
	for _, s := range inc.Services {
		if o, ok := e.Owners(ctx, s); ok {
			return o
		}
	}
	return ""
}

// Request validates, dry-runs, records and returns an approval card.
func (e *Engine) Request(ctx context.Context, inc *core.Incident, actor core.UserRef, actionID string, params map[string]string, source string) (*core.ActionRequest, core.Message, error) {
	act, ok := e.Catalog.Get(actionID)
	if !ok {
		return nil, core.Message{}, userErr("unknown action %q. Catalog: %s", actionID, strings.Join(e.Catalog.IDs(), ", "))
	}
	if err := act.Validate(params); err != nil {
		return nil, core.Message{}, userErr("invalid parameters for %s: %s", actionID, err)
	}
	prov, ok := e.Providers[act.Provider]
	if !ok {
		return nil, core.Message{}, userErr("provider %q for %s is not configured", act.Provider, actionID)
	}
	spec := core.ActionSpec{ActionID: act.ID, Verb: act.Verb, Params: params}
	diff, err := prov.DryRun(ctx, spec)
	if err != nil {
		return nil, core.Message{}, userErr("dry run failed for %s: %s", actionID, err)
	}
	now := e.Now()
	req := &core.ActionRequest{
		ID: e.NewID(), IncidentID: inc.ID, ActionID: act.ID, Params: params, Risk: act.Risk,
		RequestedBy: actor, Source: source, DryRunDiff: diff, CreatedAt: now, ExpiresAt: now.Add(e.TTL), Status: core.ActionPending,
	}
	if err := e.Store.SaveAction(ctx, req); err != nil {
		return nil, core.Message{}, err
	}
	who := core.HumanActor(actor)
	if _, err := e.Recorder.Record(ctx, inc.ID, who, core.EvActionRequested, map[string]any{
		"requestId": req.ID, "actionId": act.ID, "params": params, "risk": act.Risk, "source": source, "expiresAt": req.ExpiresAt,
	}); err != nil {
		return nil, core.Message{}, err
	}
	if _, err := e.Recorder.Record(ctx, inc.ID, core.SystemActor, core.EvActionDryRun, map[string]string{"requestId": req.ID, "diff": diff}); err != nil {
		return nil, core.Message{}, err
	}
	return req, e.card(req, act, e.owner(ctx, inc)), nil
}

func (e *Engine) card(req *core.ActionRequest, act *Action, owner string) core.Message {
	params := make([]string, 0, len(req.Params))
	for k, v := range req.Params {
		params = append(params, k+"="+v)
	}
	sort.Strings(params)
	fields := []core.Field{
		{Label: "Risk", Value: string(req.Risk)},
		{Label: "Needs", Value: Describe(req.Risk)},
		{Label: "Requested by", Value: req.RequestedBy.Email},
		{Label: "Expires", Value: req.ExpiresAt.UTC().Format("15:04:05Z")},
	}
	if req.Risk == core.RiskHigh {
		fields = append(fields, core.Field{Label: "Service owner", Value: orDash(owner)})
	}
	return core.Message{
		Title:  fmt.Sprintf("Approval needed: %s (%s)", act.ID, strings.Join(params, " ")),
		Text:   act.Description + "\n```\n" + req.DryRunDiff + "\n```",
		Fields: fields,
		Buttons: []core.Button{
			{ActionID: "approve_action", Label: "Approve", Value: req.ID, Style: "primary"},
			{ActionID: "deny_action", Label: "Deny", Value: req.ID, Style: "danger"},
		},
	}
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

func (e *Engine) load(ctx context.Context, id string) (*core.ActionRequest, *core.Incident, error) {
	req, err := e.Store.GetAction(ctx, id)
	if err != nil {
		return nil, nil, userErr("action request %s not found", id)
	}
	inc, err := e.Store.GetIncident(ctx, req.IncidentID)
	if err != nil {
		return nil, nil, err
	}
	return req, inc, nil
}

// Approve records an approval and executes once the policy is satisfied.
func (e *Engine) Approve(ctx context.Context, id string, approver core.UserRef) (*core.ActionRequest, core.Message, error) {
	req, inc, err := e.load(ctx, id)
	if err != nil {
		return nil, core.Message{}, err
	}
	now := e.Now()
	if req.Status == core.ActionPending && !now.Before(req.ExpiresAt) {
		req.Status = core.ActionExpired
		_ = e.Store.SaveAction(ctx, req)
		_, _ = e.Recorder.Record(ctx, inc.ID, core.SystemActor, core.EvActionExpired, map[string]string{"requestId": req.ID})
		return req, core.Message{}, userErr("request %s expired; request it again", req.ID)
	}
	owner := e.owner(ctx, inc)
	role, err := CanApprove(req, inc, approver, owner, now)
	if err != nil {
		return req, core.Message{}, &core.UserError{Msg: err.Error()}
	}
	app := core.Approval{By: approver, Role: role, At: now}
	// Record the approval in the timeline BEFORE anything can execute.
	if _, err := e.Recorder.Record(ctx, inc.ID, core.HumanActor(approver), core.EvActionApproved, map[string]any{
		"requestId": req.ID, "role": role, "by": approver,
	}); err != nil {
		return req, core.Message{}, err
	}
	req.Approvals = append(req.Approvals, app)
	if err := e.Store.SaveAction(ctx, req); err != nil {
		return req, core.Message{}, err
	}
	if !Satisfied(req.Risk, req.RequestedBy, req.Approvals) {
		return req, core.Message{Text: fmt.Sprintf("%s approved %s as %s. Still needed: %s.", approver.Email, req.ActionID, role, Describe(req.Risk))}, nil
	}
	return e.execute(ctx, req, inc)
}

// approvalsFromTimeline rebuilds approvals for a request from the recorded,
// hash-verified timeline. Execution trusts only this, never in-memory state.
func (e *Engine) approvalsFromTimeline(ctx context.Context, incidentID, requestID string) ([]core.Approval, error) {
	evs, err := e.Store.Events(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	if _, err := core.VerifyChain(evs); err != nil {
		return nil, fmt.Errorf("timeline integrity check failed: %w", err)
	}
	var out []core.Approval
	for _, ev := range evs {
		if ev.Kind != core.EvActionApproved || ev.Actor.Type != core.ActorHuman {
			continue
		}
		var p struct {
			RequestID string       `json:"requestId"`
			Role      string       `json:"role"`
			By        core.UserRef `json:"by"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || p.RequestID != requestID {
			continue
		}
		if !strings.EqualFold(ev.Actor.Email, p.By.Email) {
			continue // actor and payload must agree
		}
		out = append(out, core.Approval{By: p.By, Role: p.Role, At: ev.At})
	}
	return out, nil
}

func (e *Engine) execute(ctx context.Context, req *core.ActionRequest, inc *core.Incident) (*core.ActionRequest, core.Message, error) {
	recorded, err := e.approvalsFromTimeline(ctx, inc.ID, req.ID)
	if err != nil {
		return req, core.Message{}, err
	}
	if !Satisfied(req.Risk, req.RequestedBy, recorded) {
		return req, core.Message{}, errors.New("approvals in the timeline do not satisfy policy; refusing to execute")
	}
	act, _ := e.Catalog.Get(req.ActionID)
	spec := core.ActionSpec{ActionID: req.ActionID, Verb: act.Verb, Params: req.Params}
	prov := e.Providers[act.Provider]

	if !e.ExecutorEnabled(ctx) {
		return e.dryRunOnly(ctx, req, inc)
	}
	out, err := prov.Execute(ctx, spec)
	if errors.Is(err, core.ErrExecutorDisabled) {
		return e.dryRunOnly(ctx, req, inc)
	}
	if err != nil {
		req.Status, req.Result = core.ActionFailed, err.Error()
		_ = e.Store.SaveAction(ctx, req)
		_, _ = e.Recorder.Record(ctx, inc.ID, core.SystemActor, core.EvActionFailed, map[string]string{"requestId": req.ID, "error": err.Error()})
		return req, core.Message{Text: fmt.Sprintf("%s failed: %s", req.ActionID, err)}, nil
	}
	req.Status, req.Result = core.ActionExecuted, out
	_ = e.Store.SaveAction(ctx, req)
	_, _ = e.Recorder.Record(ctx, inc.ID, core.SystemActor, core.EvActionExecuted, map[string]string{"requestId": req.ID, "result": out})
	text := fmt.Sprintf("%s executed: %s", req.ActionID, out)
	if v, err := prov.Verify(ctx, spec); err == nil {
		req.Status = core.ActionVerified
		_, _ = e.Recorder.Record(ctx, inc.ID, core.SystemActor, core.EvActionVerified, map[string]string{"requestId": req.ID, "result": v})
		text += "\nVerify: " + v
	} else {
		_, _ = e.Recorder.Record(ctx, inc.ID, core.SystemActor, core.EvNote, map[string]string{"requestId": req.ID, "verify": err.Error()})
		text += "\nVerify pending: " + err.Error()
	}
	_ = e.Store.SaveAction(ctx, req)
	if e.Itsm != nil && inc.TicketRef != "" {
		_ = e.Itsm.Comment(ctx, inc.TicketRef, text)
	}
	return req, core.Message{Text: text}, nil
}

func (e *Engine) dryRunOnly(ctx context.Context, req *core.ActionRequest, inc *core.Incident) (*core.ActionRequest, core.Message, error) {
	req.Status = core.ActionDryRun
	_ = e.Store.SaveAction(ctx, req)
	_, _ = e.Recorder.Record(ctx, inc.ID, core.SystemActor, core.EvNote, map[string]string{
		"requestId": req.ID, "note": "approved but executor disabled (kill switch); no change made",
	})
	return req, core.Message{Text: fmt.Sprintf("%s approved. Executor is disabled (dry-run only in this phase); no change was made. Perform the change manually if needed:\n```\n%s\n```", req.ActionID, req.DryRunDiff)}, nil
}

// Deny rejects a pending request (IC or requester).
func (e *Engine) Deny(ctx context.Context, id string, actor core.UserRef) (*core.ActionRequest, core.Message, error) {
	req, inc, err := e.load(ctx, id)
	if err != nil {
		return nil, core.Message{}, err
	}
	if req.Status != core.ActionPending {
		return req, core.Message{}, userErr("request is %s", req.Status)
	}
	ic, hasIC := inc.IC()
	if !(hasIC && same(ic, actor)) && !same(req.RequestedBy, actor) {
		return req, core.Message{}, userErr("only the IC or the requester can deny")
	}
	req.Status = core.ActionDenied
	if err := e.Store.SaveAction(ctx, req); err != nil {
		return req, core.Message{}, err
	}
	_, _ = e.Recorder.Record(ctx, inc.ID, core.HumanActor(actor), core.EvActionDenied, map[string]string{"requestId": req.ID})
	return req, core.Message{Text: fmt.Sprintf("%s denied by %s", req.ActionID, actor.Email)}, nil
}

var _ core.Remediator = (*Engine)(nil)
