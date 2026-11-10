package postmortem_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/postmortem"
	"github.com/streetsmartops/incident-management-chatbot/test/fakes"
)

func ts(min int) *time.Time {
	t := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
	return &t
}

func TestMetricsMatchHandComputed(t *testing.T) {
	inc := &core.Incident{
		ImpactStartedAt: ts(0), DetectedAt: *ts(4), AcknowledgedAt: ts(7), DeclaredAt: ts(6),
		MitigatedAt: ts(35), ResolvedAt: ts(52),
	}
	m := postmortem.ComputeMetrics(inc)
	want := map[string]time.Duration{"ttd": 4 * time.Minute, "tta": 3 * time.Minute, "declare": 2 * time.Minute, "ttm": 35 * time.Minute, "ttr": 52 * time.Minute}
	got := map[string]*time.Duration{"ttd": m.TTD, "tta": m.TTA, "declare": m.TimeToDeclare, "ttm": m.TTM, "ttr": m.TTR}
	for k, w := range want {
		if got[k] == nil || *got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
	m = postmortem.ComputeMetrics(&core.Incident{DetectedAt: *ts(0)})
	if m.TTA != nil || m.TTR != nil || m.TTD == nil || *m.TTD != 0 {
		t.Fatalf("missing timestamps must yield nil durations: %+v", m)
	}
}

func TestBM25Ranking(t *testing.T) {
	ix := postmortem.NewIndex()
	ix.Add(core.SimilarIncident{Key: "INC-1", Title: "checkout"}, "Checkout 500s after deploy v2.3.1; bad config rolled back")
	ix.Add(core.SimilarIncident{Key: "INC-2", Title: "search"}, "Search latency from Elasticsearch GC pauses")
	ix.Add(core.SimilarIncident{Key: "INC-3", Title: "payments"}, "Payments timeouts, upstream provider outage; checkout degraded")
	res, _ := ix.Search(context.Background(), "checkout errors after deploy", 2)
	if len(res) != 2 || res[0].Key != "INC-1" || res[1].Key != "INC-3" {
		t.Fatalf("ranking: %+v", res)
	}
	if res, _ := ix.Search(context.Background(), "kafka", 5); len(res) != 0 {
		t.Fatalf("no match should return nothing: %+v", res)
	}
}

func TestBlamelessWarnings(t *testing.T) {
	w := postmortem.BlamelessWarnings(map[string]string{"root_cause": "Human error: the deployer should have known better", "summary": "Config drift"})
	if len(w) != 2 {
		t.Fatalf("warnings: %v", w)
	}
}

func TestDraftAndPublish(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemStore()
	chat := fakes.NewChat("slack")
	itsm := &fakes.Itsm{}
	svc := core.NewService(store, core.Integrations{Chats: map[string]core.ChatPort{"slack": chat}, Itsm: itsm}, core.ServiceConfig{DefaultPlatform: "slack"}, core.DefaultSeverityPolicy(), nil)
	clock := &fakes.Clock{T: *ts(4)}
	svc.Now = clock.Now
	// A past incident to retrieve.
	old, _ := svc.Declare(ctx, core.DeclareRequest{Title: "Checkout 500s after deploy", Severity: core.Sev3, Services: []string{"checkout"}, Actor: core.UserRef{Platform: "slack", ID: "U0", Email: "x@example.com"}})
	old.State = core.StateClosed
	_ = store.UpdateIncident(ctx, old)

	ic := core.UserRef{Platform: "slack", ID: "U1", Email: "ic@example.com"}
	inc, _ := svc.Declare(ctx, core.DeclareRequest{Title: "Checkout errors", Severity: core.Sev2, Services: []string{"checkout"}, Actor: ic,
		Signals: []core.Signal{{Source: "multiplexer", DedupKey: "k", SloID: "7", ObservedAt: *ts(0)}}})
	llmDraft, _ := json.Marshal(map[string]any{
		"summary": "Checkout returned 500s for 52 minutes.", "root_cause": "Config change in v2.3.1.", "contributing_factors": "No canary.",
		"went_well": "Fast rollback.", "went_badly": "Human error in review.", "action_items": []any{map[string]string{"title": "Add canary stage", "owner": "checkout team"}},
	})
	llm := &fakes.ScriptedLLM{Turns: []core.LlmTurn{{StopReason: core.StopToolUse, ToolCalls: []core.LlmToolCall{{ID: "p1", Name: "submit_postmortem", Input: llmDraft}}}}}
	eng := &postmortem.Engine{Store: store, LLM: llm, Itsm: itsm, Redactor: core.MustRedactor(true), Now: clock.Now,
		Slo: &fakes.Slo{Burned: 18.5, Quality: "stale"}}
	inc.ResolvedAt = ts(52)
	pm, msg, err := eng.Draft(ctx, inc)
	if err != nil {
		t.Fatal(err)
	}
	if pm.Metrics.BudgetBurned == nil || *pm.Metrics.BudgetBurned != 18.5 || pm.Metrics.BudgetQuality != "stale" {
		t.Fatalf("budget: %+v", pm.Metrics)
	}
	if len(pm.SimilarIncidents) != 1 || pm.SimilarIncidents[0].Key != old.Key {
		t.Fatalf("similar: %+v", pm.SimilarIncidents)
	}
	if !strings.Contains(msg.Text, "Blameless check") || !strings.Contains(pm.Sections["impact"], "18.5") {
		t.Fatalf("draft message: %s", msg.Text)
	}
	if strings.Contains(llm.Sent[0], "ic@example.com") {
		t.Fatal("facts sent to the LLM must be redacted")
	}
	pm, _, err = eng.Publish(ctx, inc)
	if err != nil || pm.Status != "published" || pm.ActionItems[0].TicketRef == "" || len(itsm.Items) != 1 {
		t.Fatalf("publish: %+v %v", pm, err)
	}
	// Publishing again must not refile.
	if _, _, err := eng.Publish(ctx, inc); err != nil || len(itsm.Items) != 1 {
		t.Fatal("publish must be idempotent for filed items")
	}
}

func TestDraftWithoutLLMLeavesTodos(t *testing.T) {
	store := core.NewMemStore()
	eng := &postmortem.Engine{Store: store, Redactor: core.MustRedactor(false), Now: time.Now}
	pm, _, err := eng.Draft(context.Background(), &core.Incident{ID: "x", Key: "INC-1", DetectedAt: time.Now()})
	if err != nil || !strings.Contains(pm.Sections["root_cause"], "IC") || pm.Metrics.BudgetQuality != "unavailable" {
		t.Fatalf("%+v %v", pm, err)
	}
}
