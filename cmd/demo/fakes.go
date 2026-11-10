package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/triage"
)

// demoSlo is an in-memory Multiplexer with one guardrail for tenant acme.
type demoSlo struct {
	mu     sync.Mutex
	log    *integrationLog
	status core.SloStatus
}

func (s *demoSlo) set(st core.SloStatus) { s.mu.Lock(); s.status = st; s.mu.Unlock() }
func (s *demoSlo) get() core.SloStatus   { s.mu.Lock(); defer s.mu.Unlock(); return s.status }

func (s *demoSlo) slo() core.Slo {
	return core.Slo{ID: "7", Name: "Checkout availability", Service: "checkout", TargetPercent: 99.9, WindowHours: 720, Status: s.get(), Active: true}
}

func (s *demoSlo) Name() string                                 { return "multiplexer (demo)" }
func (s *demoSlo) ListSlos(context.Context) ([]core.Slo, error) { return []core.Slo{s.slo()}, nil }
func (s *demoSlo) GetSlo(_ context.Context, id string) (core.Slo, error) {
	if id != "7" {
		return core.Slo{}, core.ErrNotFound
	}
	return s.slo(), nil
}
func (s *demoSlo) SloEvents(context.Context, string, int) ([]core.SloEvent, error) {
	b, r, u := 12.5, 5.8, 99.71
	return []core.SloEvent{{At: time.Now(), PreviousStatus: core.SloAtRisk, NewStatus: core.SloBreached, BudgetRemainingPct: &b, BurnRate: &r, UptimePercent: &u, Message: "BREACHED"}}, nil
}
func (s *demoSlo) TenantFor(context.Context, string) (core.Tenant, bool, error) {
	return core.Tenant{ID: "1", Slug: "acme", Name: "Acme Corp", Tier: core.TierEnterprise}, true, nil
}
func (s *demoSlo) BudgetBurned(context.Context, string, time.Time, time.Time) (float64, string, error) {
	return 18.5, "stale", nil
}

type loggingPaging struct {
	log *integrationLog
	n   int
	mu  sync.Mutex
}

func (p *loggingPaging) Name() string { return "pagerduty" }
func (p *loggingPaging) WhoIsOnCall(_ context.Context, svc string) ([]core.UserRef, error) {
	p.log.add("pagerduty", "On-call for %s: bob@example.com", svc)
	return []core.UserRef{{Platform: "pagerduty", ID: "PBOB", Email: "bob@example.com"}}, nil
}
func (p *loggingPaging) Page(_ context.Context, svc string, inc *core.Incident, urgency string) (string, error) {
	p.mu.Lock()
	p.n++
	id := fmt.Sprintf("PD%04d", p.n)
	p.mu.Unlock()
	p.log.add("pagerduty", "POST /incidents → %s (%s urgency) for %s %s", id, urgency, inc.Key, inc.Severity)
	return id, nil
}
func (p *loggingPaging) Resolve(_ context.Context, ref string) error {
	p.log.add("pagerduty", "PUT /incidents %s status=resolved", ref)
	return nil
}
func (p *loggingPaging) AddNote(_ context.Context, ref, text string) error {
	p.log.add("pagerduty", "Note on %s: %q", ref, text)
	return nil
}

type loggingItsm struct {
	log *integrationLog
	n   int
	mu  sync.Mutex
}

func (i *loggingItsm) Name() string { return "jira" }
func (i *loggingItsm) CreateIncidentTicket(_ context.Context, inc *core.Incident) (string, error) {
	i.mu.Lock()
	i.n++
	key := fmt.Sprintf("OPS-%d", 100+i.n)
	i.mu.Unlock()
	i.log.add("jira", "POST /rest/api/3/issue → %s [%s] %s", key, inc.Key, inc.Title)
	return key, nil
}
func (i *loggingItsm) Comment(_ context.Context, ref, text string) error {
	i.log.add("jira", "Comment on %s: %q", ref, text)
	return nil
}
func (i *loggingItsm) CreateActionItem(_ context.Context, inc *core.Incident, it core.ActionItem) (string, error) {
	i.mu.Lock()
	i.n++
	key := fmt.Sprintf("OPS-%d", 100+i.n)
	i.mu.Unlock()
	i.log.add("jira", "Action item %s: %s", key, it.Title)
	return key, nil
}

// demoCloud is a read-only Kubernetes stand-in: dry runs only.
type demoCloud struct{ log *integrationLog }

func (c *demoCloud) Name() string     { return "kubernetes" }
func (c *demoCloud) Scopes() []string { return []string{"prod-eu-1"} }
func (c *demoCloud) ListWorkloads(context.Context, string, string) ([]core.Workload, error) {
	return nil, nil
}
func (c *demoCloud) WorkloadEvents(context.Context, core.WorkloadRef, time.Time) ([]core.ObsEvent, error) {
	return nil, nil
}
func (c *demoCloud) RolloutHistory(context.Context, core.WorkloadRef) ([]core.Revision, error) {
	return nil, nil
}
func (c *demoCloud) PodLogs(context.Context, core.WorkloadRef, time.Time, int) ([]core.LogLine, error) {
	return nil, nil
}
func (c *demoCloud) DryRun(_ context.Context, a core.ActionSpec) (string, error) {
	c.log.add("kubernetes", "Dry run %s on %s/%s/%s (read-only)", a.Verb, a.Params["cluster"], a.Params["namespace"], a.Params["deployment"])
	return fmt.Sprintf("%s/%s/deployment/%s\n~ revision 7 → 6\n- images [api=registry/checkout-api:v2.3.1]\n+ images [api=registry/checkout-api:v2.3.0]",
		a.Params["cluster"], a.Params["namespace"], a.Params["deployment"]), nil
}
func (c *demoCloud) Execute(context.Context, core.ActionSpec) (string, error) {
	return "", core.ErrExecutorDisabled
}
func (c *demoCloud) Verify(context.Context, core.ActionSpec) (string, error) { return "", nil }

// ---------------------------------------------------------------- scripted LLM

// scriptedLLM replays fixed turns for each new conversation and logs them.
type scriptedLLM struct {
	log   *integrationLog
	turns []core.LlmTurn
}

func (s *scriptedLLM) Name() string { return "scripted (demo, no API calls)" }
func (s *scriptedLLM) NewConversation(string, []core.LlmTool, int) core.LlmConversation {
	return &scriptedConv{s: s}
}

type scriptedConv struct {
	s *scriptedLLM
	i int
}

func (c *scriptedConv) next() (core.LlmTurn, error) {
	time.Sleep(700 * time.Millisecond) // make the async flow visible in the demo
	if c.i >= len(c.s.turns) {
		return core.LlmTurn{StopReason: core.StopEndTurn}, nil
	}
	t := c.s.turns[c.i]
	c.i++
	names := ""
	for _, tc := range t.ToolCalls {
		names += " " + tc.Name
	}
	c.s.log.add("llm", "Scripted model turn %d →%s", c.i, names)
	return t, nil
}
func (c *scriptedConv) Send(context.Context, string) (core.LlmTurn, error) { return c.next() }
func (c *scriptedConv) SendToolResults(context.Context, []core.LlmToolResult) (core.LlmTurn, error) {
	return c.next()
}

func call(id, name string, in any) core.LlmToolCall {
	b, _ := json.Marshal(in)
	return core.LlmToolCall{ID: id, Name: name, Input: b}
}

var k8sArgs = map[string]string{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "checkout-api"}

var triageScript = []core.LlmTurn{
	{StopReason: core.StopToolUse, ToolCalls: []core.LlmToolCall{
		call("c1", "get_incident_context", map[string]any{}),
		call("c2", "get_slo_status", map[string]any{}),
		call("c3", "list_events", map[string]any{}),
	}},
	{StopReason: core.StopToolUse, ToolCalls: []core.LlmToolCall{
		call("c4", "rollout_history", k8sArgs),
		call("c5", "search_logs", map[string]any{"query": "service:checkout status:error"}),
	}},
	{StopReason: core.StopToolUse, ToolCalls: []core.LlmToolCall{call("s1", triage.SubmitTool, map[string]any{
		"summary":     "Checkout availability breached ~5 minutes after checkout-api v2.3.1 was deployed; errors are NullPointerExceptions in the payment step introduced in that release.",
		"blastRadius": map[string]any{"services": []string{"checkout"}, "tenants": []string{"acme"}, "regions": []string{"eu"}, "userImpact": "card payments failing at checkout"},
		"hypotheses": []any{
			map[string]any{"cause": "Bad deploy: checkout-api v2.3.1 (NPE in PaymentStep)", "confidence": 0.82, "evidence": []any{
				map[string]any{"tool": "list_events", "ref": "c3", "finding": "Deploy checkout-api v2.3.1 at 02:40Z, 5 min before the breach"},
				map[string]any{"tool": "rollout_history", "ref": "c4", "finding": "Revision 7 = v2.3.1; revision 6 = v2.3.0"},
				map[string]any{"tool": "search_logs", "ref": "c5", "finding": "NullPointerException at PaymentStep.apply (v2.3.1)"},
			}},
			map[string]any{"cause": "Payment provider latency", "confidence": 0.12, "evidence": []any{
				map[string]any{"tool": "search_logs", "ref": "c5", "finding": "A few upstream timeouts, far fewer than the NPEs"},
			}},
			map[string]any{"cause": "Database saturation", "confidence": 0.4, "evidence": []any{
				map[string]any{"tool": "query_metric", "ref": "c99", "finding": "invented citation (will be dropped)"},
			}},
		},
		"recommendedActions": []any{
			map[string]any{"actionId": "k8s.rollout_undo", "params": k8sArgs, "rationale": "Roll back to v2.3.0 (revision 6), the last healthy release.", "risk": "low"},
			map[string]any{"actionId": "shell.exec", "params": map[string]string{"cmd": "kubectl delete pod -l app=checkout"}, "rationale": "not in catalog (will be dropped)"},
		},
		"missingData": []string{"database metrics (not queried)"},
	})}},
}

var postmortemScript = []core.LlmTurn{{StopReason: core.StopToolUse, ToolCalls: []core.LlmToolCall{call("p1", "submit_postmortem", map[string]any{
	"summary":              "Checkout payments failed for acme after checkout-api v2.3.1 shipped a null-handling regression in the payment step.",
	"root_cause":           "A refactor in v2.3.1 removed a null check in PaymentStep.apply; orders without a saved card triggered a NullPointerException.",
	"contributing_factors": "No canary stage for checkout-api; the regression test suite did not cover guest checkout.",
	"went_well":            "SLO breach auto-declared within two evaluations; triage identified the deploy with cited evidence; rollback approved by IC and service owner.",
	"went_badly":           "Rollback was manual because the executor is still disabled in this phase.",
	"action_items":         []any{map[string]string{"title": "Add a canary stage to the checkout-api pipeline", "owner": "checkout team"}, map[string]string{"title": "Add guest-checkout regression tests", "owner": "checkout team"}},
})}}}

func replayExecutor() *triage.ReplayExecutor {
	ev := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	return &triage.ReplayExecutor{
		Context: ev(map[string]any{"service": "checkout", "tenant": "acme"}),
		Recordings: map[string][]triage.ReplayRule{
			"get_slo_status":  {{Output: ev(map[string]any{"slo": "Checkout availability", "status": "breached", "burnRate": 5.8, "budgetRemainingPct": 12.5})}},
			"list_events":     {{Output: ev([]map[string]string{{"at": "02:40Z", "title": "Deploy checkout-api v2.3.1", "source": "argocd"}})}},
			"rollout_history": {{Output: ev([]map[string]any{{"revision": "7", "images": []string{"api=registry/checkout-api:v2.3.1"}}, {"revision": "6", "images": []string{"api=registry/checkout-api:v2.3.0"}}})}},
			"search_logs": {{Output: ev([]map[string]string{
				{"message": "NullPointerException at PaymentStep.apply (v2.3.1) user=guest@example.com token=sk_live_abc123secret", "status": "error"},
				{"message": "upstream timeout psp.example:443", "status": "warn"},
			})}},
		},
		Redactor: core.MustRedactor(true),
	}
}
