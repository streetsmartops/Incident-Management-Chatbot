package core_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

var t0 = time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)

func TestEveryAllowedTransitionAndRejections(t *testing.T) {
	want := map[core.State][]core.State{
		core.StateDetected:      {core.StateDeclared, core.StateDismissed},
		core.StateDeclared:      {core.StateInvestigating, core.StateResolved},
		core.StateInvestigating: {core.StateIdentified, core.StateMitigating, core.StateResolved},
		core.StateIdentified:    {core.StateMitigating, core.StateResolved},
		core.StateMitigating:    {core.StateMonitoring, core.StateInvestigating},
		core.StateMonitoring:    {core.StateResolved, core.StateInvestigating},
		core.StateResolved:      {core.StatePostmortem, core.StateInvestigating, core.StateClosed},
		core.StatePostmortem:    {core.StateClosed},
		core.StateClosed:        {},
		core.StateDismissed:     {},
	}
	all := []core.State{core.StateDetected, core.StateDeclared, core.StateInvestigating, core.StateIdentified, core.StateMitigating,
		core.StateMonitoring, core.StateResolved, core.StatePostmortem, core.StateClosed, core.StateDismissed}
	for from, allowed := range want {
		for _, to := range all {
			resolved := t0
			inc := &core.Incident{State: from, Severity: core.Sev3, ResolvedAt: &resolved}
			err := core.CheckTransition(inc, to, t0.Add(time.Hour))
			isAllowed := false
			for _, a := range allowed {
				if a == to {
					isAllowed = true
				}
			}
			if isAllowed && err != nil {
				t.Errorf("%s → %s should be allowed: %v", from, to, err)
			}
			if !isAllowed && err == nil {
				t.Errorf("%s → %s should be rejected", from, to)
			}
		}
	}
}

func TestResolvedToClosedBlockedForSev1And2(t *testing.T) {
	for _, sev := range []core.Severity{core.Sev1, core.Sev2} {
		inc := &core.Incident{State: core.StateResolved, Severity: sev}
		err := core.CheckTransition(inc, core.StateClosed, t0)
		var te *core.TransitionError
		if !errors.As(err, &te) || !strings.Contains(te.Reason, "postmortem") {
			t.Fatalf("%s: want postmortem-required error, got %v", sev, err)
		}
	}
	inc := &core.Incident{State: core.StateResolved, Severity: core.Sev3}
	if err := core.CheckTransition(inc, core.StateClosed, t0); err != nil {
		t.Fatalf("SEV3 may close without postmortem: %v", err)
	}
}

func TestReopenWindow(t *testing.T) {
	resolved := t0
	inc := &core.Incident{State: core.StateResolved, Severity: core.Sev2, ResolvedAt: &resolved}
	if err := core.CheckTransition(inc, core.StateInvestigating, t0.Add(71*time.Hour)); err != nil {
		t.Fatalf("reopen within 72h: %v", err)
	}
	if err := core.CheckTransition(inc, core.StateInvestigating, t0.Add(73*time.Hour)); err == nil {
		t.Fatal("reopen after 72h must fail")
	}
}

func TestApplyTransitionTimestamps(t *testing.T) {
	inc := &core.Incident{State: core.StateMitigating}
	core.ApplyTransition(inc, core.StateMonitoring, t0)
	if inc.MitigatedAt == nil || !inc.MitigatedAt.Equal(t0) {
		t.Fatal("mitigatedAt not set")
	}
	core.ApplyTransition(inc, core.StateResolved, t0.Add(time.Minute))
	core.ApplyTransition(inc, core.StateInvestigating, t0.Add(2*time.Minute))
	if inc.ResolvedAt != nil {
		t.Fatal("reopen must clear resolvedAt")
	}
	if !inc.MitigatedAt.Equal(t0) {
		t.Fatal("first mitigation time must be kept")
	}
}

func TestSeverityMatrixDefault(t *testing.T) {
	p := core.DefaultSeverityPolicy()
	cases := []struct {
		st   core.SloStatus
		tier core.TenantTier
		want core.Severity
	}{
		{core.SloBreached, core.TierEnterprise, core.Sev1},
		{core.SloBreached, core.TierBusiness, core.Sev2},
		{core.SloBreached, core.TierStarter, core.Sev3},
		{core.SloBreached, core.TierUnknown, core.Sev2},
		{core.SloBreached, "", core.Sev2},
		{core.SloAtRisk, core.TierEnterprise, core.Sev2},
		{core.SloAtRisk, core.TierBusiness, core.Sev3},
		{core.SloAtRisk, core.TierStarter, core.Sev4},
		{core.SloTimeout, core.TierEnterprise, core.Sev3},
		{core.SloNoData, core.TierStarter, core.Sev4},
		{core.SloOK, core.TierEnterprise, core.SevUnknown},
	}
	for _, c := range cases {
		if got := p.Classify(c.st, c.tier, 1); got != c.want {
			t.Errorf("%s×%s = %s, want %s", c.st, c.tier, got, c.want)
		}
	}
	if got := p.Classify(core.SloBreached, core.TierBusiness, 3); got != core.Sev1 {
		t.Errorf("multi-tenant escalation: got %s", got)
	}
	if got := p.Classify(core.SloBreached, core.TierEnterprise, 5); got != core.Sev1 {
		t.Errorf("escalation must cap at SEV1: got %s", got)
	}
}

func TestAutoDeclareRule(t *testing.T) {
	p := core.DefaultSeverityPolicy()
	sig := func(st core.SloStatus, tier core.TenantTier) core.Signal {
		return core.Signal{SloStatus: st, Tier: tier}
	}
	cases := []struct {
		name string
		sig  core.Signal
		n    int
		auto bool
	}{
		{"enterprise breached, persisted", sig(core.SloBreached, core.TierEnterprise), 2, true},
		{"enterprise breached, first eval", sig(core.SloBreached, core.TierEnterprise), 1, false},
		{"business breached SEV2", sig(core.SloBreached, core.TierBusiness), 2, true},
		{"enterprise at_risk SEV2", sig(core.SloAtRisk, core.TierEnterprise), 3, true},
		{"starter breached SEV3", sig(core.SloBreached, core.TierStarter), 5, false},
		{"unknown tier never auto", sig(core.SloBreached, core.TierUnknown), 5, false},
		{"timeout never auto", sig(core.SloTimeout, core.TierEnterprise), 5, false},
		{"pagerduty SEV1 hint", core.Signal{SeverityHint: core.Sev1}, 2, true},
		{"pagerduty SEV3 hint", core.Signal{SeverityHint: core.Sev3}, 2, false},
	}
	for _, c := range cases {
		d := p.Decide(c.sig, 1, c.n)
		if d.AutoDeclare != c.auto {
			t.Errorf("%s: auto=%v want %v (%s)", c.name, d.AutoDeclare, c.auto, d.Reason)
		}
	}
}

func TestCompanyPolicyOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	yaml := `
matrix:
  breached: {enterprise: 1, business: 1, starter: 2, unknown: 1}
  at_risk: {enterprise: 2, business: 2, starter: 3, unknown: 2}
  timeout: {enterprise: 4, business: 4, starter: 4, unknown: 4}
  no_data: {enterprise: 4, business: 4, starter: 4, unknown: 4}
multiTenantEscalation: 0
autoDeclare:
  enabled: true
  maxSeverity: 1
  consecutiveEvaluations: 3
  requireKnownTier: true
  neverFor: [timeout, no_data]
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := core.LoadSeverityPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Classify(core.SloBreached, core.TierBusiness, 1) != core.Sev1 {
		t.Fatal("override matrix not applied")
	}
	if p.Hash == "" || p.Hash == "builtin-default" {
		t.Fatal("policy hash must be recorded")
	}
	if d := p.Decide(core.Signal{SloStatus: core.SloAtRisk, Tier: core.TierEnterprise}, 1, 5); d.AutoDeclare {
		t.Fatal("SEV2 must not auto-declare when maxSeverity=1")
	}
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("matrix:\n  breached: {enterprise: 9}\n"), 0o600)
	if _, err := core.LoadSeverityPolicy(bad); err == nil {
		t.Fatal("invalid policy must fail validation")
	}
}

func TestTimelineHashChain(t *testing.T) {
	var evs []core.TimelineEvent
	var prev *core.TimelineEvent
	for i := 0; i < 5; i++ {
		ev, err := core.NewEvent("inc1", prev, t0.Add(time.Duration(i)*time.Second), core.SystemActor, core.EvNote, map[string]int{"i": i})
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
		prev = &evs[len(evs)-1]
	}
	if bad, err := core.VerifyChain(evs); err != nil {
		t.Fatalf("chain should verify, bad=%d err=%v", bad, err)
	}
	tampered := append([]core.TimelineEvent(nil), evs...)
	tampered[2].Payload = json.RawMessage(`{"i":99}`)
	if bad, err := core.VerifyChain(tampered); err == nil || bad != 3 {
		t.Fatalf("tampering must be detected at seq 3, got bad=%d err=%v", bad, err)
	}
	if _, err := core.VerifyChain(append(evs[:1:1], evs[2:]...)); err == nil {
		t.Fatal("deletion must be detected")
	}
}

func TestParseCommand(t *testing.T) {
	cmd, err := core.ParseCommand(`declare “Checkout 500s in EU” --sev 2 --service checkout,payments --private`)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Verb != "declare" || cmd.Args[0] != "Checkout 500s in EU" || cmd.Flags["sev"] != "2" || cmd.Flags["service"] != "checkout,payments" || cmd.Flags["private"] != "true" {
		t.Fatalf("bad parse: %+v", cmd)
	}
	cmd, err = core.ParseCommand(`run k8s.rollout_restart namespace=checkout deployment=api`)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[0] != "k8s.rollout_restart" || cmd.KV["namespace"] != "checkout" || cmd.KV["deployment"] != "api" {
		t.Fatalf("bad run parse: %+v", cmd)
	}
	cmd, _ = core.ParseCommand(`update "a=b is fine in quotes"`)
	if cmd.Args[0] != "a=b is fine in quotes" || len(cmd.KV) != 0 {
		t.Fatalf("quoted kv: %+v", cmd)
	}
	if _, err := core.ParseCommand("rm -rf /"); err == nil {
		t.Fatal("unknown verbs must be rejected")
	}
	if _, err := core.ParseCommand(`update "unterminated`); err == nil {
		t.Fatal("unterminated quote must error")
	}
	if cmd, _ := core.ParseCommand(""); cmd.Verb != "help" {
		t.Fatal("empty → help")
	}
}

func TestRedactor(t *testing.T) {
	r := core.MustRedactor(true)
	in := strings.Join([]string{
		"Authorization: Bearer abc.def.ghi123",
		"key=AKIAABCDEFGHIJKLMNOP",
		"postgres://admin:hunter2@db:5432/x",
		"password=s3cr3tvalue token: xoxb-1234567890-abcdefghij",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"user alice@example.com failed",
	}, "\n")
	out := r.Redact(in)
	for _, leak := range []string{"abc.def.ghi123", "AKIAABCDEFGHIJKLMNOP", "hunter2", "s3cr3tvalue", "xoxb-1234567890", "eyJhbGciOiJIUzI1NiJ9", "alice@example.com"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "user-") {
		t.Error("emails should be replaced with a stable hash token")
	}
	if r.Redact("p99 latency 4.1s on checkout") != "p99 latency 4.1s on checkout" {
		t.Error("benign text must be unchanged")
	}
}
