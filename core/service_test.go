package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/test/fakes"
)

type harness struct {
	svc    *core.Service
	store  *core.MemStore
	chat   *fakes.Chat
	paging *fakes.Paging
	itsm   *fakes.Itsm
	slo    *fakes.Slo
	clock  *fakes.Clock
}

var (
	alice = core.UserRef{Platform: "slack", ID: "UALICE", Email: "alice@example.com"}
	bob   = core.UserRef{Platform: "slack", ID: "UBOB", Email: "bob@example.com"}
	annCh = core.ChannelRef{Platform: "slack", ID: "CANNOUNCE", Name: "incidents"}
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		store:  core.NewMemStore(),
		chat:   fakes.NewChat("slack"),
		paging: &fakes.Paging{OnCall: map[string][]core.UserRef{"checkout": {{Platform: "pagerduty", ID: "P1", Email: "oncall@example.com"}}}},
		itsm:   &fakes.Itsm{},
		slo: &fakes.Slo{
			Slos:    map[string]core.Slo{"7": {ID: "7", Name: "Checkout availability", Service: "checkout", Status: core.SloBreached}},
			Tenants: map[string]core.Tenant{"7": {ID: "1", Slug: "acme", Tier: core.TierEnterprise}},
		},
		clock: &fakes.Clock{T: time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)},
	}
	h.chat.UsersByMail["oncall@example.com"] = core.UserRef{Platform: "slack", ID: "UONCALL", Email: "oncall@example.com"}
	cfg := core.ServiceConfig{
		DefaultPlatform:  "slack",
		AnnounceChannels: map[string]core.ChannelRef{"slack": annCh},
		PageAtOrAbove:    core.Sev2,
		TicketAtOrAbove:  core.Sev3,
		StatusCadence:    map[core.Severity]time.Duration{core.Sev1: 30 * time.Minute, core.Sev2: time.Hour},
		Admins:           []string{"admin@example.com"},
	}
	h.svc = core.NewService(h.store, core.Integrations{
		Chats: map[string]core.ChatPort{"slack": h.chat}, Paging: h.paging, Itsm: h.itsm, Slo: h.slo,
	}, cfg, core.DefaultSeverityPolicy(), nil)
	h.svc.Now = h.clock.Now
	h.svc.NewID = fakes.IDs("id")
	return h
}

func (h *harness) run(t *testing.T, actor core.UserRef, ch core.ChannelRef, text string, mentions ...core.UserRef) (core.Message, error) {
	t.Helper()
	cmd, err := core.ParseCommand(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return h.svc.Execute(context.Background(), core.CommandContext{Actor: actor, Channel: ch, Mentions: mentions}, cmd)
}

func TestDeclareToResolveEndToEnd(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	msg, err := h.run(t, alice, annCh, `declare "Checkout 500s" --sev 2 --service checkout`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Text, "INC-2026-0001") {
		t.Fatalf("reply: %q", msg.Text)
	}
	if len(h.chat.Channels) != 1 || !strings.HasPrefix(h.chat.Channels[0].Name, "inc-2026-0001-checkout-500s") {
		t.Fatalf("war room: %+v", h.chat.Channels)
	}
	room := h.chat.Channels[0]
	if len(h.paging.Pages) != 1 || len(h.itsm.Tickets) != 1 {
		t.Fatalf("pages=%v tickets=%v", h.paging.Pages, h.itsm.Tickets)
	}
	invited := h.chat.Invites[room.ID]
	if len(invited) < 2 {
		t.Fatalf("IC and on-call should be invited, got %+v", invited)
	}
	steps := []string{"status investigating", "status identified", "status mitigating", "status monitoring", `update "Rolled back; error rate normal"`, `resolve "Bad deploy rolled back"`}
	for _, s := range steps {
		if _, err := h.run(t, alice, room, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		h.clock.Advance(time.Minute)
	}
	inc, err := h.store.FindIncidentByChannel(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != core.StateResolved || inc.MitigatedAt == nil || inc.ResolvedAt == nil {
		t.Fatalf("final state: %+v", inc)
	}
	if len(h.paging.Resolved) != 1 {
		t.Fatal("page should be resolved")
	}
	evs, _ := h.store.Events(ctx, inc.ID)
	if _, err := core.VerifyChain(evs); err != nil {
		t.Fatalf("chain: %v", err)
	}
	kinds := map[core.EventKind]int{}
	for _, e := range evs {
		kinds[e.Kind]++
	}
	for _, k := range []core.EventKind{core.EvDetected, core.EvDeclared, core.EvRoleAssigned, core.EvWarRoomCreated, core.EvPaged, core.EvTicketCreated, core.EvStatusPosted} {
		if kinds[k] == 0 {
			t.Errorf("missing timeline event %s", k)
		}
	}
	if kinds[core.EvTransition] != 5 {
		t.Errorf("want 5 transitions, got %d", kinds[core.EvTransition])
	}
	// SEV2 cannot be closed without postmortem.
	if _, err := h.run(t, alice, room, "postmortem publish"); err == nil {
		t.Fatal("publish before draft must fail")
	}
}

func TestEveryCommandWritesTimelineEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.run(t, alice, annCh, `declare "x" --sev 3`); err != nil {
		t.Fatal(err)
	}
	room := h.chat.Channels[0]
	inc, _ := h.store.FindIncidentByChannel(ctx, room)
	count := func() int { e, _ := h.store.Events(ctx, inc.ID); return len(e) }
	for _, c := range []struct {
		text     string
		mentions []core.UserRef
	}{
		{"sev 2 --reason worse", nil},
		{"role comms", []core.UserRef{bob}},
		{"status investigating", nil},
		{`update "looking"`, nil},
		{`resolve "done"`, nil},
	} {
		before := count()
		if _, err := h.run(t, alice, room, c.text, c.mentions...); err != nil {
			t.Fatalf("%s: %v", c.text, err)
		}
		if count() <= before {
			t.Errorf("%q did not write a timeline event", c.text)
		}
	}
}

func TestRBAC(t *testing.T) {
	h := newHarness(t)
	if _, err := h.run(t, alice, annCh, `declare "x" --sev 3`); err != nil {
		t.Fatal(err)
	}
	room := h.chat.Channels[0]
	var ue *core.UserError
	if _, err := h.run(t, bob, room, "sev 1"); !errors.As(err, &ue) {
		t.Fatalf("non-IC sev change must be refused, got %v", err)
	}
	if _, err := h.run(t, bob, room, "role ic me"); !errors.As(err, &ue) {
		t.Fatalf("taking an occupied IC role must be refused, got %v", err)
	}
	admin := core.UserRef{Platform: "slack", ID: "UADM", Email: "admin@example.com"}
	if _, err := h.run(t, admin, room, "sev 2"); err != nil {
		t.Fatalf("admin override: %v", err)
	}
	if _, err := h.run(t, alice, room, "role comms", bob); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, bob, room, `update "comms can post updates"`); err != nil {
		t.Fatalf("comms update: %v", err)
	}
	if _, err := h.run(t, bob, annCh, "timeline"); err == nil {
		t.Fatal("commands outside a war room need --incident")
	}
	if _, err := h.run(t, bob, annCh, "timeline --incident inc-2026-0001"); err != nil {
		t.Fatalf("--incident lookup: %v", err)
	}
}

func TestVacantICCanBeTaken(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sig := core.Signal{Source: "multiplexer", DedupKey: "multiplexer:guardrail:7", SloID: "7", Title: "Checkout availability breached", SloStatus: core.SloBreached}
	if _, _, err := h.svc.HandleSignal(ctx, sig); err != nil {
		t.Fatal(err)
	}
	out, inc, err := h.svc.HandleSignal(ctx, sig)
	if err != nil || out != core.OutcomeAutoDeclared {
		t.Fatalf("outcome %s err %v", out, err)
	}
	if _, ok := inc.IC(); ok {
		t.Fatal("auto-declared incident has no IC")
	}
	if _, err := h.run(t, bob, *inc.WarRoom, "role ic me"); err != nil {
		t.Fatalf("anyone may take a vacant IC: %v", err)
	}
}

func TestSignalAutoDeclareDedupeAndProposal(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sig := core.Signal{Source: "multiplexer", DedupKey: "multiplexer:guardrail:7", SloID: "7", Title: "Checkout availability breached", SloStatus: core.SloBreached}

	out, _, err := h.svc.HandleSignal(ctx, sig)
	if err != nil || out != core.OutcomeProposed {
		t.Fatalf("first observation should propose, got %s %v", out, err)
	}
	out, inc, err := h.svc.HandleSignal(ctx, sig)
	if err != nil || out != core.OutcomeAutoDeclared {
		t.Fatalf("second observation should auto-declare, got %s %v", out, err)
	}
	if inc.Severity != core.Sev1 || !inc.AutoDeclared || inc.Tenants[0] != "acme" {
		t.Fatalf("incident: %+v", inc)
	}
	// Same breach arriving via PagerDuty (correlated dedup key) attaches.
	pd := core.Signal{Source: "pagerduty", DedupKey: "multiplexer:guardrail:7", SloID: "7", Title: "PD", Acknowledged: true}
	out, got, err := h.svc.HandleSignal(ctx, pd)
	if err != nil || out != core.OutcomeAttached || got.ID != inc.ID {
		t.Fatalf("PD duplicate should attach to %s, got %s %v", inc.ID, out, err)
	}
	inc, _ = h.store.GetIncident(ctx, inc.ID)
	if inc.AcknowledgedAt == nil {
		t.Fatal("ack should set acknowledgedAt")
	}
	all, _ := h.store.ListIncidents(ctx, core.IncidentFilter{})
	if len(all) != 1 {
		t.Fatalf("duplicate PD + Multiplexer signal must produce 1 incident, got %d", len(all))
	}

	// Starter tier: SEV3 → proposal only, posted once per streak.
	h.slo.Slos["9"] = core.Slo{ID: "9", Service: "search", Status: core.SloBreached}
	h.slo.Tenants["9"] = core.Tenant{Slug: "tiny", Tier: core.TierStarter}
	s9 := core.Signal{Source: "multiplexer", DedupKey: "multiplexer:guardrail:9", SloID: "9", Title: "Search breached", SloStatus: core.SloBreached}
	before := len(h.chat.PostsTo(annCh.ID))
	for i := 0; i < 3; i++ {
		_, _, _ = h.svc.HandleSignal(ctx, s9)
	}
	proposals := 0
	for _, m := range h.chat.PostsTo(annCh.ID)[before:] {
		if strings.HasPrefix(m.Title, "Proposed SEV3") {
			proposals++
		}
	}
	if proposals != 1 {
		t.Fatalf("want exactly 1 proposal for a 3-poll streak, got %d", proposals)
	}

	// Unknown tier never auto-declares.
	h.slo.Slos["11"] = core.Slo{ID: "11", Service: "auth", Status: core.SloBreached}
	s11 := core.Signal{Source: "multiplexer", DedupKey: "multiplexer:guardrail:11", SloID: "11", Title: "Auth breached", SloStatus: core.SloBreached}
	for i := 0; i < 4; i++ {
		if out, _, _ := h.svc.HandleSignal(ctx, s11); out == core.OutcomeAutoDeclared {
			t.Fatal("unknown tier must not auto-declare")
		}
	}
}

func TestProposalButtonDeclares(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.slo.Tenants["7"] = core.Tenant{Slug: "tiny", Tier: core.TierStarter}
	sig := core.Signal{ID: "sig-1", Source: "multiplexer", DedupKey: "multiplexer:guardrail:7", SloID: "7", Title: "Checkout", SloStatus: core.SloBreached}
	if out, _, _ := h.svc.HandleSignal(ctx, sig); out != core.OutcomeProposed {
		t.Fatalf("want proposal, got %s", out)
	}
	msg, err := h.svc.HandleInteraction(ctx, core.CommandContext{Actor: bob}, "declare_signal", "sig-1|3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Text, "INC-2026-0001") {
		t.Fatalf("reply %q", msg.Text)
	}
	msg, _ = h.svc.HandleInteraction(ctx, core.CommandContext{Actor: alice}, "declare_signal", "sig-1|3")
	if !strings.Contains(msg.Text, "Already declared") {
		t.Fatalf("double click must not double-declare: %q", msg.Text)
	}
}

func TestSideEffectFailureIsRecordedNotFatal(t *testing.T) {
	h := newHarness(t)
	h.paging.Err = errors.New("pagerduty 503")
	if _, err := h.run(t, alice, annCh, `declare "x" --sev 1 --service checkout`); err != nil {
		t.Fatalf("declare must succeed despite paging failure: %v", err)
	}
	inc, _ := h.store.FindIncidentByKey(context.Background(), "INC-2026-0001")
	evs, _ := h.store.Events(context.Background(), inc.ID)
	found := false
	for _, e := range evs {
		if e.Kind == core.EvError && strings.Contains(string(e.Payload), "pagerduty 503") {
			found = true
		}
	}
	if !found {
		t.Fatal("paging failure must be recorded in the timeline")
	}
}

func TestStatusCadenceNudge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.run(t, alice, annCh, `declare "x" --sev 1`); err != nil {
		t.Fatal(err)
	}
	room := h.chat.Channels[0]
	nudges := func() int {
		n := 0
		for _, m := range h.chat.PostsTo(room.ID) {
			if strings.Contains(m.Text, "overdue") {
				n++
			}
		}
		return n
	}
	h.clock.Advance(29 * time.Minute)
	h.svc.Tick(ctx)
	if nudges() != 0 {
		t.Fatal("no nudge before cadence")
	}
	h.clock.Advance(2 * time.Minute)
	h.svc.Tick(ctx)
	h.svc.Tick(ctx)
	if nudges() != 1 {
		t.Fatalf("want 1 nudge, got %d", nudges())
	}
	if _, err := h.run(t, alice, room, `update "still working"`); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(20 * time.Minute)
	h.svc.Tick(ctx)
	if nudges() != 1 {
		t.Fatal("update must reset cadence")
	}
}

func TestTeamsFallbackWithoutWarRoom(t *testing.T) {
	h := newHarness(t)
	teams := fakes.NewChat("teams")
	teams.NoWarRooms = true
	h2 := core.NewService(h.store, core.Integrations{Chats: map[string]core.ChatPort{"teams": teams}},
		core.ServiceConfig{DefaultPlatform: "teams", AnnounceChannels: map[string]core.ChannelRef{"teams": {Platform: "teams", ID: "19:ann"}}},
		core.DefaultSeverityPolicy(), nil)
	user := core.UserRef{Platform: "teams", ID: "29:1", Email: "carol@example.com"}
	cmd, _ := core.ParseCommand(`declare "Teams incident" --sev 3`)
	if _, err := h2.Execute(context.Background(), core.CommandContext{Actor: user, Channel: core.ChannelRef{Platform: "teams", ID: "19:ann"}}, cmd); err != nil {
		t.Fatal(err)
	}
	cmd, _ = core.ParseCommand(`status investigating --incident INC-2026-0001`)
	if _, err := h2.Execute(context.Background(), core.CommandContext{Actor: user, Channel: core.ChannelRef{Platform: "teams", ID: "19:ann"}}, cmd); err != nil {
		t.Fatalf("teams command via --incident: %v", err)
	}
	if len(teams.PostsTo("19:ann")) == 0 {
		t.Fatal("teams announce channel should get the declaration")
	}
}

func TestMultiTenantEscalation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Three starter tenants (SEV3 each) breaching the same service escalate to SEV2 → auto-declare.
	for i, id := range []string{"21", "22", "23"} {
		h.slo.Slos[id] = core.Slo{ID: id, Service: "search", Status: core.SloBreached}
		h.slo.Tenants[id] = core.Tenant{Slug: "t" + id, Tier: core.TierStarter}
		sig := core.Signal{Source: "multiplexer", DedupKey: "multiplexer:guardrail:" + id, SloID: id, Service: "search", Title: "Search breached " + id, SloStatus: core.SloBreached}
		var out core.SignalOutcome
		for j := 0; j < 2; j++ {
			out, _, _ = h.svc.HandleSignal(ctx, sig)
		}
		if i < 2 && out == core.OutcomeAutoDeclared {
			t.Fatalf("tenant %d alone (SEV3) must not auto-declare", i)
		}
		if i == 2 && out != core.OutcomeAutoDeclared {
			t.Fatalf("third tenant should escalate to SEV2 and auto-declare, got %s", out)
		}
	}
}
