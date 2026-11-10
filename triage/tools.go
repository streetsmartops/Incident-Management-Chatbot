// Package triage runs AI-assisted, read-only incident triage: an agent loop
// over tools mapped to ports, with budgets, untrusted-data handling,
// redaction, citation enforcement and catalog-restricted recommendations.
package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Executor exposes read-only tools. LiveExecutor uses the configured ports;
// the eval harness uses a replay executor with recorded outputs.
type Executor interface {
	Tools(inc *core.Incident) []core.LlmTool
	Call(ctx context.Context, inc *core.Incident, name string, input json.RawMessage) (string, error)
}

// SimilarSearcher finds similar past incidents (postmortem.Retriever).
type SimilarSearcher interface {
	Search(ctx context.Context, query string, k int) ([]core.SimilarIncident, error)
}

// LiveExecutor maps tools onto ports. Every result is redacted here, inside
// incidentd, before it reaches the LLM.
type LiveExecutor struct {
	Store    core.Store
	Obs      core.ObservabilityProvider
	Cloud    core.CloudProvider
	Slo      core.SloSource
	Paging   core.PagingProvider
	Similar  SimilarSearcher
	Redactor *core.Redactor
	Lookback time.Duration
	Now      func() time.Time
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var (
	str        = map[string]any{"type": "string"}
	minutesArg = map[string]any{"type": "integer", "description": "Minutes before now to search (default: incident window)."}
	k8sArgs    = map[string]any{"cluster": str, "namespace": str, "deployment": str}
)

// Tools returns the tools supported by configured adapters.
func (x *LiveExecutor) Tools(*core.Incident) []core.LlmTool {
	t := []core.LlmTool{{
		Name:        "get_incident_context",
		Description: "Get the incident's details, attached signals and recent timeline.",
		InputSchema: obj(map[string]any{}),
	}}
	if x.Slo != nil {
		t = append(t, core.LlmTool{Name: "get_slo_status", Description: "Get SLO status and recent burn-rate/error-budget events for the incident's SLOs (or a given slo_id).",
			InputSchema: obj(map[string]any{"slo_id": str})})
	}
	if x.Obs != nil {
		caps := x.Obs.Capabilities()
		if caps[core.CapMetrics] {
			t = append(t, core.LlmTool{Name: "query_metric", Description: "Run a read-only metrics timeseries query (Datadog query syntax).",
				InputSchema: obj(map[string]any{"query": str, "minutes_back": minutesArg}, "query")})
		}
		if caps[core.CapLogs] {
			t = append(t, core.LlmTool{Name: "search_logs", Description: "Search logs (Datadog log query syntax), newest first.",
				InputSchema: obj(map[string]any{"query": str, "minutes_back": minutesArg, "limit": map[string]any{"type": "integer"}}, "query")})
		}
		if caps[core.CapMonitors] {
			t = append(t, core.LlmTool{Name: "list_alerting_monitors", Description: "List monitors currently in Alert or Warn, optionally filtered by monitor tags.",
				InputSchema: obj(map[string]any{"tags": map[string]any{"type": "array", "items": str}})})
		}
		if caps[core.CapEvents] {
			t = append(t, core.LlmTool{Name: "list_events", Description: "List change events (deploys, config changes, alerts) in the window.",
				InputSchema: obj(map[string]any{"query": str, "minutes_back": minutesArg})})
		}
	}
	if x.Cloud != nil && len(x.Cloud.Scopes()) > 0 {
		t = append(t,
			core.LlmTool{Name: "list_k8s_workloads", Description: "List Deployments and readiness in a cluster namespace. Clusters: " + strings.Join(x.Cloud.Scopes(), ", "),
				InputSchema: obj(map[string]any{"cluster": str, "namespace": str}, "cluster", "namespace")},
			core.LlmTool{Name: "k8s_events", Description: "Recent Kubernetes events for a Deployment and its pods.",
				InputSchema: obj(k8sArgs, "cluster", "namespace", "deployment")},
			core.LlmTool{Name: "rollout_history", Description: "Rollout revisions and images for a Deployment, newest first.",
				InputSchema: obj(k8sArgs, "cluster", "namespace", "deployment")},
			core.LlmTool{Name: "pod_logs", Description: "Tail logs from up to 3 pods of a Deployment.",
				InputSchema: obj(map[string]any{"cluster": str, "namespace": str, "deployment": str, "tail": map[string]any{"type": "integer"}}, "cluster", "namespace", "deployment")},
		)
	}
	if x.Paging != nil {
		t = append(t, core.LlmTool{Name: "who_is_on_call", Description: "Who is on call for a service.",
			InputSchema: obj(map[string]any{"service": str}, "service")})
	}
	if x.Similar != nil {
		t = append(t, core.LlmTool{Name: "search_similar_incidents", Description: "Search past incidents and postmortems similar to a description.",
			InputSchema: obj(map[string]any{"query": str}, "query")})
	}
	return t
}

func (x *LiveExecutor) window(inc *core.Incident, minutes int) (time.Time, time.Time) {
	now := x.Now()
	if minutes > 0 {
		return now.Add(-time.Duration(minutes) * time.Minute), now
	}
	start := inc.DetectedAt
	if inc.ImpactStartedAt != nil {
		start = *inc.ImpactStartedAt
	}
	return start.Add(-x.Lookback), now
}

type args struct {
	Query       string   `json:"query"`
	MinutesBack int      `json:"minutes_back"`
	Limit       int      `json:"limit"`
	Tags        []string `json:"tags"`
	SloID       string   `json:"slo_id"`
	Cluster     string   `json:"cluster"`
	Namespace   string   `json:"namespace"`
	Deployment  string   `json:"deployment"`
	Tail        int      `json:"tail"`
	Service     string   `json:"service"`
}

func asJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("marshal error: %v", err)
	}
	return string(b)
}

// Call runs one tool. Unknown tools are an error (never executed).
func (x *LiveExecutor) Call(ctx context.Context, inc *core.Incident, name string, input json.RawMessage) (string, error) {
	offered := false
	for _, t := range x.Tools(inc) {
		if t.Name == name {
			offered = true
			break
		}
	}
	if !offered {
		return "", fmt.Errorf("tool %q is not available with the configured adapters", name)
	}
	var a args
	if len(input) > 0 {
		if err := json.Unmarshal(input, &a); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
	}
	out, err := x.call(ctx, inc, name, a)
	if err != nil {
		return "", err
	}
	return x.Redactor.Redact(out), nil
}

func (x *LiveExecutor) call(ctx context.Context, inc *core.Incident, name string, a args) (string, error) {
	ref := core.WorkloadRef{Cluster: a.Cluster, Namespace: a.Namespace, Kind: "Deployment", Name: a.Deployment}
	switch name {
	case "get_incident_context":
		evs, err := x.Store.Events(ctx, inc.ID)
		if err != nil {
			return "", err
		}
		if len(evs) > 40 {
			evs = evs[len(evs)-40:]
		}
		type tev struct {
			At      time.Time       `json:"at"`
			Kind    core.EventKind  `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		}
		var tl []tev
		for _, e := range evs {
			if e.Kind == core.EvTriageResult {
				continue // don't feed previous model output back as evidence
			}
			tl = append(tl, tev{e.At, e.Kind, e.Payload})
		}
		return asJSON(map[string]any{
			"key": inc.Key, "title": inc.Title, "severity": inc.Severity.String(), "state": inc.State, "services": inc.Services,
			"tenants": inc.Tenants, "sloRefs": inc.SloRefs, "detectedAt": inc.DetectedAt, "impactStartedAt": inc.ImpactStartedAt, "timeline": tl,
		}), nil
	case "get_slo_status":
		ids := inc.SloRefs
		if a.SloID != "" {
			ids = []string{a.SloID}
		}
		var out []any
		for _, id := range ids {
			slo, err := x.Slo.GetSlo(ctx, id)
			if err != nil {
				out = append(out, map[string]string{"slo_id": id, "error": err.Error()})
				continue
			}
			evs, _ := x.Slo.SloEvents(ctx, id, 10)
			out = append(out, map[string]any{"slo": slo, "recentEvents": evs})
		}
		return asJSON(out), nil
	case "query_metric":
		from, to := x.window(inc, a.MinutesBack)
		s, err := x.Obs.QueryMetric(ctx, a.Query, from, to)
		return asJSON(summarizeSeries(s)), err
	case "search_logs":
		from, to := x.window(inc, a.MinutesBack)
		lim := a.Limit
		if lim <= 0 || lim > 100 {
			lim = 50
		}
		l, err := x.Obs.SearchLogs(ctx, a.Query, from, to, lim)
		return asJSON(l), err
	case "list_alerting_monitors":
		m, err := x.Obs.MonitorsInState(ctx, []string{"alert", "warn"}, a.Tags)
		return asJSON(m), err
	case "list_events":
		from, to := x.window(inc, a.MinutesBack)
		e, err := x.Obs.Events(ctx, a.Query, from, to)
		return asJSON(e), err
	case "list_k8s_workloads":
		w, err := x.Cloud.ListWorkloads(ctx, a.Cluster, a.Namespace)
		return asJSON(w), err
	case "k8s_events":
		from, _ := x.window(inc, 0)
		e, err := x.Cloud.WorkloadEvents(ctx, ref, from)
		return asJSON(e), err
	case "rollout_history":
		h, err := x.Cloud.RolloutHistory(ctx, ref)
		return asJSON(h), err
	case "pod_logs":
		from, _ := x.window(inc, 0)
		l, err := x.Cloud.PodLogs(ctx, ref, from, a.Tail)
		return asJSON(l), err
	case "who_is_on_call":
		u, err := x.Paging.WhoIsOnCall(ctx, a.Service)
		return asJSON(u), err
	case "search_similar_incidents":
		r, err := x.Similar.Search(ctx, a.Query, 5)
		return asJSON(r), err
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// summarizeSeries reduces long series to stats + a few points to save tokens.
func summarizeSeries(ss []core.Series) []map[string]any {
	var out []map[string]any
	for _, s := range ss {
		if len(s.Points) == 0 {
			out = append(out, map[string]any{"metric": s.Metric, "scope": s.Scope, "points": 0})
			continue
		}
		mn, mx, sum := s.Points[0][1], s.Points[0][1], 0.0
		for _, p := range s.Points {
			mn, mx, sum = min(mn, p[1]), max(mx, p[1]), sum+p[1]
		}
		step := max(1, len(s.Points)/12)
		var sample [][2]any
		for i := 0; i < len(s.Points); i += step {
			sample = append(sample, [2]any{time.UnixMilli(int64(s.Points[i][0])).UTC().Format("15:04Z"), s.Points[i][1]})
		}
		out = append(out, map[string]any{
			"metric": s.Metric, "scope": s.Scope, "points": len(s.Points),
			"min": mn, "max": mx, "avg": sum / float64(len(s.Points)), "last": s.Points[len(s.Points)-1][1], "sample": sample,
		})
	}
	return out
}
