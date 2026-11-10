package remediation_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/remediation"
	"github.com/streetsmartops/incident-management-chatbot/test/fakes"
)

var (
	ic        = core.UserRef{Platform: "slack", ID: "UIC", Email: "ic@example.com"}
	requester = core.UserRef{Platform: "slack", ID: "UREQ", Email: "req@example.com"}
	owner     = core.UserRef{Platform: "slack", ID: "UOWN", Email: "owner@example.com"}
	other     = core.UserRef{Platform: "slack", ID: "UOTH", Email: "other@example.com"}
	unmapped  = core.UserRef{Platform: "slack", ID: "UNOMAIL"}
	t0        = time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
)

func TestCatalogLoadsAndValidates(t *testing.T) {
	cat, err := remediation.LoadCatalog("catalog")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cat.IDs(), ","); got != "k8s.rollout_restart,k8s.rollout_undo,k8s.scale_deployment" {
		t.Fatalf("ids %s", got)
	}
	scale, _ := cat.Get("k8s.scale_deployment")
	ok := map[string]string{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "api", "replicas": "5"}
	if err := scale.Validate(ok); err != nil {
		t.Fatal(err)
	}
	bad := []map[string]string{
		{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "api", "replicas": "500"},
		{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "api"},
		{"cluster": "prod-eu-1", "namespace": "checkout; rm -rf /", "deployment": "api", "replicas": "2"},
		{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "api", "replicas": "2", "command": "bash"},
	}
	for _, p := range bad {
		if err := scale.Validate(p); err == nil {
			t.Errorf("should reject %v", p)
		}
	}
	if _, err := remediation.NewCatalog([]*remediation.Action{{ID: "x", Provider: "kubernetes", Verb: "scale", Risk: "extreme"}}); err == nil {
		t.Fatal("invalid risk must be rejected")
	}
}

func req(risk core.Risk) *core.ActionRequest {
	return &core.ActionRequest{ID: "r1", Risk: risk, RequestedBy: requester, Status: core.ActionPending, CreatedAt: t0, ExpiresAt: t0.Add(15 * time.Minute)}
}

func incWithIC(u core.UserRef) *core.Incident {
	return &core.Incident{ID: "inc1", Roles: map[core.Role]core.UserRef{core.RoleIC: u}, Services: []string{"checkout"}}
}

func TestApprovalPolicy(t *testing.T) {
	inc := incWithIC(ic)
	cases := []struct {
		name     string
		risk     core.Risk
		approver core.UserRef
		inc      *core.Incident
		ok       bool
	}{
		{"low: any mapped user", core.RiskLow, other, inc, true},
		{"low: requester not IC", core.RiskLow, requester, inc, false},
		{"low: requester who is IC", core.RiskLow, requester, incWithIC(requester), true},
		{"low: unmapped identity", core.RiskLow, unmapped, inc, false},
		{"medium: IC", core.RiskMedium, ic, inc, true},
		{"medium: non-IC", core.RiskMedium, other, inc, false},
		{"medium: IC is requester", core.RiskMedium, requester, incWithIC(requester), false},
		{"high: IC", core.RiskHigh, ic, inc, true},
		{"high: owner", core.RiskHigh, owner, inc, true},
		{"high: random", core.RiskHigh, other, inc, false},
		{"high: requester", core.RiskHigh, requester, incWithIC(requester), false},
	}
	for _, c := range cases {
		_, err := remediation.CanApprove(req(c.risk), c.inc, c.approver, owner.Email, t0.Add(time.Minute))
		if (err == nil) != c.ok {
			t.Errorf("%s: ok=%v err=%v", c.name, c.ok, err)
		}
	}
}

func TestTwoPersonRuleAndExpiry(t *testing.T) {
	inc := incWithIC(ic)
	r := req(core.RiskHigh)
	role, err := remediation.CanApprove(r, inc, ic, owner.Email, t0)
	if err != nil {
		t.Fatal(err)
	}
	r.Approvals = append(r.Approvals, core.Approval{By: ic, Role: role, At: t0})
	if remediation.Satisfied(r.Risk, r.RequestedBy, r.Approvals) {
		t.Fatal("one approval must not satisfy high risk")
	}
	if _, err := remediation.CanApprove(r, inc, ic, owner.Email, t0); err == nil {
		t.Fatal("same person cannot approve twice")
	}
	role, err = remediation.CanApprove(r, inc, owner, owner.Email, t0)
	if err != nil || role != remediation.RoleServiceOwner {
		t.Fatalf("owner approval: %s %v", role, err)
	}
	r.Approvals = append(r.Approvals, core.Approval{By: owner, Role: role, At: t0})
	if !remediation.Satisfied(r.Risk, r.RequestedBy, r.Approvals) {
		t.Fatal("IC + owner must satisfy high risk")
	}

	// IC who is also the owner fills only one slot.
	r2 := req(core.RiskHigh)
	both := incWithIC(owner)
	role, _ = remediation.CanApprove(r2, both, owner, owner.Email, t0)
	r2.Approvals = []core.Approval{{By: owner, Role: role}}
	if remediation.Satisfied(r2.Risk, r2.RequestedBy, r2.Approvals) {
		t.Fatal("one person holding both roles must not satisfy the two-person rule")
	}
	forged := []core.Approval{{By: owner, Role: remediation.RoleIC}, {By: owner, Role: remediation.RoleServiceOwner}}
	if remediation.Satisfied(core.RiskHigh, requester, forged) {
		t.Fatal("same person in both roles must not satisfy")
	}

	// High risk without a known owner cannot be approved.
	if _, err := remediation.CanApprove(req(core.RiskHigh), inc, ic, "", t0); err == nil {
		t.Fatal("no owner configured => cannot satisfy two-person rule")
	}
	// Expiry.
	if _, err := remediation.CanApprove(req(core.RiskLow), inc, other, owner.Email, t0.Add(15*time.Minute)); err == nil {
		t.Fatal("expired request must be refused")
	}
}

type engineHarness struct {
	eng   *remediation.Engine
	svc   *core.Service
	store *core.MemStore
	cloud *fakeCloud
	clock *fakes.Clock
	inc   *core.Incident
}

type fakeCloud struct {
	executed []core.ActionSpec
	enabled  bool
}

func (f *fakeCloud) Name() string     { return "kubernetes" }
func (f *fakeCloud) Scopes() []string { return []string{"prod-eu-1"} }
func (f *fakeCloud) ListWorkloads(context.Context, string, string) ([]core.Workload, error) {
	return nil, nil
}
func (f *fakeCloud) WorkloadEvents(context.Context, core.WorkloadRef, time.Time) ([]core.ObsEvent, error) {
	return nil, nil
}
func (f *fakeCloud) RolloutHistory(context.Context, core.WorkloadRef) ([]core.Revision, error) {
	return nil, nil
}
func (f *fakeCloud) PodLogs(context.Context, core.WorkloadRef, time.Time, int) ([]core.LogLine, error) {
	return nil, nil
}
func (f *fakeCloud) DryRun(_ context.Context, a core.ActionSpec) (string, error) {
	return "~ " + a.Verb + " " + a.Params["deployment"], nil
}
func (f *fakeCloud) Execute(_ context.Context, a core.ActionSpec) (string, error) {
	if !f.enabled {
		return "", core.ErrExecutorDisabled
	}
	f.executed = append(f.executed, a)
	return "ok", nil
}
func (f *fakeCloud) Verify(context.Context, core.ActionSpec) (string, error) {
	return "rollout complete", nil
}

func newEngine(t *testing.T, envEnabled bool) *engineHarness {
	t.Helper()
	cat, err := remediation.LoadCatalog("catalog")
	if err != nil {
		t.Fatal(err)
	}
	store := core.NewMemStore()
	clock := &fakes.Clock{T: t0}
	chat := fakes.NewChat("slack")
	svc := core.NewService(store, core.Integrations{Chats: map[string]core.ChatPort{"slack": chat}}, core.ServiceConfig{DefaultPlatform: "slack"}, core.DefaultSeverityPolicy(), nil)
	svc.Now = clock.Now
	svc.NewID = fakes.IDs("inc")
	cloud := &fakeCloud{}
	eng := &remediation.Engine{
		Catalog: cat, Providers: map[string]core.CloudProvider{"kubernetes": cloud}, Store: store, Recorder: svc,
		Owners: func(_ context.Context, s string) (string, bool) { return owner.Email, s == "checkout" },
		TTL:    15 * time.Minute, EnvEnabled: envEnabled, Now: clock.Now, NewID: fakes.IDs("req"),
	}
	inc, err := svc.Declare(context.Background(), core.DeclareRequest{Title: "x", Severity: core.Sev2, Services: []string{"checkout"}, Actor: ic})
	if err != nil {
		t.Fatal(err)
	}
	return &engineHarness{eng: eng, svc: svc, store: store, cloud: cloud, clock: clock, inc: inc}
}

var undo = map[string]string{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "api"}

func kinds(t *testing.T, store core.Store, incID string) []core.EventKind {
	evs, _ := store.Events(context.Background(), incID)
	var out []core.EventKind
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestFlowDryRunOnlyWhenKillSwitchOff(t *testing.T) {
	h := newEngine(t, false)
	ctx := context.Background()
	r, card, err := h.eng.Request(ctx, h.inc, requester, "k8s.rollout_undo", undo, "human")
	if err != nil {
		t.Fatal(err)
	}
	if len(card.Buttons) != 2 || !strings.Contains(card.Text, "rollout_undo api") {
		t.Fatalf("card: %+v", card)
	}
	if _, _, err := h.eng.Approve(ctx, r.ID, ic); err != nil {
		t.Fatal(err)
	}
	r2, msg, err := h.eng.Approve(ctx, r.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Status != core.ActionDryRun || !strings.Contains(msg.Text, "dry-run only") || len(h.cloud.executed) != 0 {
		t.Fatalf("status %s msg %q executed %d", r2.Status, msg.Text, len(h.cloud.executed))
	}
	// Env on but DB flag off: still dry-run only.
	h.eng.EnvEnabled = true
	h.cloud.enabled = true
	r3, _, _ := h.eng.Request(ctx, h.inc, requester, "k8s.rollout_restart", undo, "human")
	r3, _, _ = h.eng.Approve(ctx, r3.ID, ic)
	if r3.Status != core.ActionDryRun || len(h.cloud.executed) != 0 {
		t.Fatal("both kill-switch keys are required")
	}
}

func TestFlowExecutesWithBothKeysAfterRecordedApproval(t *testing.T) {
	h := newEngine(t, true)
	ctx := context.Background()
	h.cloud.enabled = true
	_ = h.store.SetFlag(ctx, remediation.FlagRemediationEnabled, true)
	r, _, err := h.eng.Request(ctx, h.inc, requester, "k8s.scale_deployment", map[string]string{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "api", "replicas": "6"}, "triage")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.Approve(ctx, r.ID, other); err == nil {
		t.Fatal("medium risk needs the IC")
	}
	r, msg, err := h.eng.Approve(ctx, r.ID, ic)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != core.ActionVerified || len(h.cloud.executed) != 1 || !strings.Contains(msg.Text, "rollout complete") {
		t.Fatalf("status %s executed %d msg %q", r.Status, len(h.cloud.executed), msg.Text)
	}
	ks := kinds(t, h.store, h.inc.ID)
	idxApproved, idxExecuted := -1, -1
	for i, k := range ks {
		if k == core.EvActionApproved && idxApproved < 0 {
			idxApproved = i
		}
		if k == core.EvActionExecuted {
			idxExecuted = i
		}
	}
	if idxApproved < 0 || idxExecuted < 0 || idxApproved > idxExecuted {
		t.Fatalf("approval must be recorded before execution: %v", ks)
	}
}

func TestExecutionRefusedIfTimelineLacksApproval(t *testing.T) {
	h := newEngine(t, true)
	ctx := context.Background()
	h.cloud.enabled = true
	_ = h.store.SetFlag(ctx, remediation.FlagRemediationEnabled, true)
	r, _, _ := h.eng.Request(ctx, h.inc, requester, "k8s.scale_deployment", map[string]string{"cluster": "prod-eu-1", "namespace": "checkout", "deployment": "api", "replicas": "6"}, "triage")
	// Forge an approval directly in the store (bypassing the timeline).
	r.Approvals = []core.Approval{{By: ic, Role: remediation.RoleIC, At: t0}}
	_ = h.store.SaveAction(ctx, r)
	// A new approval by `other` is refused (medium needs IC) and nothing executes.
	_, _, _ = h.eng.Approve(ctx, r.ID, other)
	if len(h.cloud.executed) != 0 {
		t.Fatal("must not execute")
	}
	// High-risk forged case: a forged in-memory owner approval plus a real,
	// recorded IC approval looks satisfied in memory, but the timeline only
	// holds the IC approval, so execution must be refused.
	r2, _, _ := h.eng.Request(ctx, h.inc, requester, "k8s.rollout_undo", undo, "triage")
	r2.Approvals = []core.Approval{{By: owner, Role: remediation.RoleServiceOwner, At: t0}}
	_ = h.store.SaveAction(ctx, r2)
	_, _, err := h.eng.Approve(ctx, r2.ID, ic)
	if err == nil || !strings.Contains(err.Error(), "do not satisfy policy") {
		t.Fatalf("forged approval must be caught by the timeline check, got %v", err)
	}
	if len(h.cloud.executed) != 0 {
		t.Fatal("must not execute on forged approvals")
	}
}

func TestApproveExpiredAndDeny(t *testing.T) {
	h := newEngine(t, false)
	ctx := context.Background()
	r, _, _ := h.eng.Request(ctx, h.inc, requester, "k8s.rollout_restart", undo, "human")
	h.clock.Advance(16 * time.Minute)
	if _, _, err := h.eng.Approve(ctx, r.ID, ic); err == nil {
		t.Fatal("expired approval must fail")
	}
	got, _ := h.store.GetAction(ctx, r.ID)
	if got.Status != core.ActionExpired {
		t.Fatalf("status %s", got.Status)
	}
	r2, _, _ := h.eng.Request(ctx, h.inc, requester, "k8s.rollout_restart", undo, "human")
	var ue *core.UserError
	if _, _, err := h.eng.Deny(ctx, r2.ID, other); !errors.As(err, &ue) {
		t.Fatal("random user cannot deny")
	}
	if r2, _, err := h.eng.Deny(ctx, r2.ID, ic); err != nil || r2.Status != core.ActionDenied {
		t.Fatalf("IC deny: %v", err)
	}
}

func TestUnknownActionAndNoFreeForm(t *testing.T) {
	h := newEngine(t, true)
	if _, _, err := h.eng.Request(context.Background(), h.inc, ic, "shell", map[string]string{"cmd": "rm -rf /"}, "human"); err == nil {
		t.Fatal("non-catalog actions must be rejected")
	}
}
