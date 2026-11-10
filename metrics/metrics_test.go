package metrics_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/metrics"
	"github.com/streetsmartops/incident-management-chatbot/test/fakes"
)

func scrape(t *testing.T, p *metrics.Prom) string {
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestMetricsFromServiceActivity(t *testing.T) {
	store := core.NewMemStore()
	p := metrics.New(metrics.Options{Store: store, ExecutorEnabled: func(context.Context) bool { return false }, Version: "test"})
	chat := fakes.NewChat("slack")
	svc := core.NewService(store, core.Integrations{Chats: map[string]core.ChatPort{"slack": chat}, Paging: &fakes.Paging{Err: errors.New("pd down")}},
		core.ServiceConfig{DefaultPlatform: "slack", PageAtOrAbove: core.Sev2}, core.DefaultSeverityPolicy(), nil)
	svc.Metrics = p
	user := core.UserRef{Platform: "slack", ID: "U1", Email: "a@example.com"}
	run := func(ch core.ChannelRef, text string) {
		cmd, _ := core.ParseCommand(text)
		_, _ = svc.Execute(context.Background(), core.CommandContext{Actor: user, Channel: ch}, cmd)
	}
	run(core.ChannelRef{}, `declare "x" --sev 1`)
	room := chat.Channels[0]
	run(room, "status investigating")
	run(room, "sev 9")
	run(room, `resolve "done"`)
	p.TriageRun("ok", 12*time.Second, 4, 1, 2, 1000, 200)
	p.HTTP("POST /slack/commands", 200, 50*time.Millisecond)
	p.Poll(nil)
	out := scrape(t, p)
	for _, want := range []string{
		`incidentd_incidents_declared_total{auto="false",severity="SEV1"} 1`,
		`incidentd_commands_total{result="user_error",verb="sev"} 1`,
		`incidentd_commands_total{result="ok",verb="resolve"} 1`,
		`incidentd_side_effect_failures_total{step="page"} 1`,
		`incidentd_incident_transitions_total{from="investigating",to="resolved"} 1`,
		`incidentd_incident_duration_seconds_count{metric="ttr",severity="SEV1"} 1`,
		`incidentd_triage_dropped_total{kind="action"} 2`,
		`incidentd_remediation_executor_enabled 0`,
		`incidentd_build_info{version="test"} 1`,
		`incidentd_http_requests_total{code="200",route="POST /slack/commands"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	// Open-incident gauge reflects store state (resolved => none open).
	if strings.Contains(out, "incidentd_incidents_open{") {
		t.Error("resolved incident must not be counted as open")
	}
	run(core.ChannelRef{}, `declare "y" --sev 2`)
	if !strings.Contains(scrape(t, p), `incidentd_incidents_open{severity="SEV2",state="declared"} 1`) {
		t.Error("open gauge not reported")
	}
}

var suffix = regexp.MustCompile(`_(bucket|sum|count)$`)

func TestDashboardsOnlyUseExportedMetrics(t *testing.T) {
	check := func(file string, re *regexp.Regexp, norm func(string) string) {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, m := range re.FindAllString(string(b), -1) {
			name := norm(m)
			if name == "" {
				continue
			}
			found++
			if !slices.Contains(metrics.Names, name) {
				t.Errorf("%s references unknown metric %q", file, m)
			}
		}
		if found == 0 {
			t.Errorf("%s: no metrics found", file)
		}
	}
	prom := regexp.MustCompile(`incidentd_[a-z_]+`)
	promNorm := func(m string) string { return suffix.ReplaceAllString(m, "") }
	check("../deploy/dashboards/grafana/incidentd.json", prom, promNorm)
	check("../deploy/monitoring/prometheus-rules.yaml", prom, promNorm)

	// Datadog: every incidentd.<name> must be the target of the OpenMetrics mapping.
	var mapping map[string]string
	b, _ := os.ReadFile("../deploy/dashboards/datadog/openmetrics-mapping.json")
	if err := json.Unmarshal(b, &mapping); err != nil {
		t.Fatal(err)
	}
	rev := map[string]string{}
	for src, dst := range mapping {
		rev[dst] = src
		ok := slices.Contains(metrics.Names, src) || slices.Contains(metrics.Names, src+"_total")
		if !ok {
			t.Errorf("mapping source %q is not exported", src)
		}
	}
	dd := regexp.MustCompile(`incidentd\.[a-z_]+`)
	check("../deploy/dashboards/datadog/incidentd.json", dd, func(m string) string {
		src, ok := rev[strings.TrimPrefix(m, "incidentd.")]
		if !ok {
			t.Errorf("datadog dashboard uses unmapped metric %q", m)
			return ""
		}
		if slices.Contains(metrics.Names, src) {
			return src
		}
		return src + "_total"
	})

	// The k8s annotation must carry the same mapping.
	dep, _ := os.ReadFile("../deploy/k8s/deployment.yaml")
	for src, dst := range mapping {
		if !strings.Contains(string(dep), `"`+src+`":"`+dst+`"`) {
			t.Errorf("deployment.yaml OpenMetrics annotation missing %s -> %s", src, dst)
		}
	}
}
