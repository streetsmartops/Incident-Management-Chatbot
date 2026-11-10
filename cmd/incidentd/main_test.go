package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/config"
	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/remediation"
	"github.com/streetsmartops/incident-management-chatbot/test/fakes"
)

func testApp(t *testing.T) *App {
	t.Helper()
	yaml := `
database: {driver: memory}
slack: {enabled: true, announceChannelId: CANN}
pagerduty: {enabled: true, fromEmail: bot@example.com}
remediation: {catalogDir: ../../remediation/catalog}
llm: {enabled: true}
`
	env := map[string]string{"SLACK_BOT_TOKEN": "xoxb", "SLACK_SIGNING_SECRET": "sig", "PAGERDUTY_API_TOKEN": "pd", "PAGERDUTY_WEBHOOK_SECRET": "wh",
		"ANTHROPIC_API_KEY": "k", "INCIDENTD_ADMIN_TOKEN": "admin-tok", "REMEDIATION_ENABLED": "true"}
	cfg, err := config.Parse([]byte(yaml), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	app, err := Build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func TestRoutesAndAuth(t *testing.T) {
	app := testApp(t)
	srv := httptest.NewServer(app.Routes())
	defer srv.Close()
	get := func(path, auth string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if r := get("/healthz", ""); r.StatusCode != 200 {
		t.Fatalf("healthz %d", r.StatusCode)
	}
	if r := get("/readyz", ""); r.StatusCode != 200 {
		t.Fatalf("readyz %d", r.StatusCode)
	}
	for _, p := range []string{"/slack/commands", "/slack/interactivity", "/webhooks/pagerduty"} {
		resp, _ := srv.Client().Post(srv.URL+p, "application/x-www-form-urlencoded", strings.NewReader("text=declare"))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s must reject unsigned requests, got %d", p, resp.StatusCode)
		}
	}
	if r := get("/admin/timeline/verify?incident=INC-1", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin without token: %d", r.StatusCode)
	}
	if r := get("/admin/timeline/verify?incident=INC-1", "wrong"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin wrong token: %d", r.StatusCode)
	}
	// Teams is disabled: route not mounted.
	resp, _ := srv.Client().Post(srv.URL+"/teams/messages", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("teams disabled should not be routed: %d", resp.StatusCode)
	}
}

func TestKillSwitchNeedsBothKeys(t *testing.T) {
	app := testApp(t)
	srv := httptest.NewServer(app.Routes())
	defer srv.Close()
	ctx := context.Background()
	if app.Remedy.ExecutorEnabled(ctx) {
		t.Fatal("runtime flag defaults off")
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/remediation?enabled=true", nil)
	req.Header.Set("Authorization", "Bearer admin-tok")
	resp, err := srv.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("admin flip: %v %d", err, resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["executorEnabled"] != true {
		t.Fatalf("env + flag should enable: %+v", body)
	}
	app.Remedy.EnvEnabled = false
	if app.Remedy.ExecutorEnabled(ctx) {
		t.Fatal("env key off must disable regardless of flag")
	}
}

func TestEvalFixturesLoad(t *testing.T) {
	fx, err := loadFixtures("../../test/fixtures/incidents")
	if err != nil {
		t.Fatal(err)
	}
	if len(fx) < 20 {
		t.Fatalf("need >= 20 eval incidents, have %d", len(fx))
	}
	for _, f := range fx {
		if f.Source != "synthetic" || len(f.GroundTruth.Keywords) == 0 || len(f.Recordings) == 0 || len(f.GroundTruth.RelevantTools) == 0 {
			t.Errorf("%s: incomplete fixture", f.ID)
		}
		for _, tool := range f.GroundTruth.RelevantTools {
			if _, ok := f.Recordings[tool]; !ok {
				t.Errorf("%s: relevant tool %s has no recording", f.ID, tool)
			}
		}
	}
}

func TestImportRejectsOver90Days(t *testing.T) {
	if err := evalImportPagerDuty([]string{"-since", "2200h"}); err == nil || !strings.Contains(err.Error(), "90-day") {
		t.Fatalf("got %v", err)
	}
}

func TestEvalOnePipelineOffline(t *testing.T) {
	fx, err := loadFixtures("../../test/fixtures/incidents")
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := remediationCatalog(t)
	llm := &fakes.ScriptedLLM{Turns: []core.LlmTurn{
		{StopReason: core.StopToolUse, ToolCalls: []core.LlmToolCall{{ID: "c1", Name: "list_events", Input: json.RawMessage(`{}`)}}},
		{StopReason: core.StopToolUse, ToolCalls: []core.LlmToolCall{{ID: "s", Name: "submit_triage", Input: json.RawMessage(`{"summary":"s","blastRadius":{},"recommendedActions":[],"missingData":[],"hypotheses":[{"cause":"Bad deploy v2.3.1","confidence":0.8,"evidence":[{"tool":"list_events","ref":"c1","finding":"deploy at 02:40"}]}]}`)}}},
	}}
	rec := evalOne(context.Background(), llm, cat, core.MustRedactor(true), fx[0], 15, 10*time.Second)
	if rec.Error != "" || rec.Result == nil || len(rec.Result.Hypotheses) != 1 || rec.Source != "synthetic" {
		t.Fatalf("record: %+v", rec)
	}
}

func remediationCatalog(t *testing.T) (*remediation.Catalog, error) {
	c, err := remediation.LoadCatalog("../../remediation/catalog")
	if err != nil {
		t.Fatal(err)
	}
	return c, nil
}

func TestHTTPMetricsUseRoutePattern(t *testing.T) {
	app := testApp(t)
	srv := httptest.NewServer(app.Routes())
	defer srv.Close()
	_, _ = srv.Client().Get(srv.URL + "/healthz")
	_, _ = srv.Client().Get(srv.URL + "/no/such/path/12345")
	rec := httptest.NewRecorder()
	app.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	if !strings.Contains(out, `route="GET /healthz"`) || !strings.Contains(out, `route="unmatched"`) || strings.Contains(out, "12345") {
		t.Fatalf("route labels must be mux patterns, not raw paths:\n%s", grep(out, "incidentd_http_requests_total"))
	}
}

func grep(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
