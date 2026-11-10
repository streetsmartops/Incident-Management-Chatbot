# Dashboards and alerting

`incidentd` exports Prometheus metrics on an **internal** port (`server.metricsListen`, default
`:9090`, path `/metrics`). The ingress never exposes it. This repo ships:

| File | What |
|---|---|
| `deploy/dashboards/grafana/incidentd.json` | Grafana dashboard (Prometheus datasource) |
| `deploy/dashboards/datadog/incidentd.json` | Datadog dashboard (OpenMetrics check) |
| `deploy/dashboards/datadog/openmetrics-mapping.json` | Prometheus → Datadog metric name mapping |
| `deploy/monitoring/prometheus-rules.yaml` | Alert rules (plain Prometheus) |
| `deploy/monitoring/prometheusrule.yaml` | Same rules as a Prometheus Operator `PrometheusRule` |
| `deploy/monitoring/servicemonitor.yaml` | Prometheus Operator scrape config |
| `deploy/dashboards/gen_dashboards.py` | Generator for all of the above. Edit this, not the JSON |

`metrics/metrics_test.go` fails CI if a dashboard or alert references a metric that `incidentd`
does not export. Every PromQL expression was validated with the Prometheus parser.

## What the dashboard shows

| Row | Panels |
|---|---|
| Overview | Open incidents · open SEV1/SEV2 · declared and auto-declared in range · remediation executor state · version |
| Incident response | Open incidents by severity · declarations by severity · signals by source/outcome · TTD/TTA/TTM/TTR p50 · TTR p90 by severity · transitions |
| Bot health | Commands by result and verb · side-effect failures by step (paging, ticket, war room) · HTTP p95 by route (Slack needs < 3 s) · 4xx/5xx by route · SLO polls and timeline conflicts |
| AI triage | Runs by outcome · duration p50/p95 · guardrail drops (uncited hypotheses, non-catalog actions) · LLM tokens per hour |
| Remediation | Action requests by status · by action and risk |

## Metrics reference

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `incidentd_incidents_open` | gauge | `severity`, `state` | Incidents in declared…monitoring (read from the DB at scrape time) |
| `incidentd_incidents_declared_total` | counter | `severity`, `auto` | Declarations |
| `incidentd_incident_transitions_total` | counter | `from`, `to` | Lifecycle transitions |
| `incidentd_incident_duration_seconds` | histogram | `metric` (ttd/tta/ttm/ttr), `severity` | Observed when an incident resolves |
| `incidentd_signals_total` | counter | `source`, `outcome` | attached / auto_declared / proposed / ignored / error |
| `incidentd_commands_total` | counter | `verb`, `result` | ok / user_error / error |
| `incidentd_interactions_total` | counter | `action`, `result` | Button clicks |
| `incidentd_side_effect_failures_total` | counter | `step` | page, itsm_ticket, war_room, invite_*, post_* … |
| `incidentd_timeline_conflicts_total` | counter | | Concurrent append races retried |
| `incidentd_triage_runs_total` | counter | `outcome` | ok / error |
| `incidentd_triage_duration_seconds` | histogram | | Wall time |
| `incidentd_triage_tool_calls` | histogram | | Tool calls per run |
| `incidentd_triage_dropped_total` | counter | `kind` | hypothesis / action dropped by guardrails |
| `incidentd_llm_tokens_total` | counter | `direction` | input / output |
| `incidentd_action_requests_total` | counter | `action`, `risk`, `status` | Status changes (pending, dry_run_only, executed, …) |
| `incidentd_remediation_executor_enabled` | gauge | | 1 if both kill-switch keys are on |
| `incidentd_http_requests_total` | counter | `route`, `code` | `route` is the mux pattern, never a raw path |
| `incidentd_http_request_duration_seconds` | histogram | `route` | Time to ack |
| `incidentd_slo_polls_total` | counter | `result` | Multiplexer poll cycles |
| `incidentd_build_info` | gauge | `version` | Always 1 |

Go runtime and process metrics (`go_*`, `process_*`) are exported too.

## Setup: Prometheus Operator + Grafana

1. Scrape the metrics port:

   ```bash
   kubectl apply -f deploy/monitoring/servicemonitor.yaml
   ```

   Adjust the `release: prometheus` label to match your Prometheus `serviceMonitorSelector`.

2. Load the alerts:

   ```bash
   kubectl apply -f deploy/monitoring/prometheusrule.yaml
   ```

3. Import the dashboard in Grafana: **Dashboards, then New, then Import**, upload `deploy/dashboards/grafana/incidentd.json`, then pick your Prometheus datasource (the dashboard has a `datasource` variable). With the Grafana sidecar, put the JSON in a ConfigMap labelled `grafana_dashboard: "1"` instead:

   ```bash
   kubectl -n monitoring create configmap incidentd-dashboard --from-file=deploy/dashboards/grafana/incidentd.json
   ```

   ```bash
   kubectl -n monitoring label configmap incidentd-dashboard grafana_dashboard=1
   ```

4. Check that Prometheus' **Targets** page shows the `incidentd` endpoints as `UP`.

## Setup: plain Prometheus

The pod template carries `prometheus.io/scrape: "true"`, `prometheus.io/port: "9090"` and
`prometheus.io/path: /metrics`. These work with the common annotation-based `kubernetes_sd_configs` job.
Without service discovery, add a static job:

```yaml
scrape_configs:
  - job_name: incidentd
    static_configs:
      - targets: ["incidentd-metrics.incidentd.svc:9090"]
rule_files:
  - /etc/prometheus/rules/incidentd.yaml   # deploy/monitoring/prometheus-rules.yaml
```

The `IncidentdDown` alert expects `job="incidentd"`. Rename it if your job is called something else.

## Setup: Datadog

1. The pod template already carries the Agent's OpenMetrics annotation
   (`ad.datadoghq.com/incidentd.checks`). The Datadog Agent in the cluster picks it up
   automatically: endpoint `http://%%host%%:9090/metrics`, namespace `incidentd`, with explicit metric
   renames from `openmetrics-mapping.json`.
2. Import the dashboard: **Dashboards, then New Dashboard, then the gear icon, then Import dashboard JSON**, and paste
   `deploy/dashboards/datadog/incidentd.json`. Or use the API:

   ```bash
   curl -X POST "https://api.datadoghq.com/api/v1/dashboard" \
     -H "DD-API-KEY: $DD_API_KEY" -H "DD-APPLICATION-KEY: $DD_APP_KEY" \
     -H "Content-Type: application/json" -d @deploy/dashboards/datadog/incidentd.json
   ```

   The application key for this call needs dashboard write access. It is **not** the bot's read-only key.
3. Check the metric names once data arrives. **UNVERIFIED:** Datadog's docs confirm the
   namespace prefix and that histograms arrive as `.bucket`, `.sum` and `.count`, but do not state how the
   OpenMetrics V2 check names counters. The dashboard assumes counters appear as
   `incidentd.<name>.count` (for example `incidentd.commands.count`). Open **Metrics, then Explorer**,
   search `incidentd.`, and if the names differ, edit `DD_MAP` / the queries in
   `gen_dashboards.py` and regenerate.
4. Monitors: recreate the alerts in `prometheus-rules.yaml` as Datadog monitors on the same
   metrics. The thresholds and runbook links carry over.

## Alerts

| Alert | Severity | Fires when | Runbook |
|---|---|---|---|
| `IncidentdDown` | page | Scrape target down for 5 minutes | [operations.md#incidentd-down](operations.md#incidentd-down) |
| `IncidentdSloPollFailing` | page | No successful Multiplexer poll for 10 minutes | [#slo-polling-failing](operations.md#slo-polling-failing) |
| `IncidentdSideEffectsFailing` | ticket | More than 3 failures of one side effect in 15 minutes | [#side-effect-failures](operations.md#side-effect-failures) |
| `IncidentdSignatureFailures` | ticket | More than 10 rejected chat/webhook requests in 15 minutes | [#signature-failures](operations.md#signature-failures) |
| `IncidentdTriageErrors` | ticket | More than 3 triage errors in 1 hour | [#triage-errors](operations.md#triage-errors) |
| `IncidentdSlowAck` | ticket | Slack p95 ack time above 2 s for 10 minutes | [#slow-acks](operations.md#slow-acks) |
| `IncidentdExecutorEnabled` | info | Remediation execution is enabled | [#kill-switch](operations.md#kill-switch) |

Route `page` alerts to a PagerDuty service that does **not** depend on `incidentd`. The bot
can't page you about itself being down.

## Changing dashboards

Edit `deploy/dashboards/gen_dashboards.py`, then run:

```bash
python3 deploy/dashboards/gen_dashboards.py
```

```bash
go test ./metrics/
```

If you add a metric, add it to `metrics.Names` and to `DD_MAP`, and regenerate. The test enforces
that the k8s annotation carries the same mapping.
