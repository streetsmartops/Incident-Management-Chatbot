package datadog_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/providers/datadog"
)

func TestDatadogEndpointsAndShapes(t *testing.T) {
	from := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("DD-API-KEY") != "api" || r.Header.Get("DD-APPLICATION-KEY") != "app" {
			t.Errorf("missing auth headers on %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/query":
			q := r.URL.Query()
			if q.Get("from") != strconv.FormatInt(from.Unix(), 10) || q.Get("to") != strconv.FormatInt(to.Unix(), 10) || q.Get("query") == "" {
				t.Errorf("query params: %v", q)
			}
			_, _ = w.Write([]byte(`{"status":"ok","res_type":"time_series","series":[{"metric":"trace.http.request.errors","scope":"service:checkout","pointlist":[[1762740000000,1.5],[1762740060000,null],[1762740120000,42]],"tag_set":["service:checkout","env:prod"]}],"from_date":1762740000000,"to_date":1762743600000}`))
		case "POST /api/v2/logs/events/search":
			b, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(b, &body)
			f := body["filter"].(map[string]any)
			if f["query"] != "service:checkout status:error" || f["from"] != "2026-11-10T02:00:00Z" || body["page"].(map[string]any)["limit"].(float64) != 50 {
				t.Errorf("logs body: %s", b)
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"AAA","type":"log","attributes":{"timestamp":"2026-11-10T02:42:00Z","service":"checkout","status":"error","message":"upstream timeout payments:443","host":"pod-1","tags":["env:prod"]}}],"links":{},"meta":{"status":"done"}}`))
		case "GET /api/v1/monitor":
			if r.URL.Query().Get("group_states") != "alert,warn" {
				t.Errorf("monitor query: %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`[{"id":101,"name":"Checkout 5xx","overall_state":"Alert","query":"sum(last_5m):...","tags":["service:checkout"],"type":"query alert"},{"id":102,"name":"OK one","overall_state":"OK","query":"x","type":"metric alert"}]`))
		case "GET /api/v2/events":
			if r.URL.Query().Get("filter[query]") != "source:kubernetes" {
				t.Errorf("events query: %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"E1","type":"event","attributes":{"timestamp":"2026-11-10T02:40:00Z","message":"Deployment checkout-api rolled out image v2.3.1","tags":["service:checkout"],"attributes":{"title":"Deploy checkout-api v2.3.1","source_type_name":"kubernetes"}}}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := datadog.New("datadoghq.com", srv.URL, "api", "app", srv.Client())
	ctx := context.Background()

	series, err := c.QueryMetric(ctx, "sum:trace.http.request.errors{service:checkout}", from, to)
	if err != nil || len(series) != 1 || len(series[0].Points) != 2 || series[0].Tags["env"] != "prod" {
		t.Fatalf("series: %+v %v", series, err)
	}
	logs, err := c.SearchLogs(ctx, "service:checkout status:error", from, to, 50)
	if err != nil || len(logs) != 1 || logs[0].Message != "upstream timeout payments:443" {
		t.Fatalf("logs: %+v %v", logs, err)
	}
	mons, err := c.MonitorsInState(ctx, []string{"alert", "warn"}, nil)
	if err != nil || len(mons) != 1 || mons[0].ID != "101" {
		t.Fatalf("monitors: %+v %v", mons, err)
	}
	evs, err := c.Events(ctx, "source:kubernetes", from, to)
	if err != nil || len(evs) != 1 || evs[0].Title != "Deploy checkout-api v2.3.1" {
		t.Fatalf("events: %+v %v", evs, err)
	}
}

func TestSiteDerivedBaseURL(t *testing.T) {
	// Behavioural check via an unreachable host is avoided; just ensure construction works for EU.
	c := datadog.New("datadoghq.eu", "", "a", "b", nil)
	if c.Name() != "datadog" || len(c.Capabilities()) != 4 {
		t.Fatal("unexpected client")
	}
}
