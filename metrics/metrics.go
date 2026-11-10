// Package metrics exposes Prometheus metrics for incidentd. All metric names
// are prefixed incidentd_ and listed in Names (tests assert the shipped
// dashboards only use these).
package metrics

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Prom implements core.Metrics.
type Prom struct {
	reg *prometheus.Registry

	declared      *prometheus.CounterVec
	transitions   *prometheus.CounterVec
	resolveTimes  *prometheus.HistogramVec
	signals       *prometheus.CounterVec
	commands      *prometheus.CounterVec
	interactions  *prometheus.CounterVec
	sideEffects   *prometheus.CounterVec
	conflicts     prometheus.Counter
	triageRuns    *prometheus.CounterVec
	triageSeconds prometheus.Histogram
	triageTools   prometheus.Histogram
	triageDropped *prometheus.CounterVec
	llmTokens     *prometheus.CounterVec
	actions       *prometheus.CounterVec
	httpRequests  *prometheus.CounterVec
	httpSeconds   *prometheus.HistogramVec
	polls         *prometheus.CounterVec
}

// Names lists every metric family incidentd exports (without _bucket/_sum/_count suffixes).
var Names = []string{
	"incidentd_incidents_declared_total", "incidentd_incident_transitions_total", "incidentd_incident_duration_seconds",
	"incidentd_incidents_open", "incidentd_signals_total", "incidentd_commands_total", "incidentd_interactions_total",
	"incidentd_side_effect_failures_total", "incidentd_timeline_conflicts_total", "incidentd_triage_runs_total",
	"incidentd_triage_duration_seconds", "incidentd_triage_tool_calls", "incidentd_triage_dropped_total",
	"incidentd_llm_tokens_total", "incidentd_action_requests_total", "incidentd_remediation_executor_enabled",
	"incidentd_http_requests_total", "incidentd_http_request_duration_seconds", "incidentd_slo_polls_total",
	"incidentd_build_info",
}

// minutes-scale buckets for incident timings: 1m .. 24h
var incidentBuckets = []float64{60, 300, 600, 900, 1800, 3600, 7200, 14400, 28800, 86400}

// Options wires gauges that read live state at scrape time.
type Options struct {
	Store           core.Store
	ExecutorEnabled func(context.Context) bool
	Version         string
}

// New registers all metrics on a fresh registry.
func New(o Options) *Prom {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := func(c prometheus.Collector) { r.MustRegister(c) }
	p := &Prom{reg: r}
	p.declared = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_incidents_declared_total", Help: "Incidents declared, by severity and whether auto-declared."}, []string{"severity", "auto"})
	p.transitions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_incident_transitions_total", Help: "Lifecycle transitions."}, []string{"from", "to"})
	p.resolveTimes = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "incidentd_incident_duration_seconds", Help: "TTD/TTA/TTM/TTR observed when incidents resolve.", Buckets: incidentBuckets}, []string{"metric", "severity"})
	p.signals = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_signals_total", Help: "Signals processed, by source and outcome."}, []string{"source", "outcome"})
	p.commands = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_commands_total", Help: "Chat commands, by verb and result."}, []string{"verb", "result"})
	p.interactions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_interactions_total", Help: "Button clicks, by action and result."}, []string{"action", "result"})
	p.sideEffects = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_side_effect_failures_total", Help: "Failed side effects (paging, ticket, war room …), by step."}, []string{"step"})
	p.conflicts = prometheus.NewCounter(prometheus.CounterOpts{Name: "incidentd_timeline_conflicts_total", Help: "Timeline append races retried."})
	p.triageRuns = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_triage_runs_total", Help: "AI triage runs, by outcome."}, []string{"outcome"})
	p.triageSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "incidentd_triage_duration_seconds", Help: "AI triage wall time.", Buckets: []float64{5, 10, 20, 30, 45, 60, 90, 120}})
	p.triageTools = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "incidentd_triage_tool_calls", Help: "Tool calls per triage run.", Buckets: []float64{1, 2, 4, 6, 8, 10, 12, 15}})
	p.triageDropped = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_triage_dropped_total", Help: "Model output discarded by guardrails."}, []string{"kind"})
	p.llmTokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_llm_tokens_total", Help: "LLM tokens used by triage."}, []string{"direction"})
	p.actions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_action_requests_total", Help: "Remediation action request status changes."}, []string{"action", "risk", "status"})
	p.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_http_requests_total", Help: "HTTP requests, by route and status code."}, []string{"route", "code"})
	p.httpSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "incidentd_http_request_duration_seconds", Help: "HTTP latency (time to ack; chat work continues async).", Buckets: prometheus.DefBuckets}, []string{"route"})
	p.polls = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "incidentd_slo_polls_total", Help: "Multiplexer poll cycles, by result."}, []string{"result"})
	for _, c := range []prometheus.Collector{p.declared, p.transitions, p.resolveTimes, p.signals, p.commands, p.interactions, p.sideEffects,
		p.conflicts, p.triageRuns, p.triageSeconds, p.triageTools, p.triageDropped, p.llmTokens, p.actions, p.httpRequests, p.httpSeconds, p.polls} {
		f(c)
	}
	build := prometheus.NewGauge(prometheus.GaugeOpts{Name: "incidentd_build_info", Help: "Build information.", ConstLabels: prometheus.Labels{"version": o.Version}})
	build.Set(1)
	f(build)
	if o.Store != nil {
		f(&openCollector{store: o.Store, desc: prometheus.NewDesc("incidentd_incidents_open", "Open incidents by severity and state.", []string{"severity", "state"}, nil)})
	}
	if o.ExecutorEnabled != nil {
		f(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "incidentd_remediation_executor_enabled", Help: "1 if both kill-switch keys allow execution."}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if o.ExecutorEnabled(ctx) {
				return 1
			}
			return 0
		}))
	}
	return p
}

// Handler serves /metrics.
func (p *Prom) Handler() http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{Registry: p.reg})
}

type openCollector struct {
	store core.Store
	desc  *prometheus.Desc
}

func (c *openCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *openCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	states := []core.State{core.StateDeclared, core.StateInvestigating, core.StateIdentified, core.StateMitigating, core.StateMonitoring}
	incs, err := c.store.ListIncidents(ctx, core.IncidentFilter{States: states})
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc, err)
		return
	}
	counts := map[[2]string]float64{}
	for _, inc := range incs {
		counts[[2]string{inc.Severity.String(), string(inc.State)}]++
	}
	for k, v := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, v, k[0], k[1])
	}
}

func result(err error) string {
	if err == nil {
		return "ok"
	}
	var ue *core.UserError
	if errors.As(err, &ue) {
		return "user_error"
	}
	return "error"
}

func (p *Prom) IncidentDeclared(sev core.Severity, auto bool) {
	p.declared.WithLabelValues(sev.String(), strconv.FormatBool(auto)).Inc()
}
func (p *Prom) IncidentTransition(from, to core.State) {
	p.transitions.WithLabelValues(string(from), string(to)).Inc()
}
func (p *Prom) IncidentResolved(sev core.Severity, ttd, tta, ttm, ttr *time.Duration) {
	for name, d := range map[string]*time.Duration{"ttd": ttd, "tta": tta, "ttm": ttm, "ttr": ttr} {
		if d != nil {
			p.resolveTimes.WithLabelValues(name, sev.String()).Observe(d.Seconds())
		}
	}
}
func (p *Prom) SignalHandled(source string, outcome core.SignalOutcome, err error) {
	o := string(outcome)
	if err != nil {
		o = "error"
	}
	p.signals.WithLabelValues(source, o).Inc()
}
func (p *Prom) Command(verb string, err error) { p.commands.WithLabelValues(verb, result(err)).Inc() }
func (p *Prom) Interaction(action string, err error) {
	p.interactions.WithLabelValues(action, result(err)).Inc()
}
func (p *Prom) SideEffectFailed(step string) { p.sideEffects.WithLabelValues(step).Inc() }
func (p *Prom) TimelineConflict()            { p.conflicts.Inc() }
func (p *Prom) TriageRun(outcome string, d time.Duration, tools, dh, da int, in, out int64) {
	p.triageRuns.WithLabelValues(outcome).Inc()
	p.triageSeconds.Observe(d.Seconds())
	if outcome == "ok" {
		p.triageTools.Observe(float64(tools))
		p.triageDropped.WithLabelValues("hypothesis").Add(float64(dh))
		p.triageDropped.WithLabelValues("action").Add(float64(da))
		p.llmTokens.WithLabelValues("input").Add(float64(in))
		p.llmTokens.WithLabelValues("output").Add(float64(out))
	}
}
func (p *Prom) ActionRequest(actionID string, risk core.Risk, status core.ActionStatus) {
	p.actions.WithLabelValues(actionID, string(risk), string(status)).Inc()
}

// Poll records a Multiplexer poll result.
func (p *Prom) Poll(err error) {
	if err != nil {
		p.polls.WithLabelValues("error").Inc()
		return
	}
	p.polls.WithLabelValues("ok").Inc()
}

// HTTP records one request. route must be a fixed pattern (not the raw path).
func (p *Prom) HTTP(route string, code int, d time.Duration) {
	p.httpRequests.WithLabelValues(route, strconv.Itoa(code)).Inc()
	p.httpSeconds.WithLabelValues(route).Observe(d.Seconds())
}

var _ core.Metrics = (*Prom)(nil)
