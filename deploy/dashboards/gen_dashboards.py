#!/usr/bin/env python3
"""Generate the incidentd dashboards and alert rules.

Outputs (committed):
  deploy/dashboards/grafana/incidentd.json   Grafana dashboard (Prometheus datasource)
  deploy/dashboards/datadog/incidentd.json   Datadog dashboard (OpenMetrics check)
  deploy/monitoring/prometheus-rules.yaml    Prometheus alerting rules

A Go test (metrics/dashboards_test.go) asserts every metric referenced here is
exported by incidentd.

Usage: python3 deploy/dashboards/gen_dashboards.py
"""
import json
import os

ROOT = os.path.join(os.path.dirname(__file__), "..", "..")

# --------------------------------------------------------------------- Grafana

_id = 0


def nid():
    global _id
    _id += 1
    return _id


def target(expr, legend=""):
    return {"datasource": {"type": "prometheus", "uid": "${datasource}"}, "expr": expr, "legendFormat": legend, "refId": chr(64 + nid() % 26 or 1)}


def panel(kind, title, exprs, x, y, w=6, h=6, unit=None, desc=""):
    p = {
        "id": nid(), "type": kind, "title": title, "description": desc,
        "gridPos": {"x": x, "y": y, "w": w, "h": h},
        "datasource": {"type": "prometheus", "uid": "${datasource}"},
        "targets": [target(e, l) for e, l in exprs],
        "fieldConfig": {"defaults": {}, "overrides": []},
        "options": {},
    }
    if unit:
        p["fieldConfig"]["defaults"]["unit"] = unit
    if kind == "stat":
        p["options"] = {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False}, "colorMode": "value", "graphMode": "none"}
    return p


def row(title, y):
    return {"id": nid(), "type": "row", "title": title, "collapsed": False, "gridPos": {"x": 0, "y": y, "w": 24, "h": 1}, "panels": []}


def q(metric, by=None, fn="sum", rng="$__rate_interval", kind="rate"):
    b = f" by ({by})" if by else ""
    return f"{fn}{b}({kind}({metric}[{rng}]))"


def quantile(qv, metric, match, by="le"):
    return f'histogram_quantile({qv}, sum by ({by}) (rate({metric}_bucket{{{match}}}[$__range])))'


def grafana():
    P = []
    y = 0
    P.append(row("Overview", y)); y += 1
    P += [
        panel("stat", "Open incidents", [('sum(incidentd_incidents_open) or vector(0)', "")], 0, y, 4, 4, desc="Incidents in declared..monitoring."),
        panel("stat", "Open SEV1/SEV2", [('sum(incidentd_incidents_open{severity=~"SEV1|SEV2"}) or vector(0)', "")], 4, y, 4, 4),
        panel("stat", "Declared (range)", [('sum(increase(incidentd_incidents_declared_total[$__range]))', "")], 8, y, 4, 4),
        panel("stat", "Auto-declared (range)", [('sum(increase(incidentd_incidents_declared_total{auto="true"}[$__range]))', "")], 12, y, 4, 4),
        panel("stat", "Remediation executor", [("max(incidentd_remediation_executor_enabled)", "")], 16, y, 4, 4,
              desc="1 = both kill-switch keys allow execution. Expected 0 until the triage eval gate passes."),
        panel("stat", "Version", [("max by (version) (incidentd_build_info)", "{{version}}")], 20, y, 4, 4),
    ]
    y += 4
    P.append(row("Incident response", y)); y += 1
    P += [
        panel("timeseries", "Open incidents by severity", [("sum by (severity) (incidentd_incidents_open)", "{{severity}}")], 0, y, 8, 7),
        panel("timeseries", "Declarations by severity", [(q("incidentd_incidents_declared_total", "severity", kind="increase", rng="1h"), "{{severity}}")], 8, y, 8, 7, desc="Per hour."),
        panel("timeseries", "Signals by outcome", [(q("incidentd_signals_total", "source, outcome"), "{{source}} {{outcome}}")], 16, y, 8, 7, unit="ops"),
    ]
    y += 7
    P += [
        panel("stat", f"{m.upper()} p50 (range)", [(quantile(0.5, "incidentd_incident_duration_seconds", f'metric="{m}"'), "")], i * 6, y, 6, 4, unit="s")
        for i, m in enumerate(["ttd", "tta", "ttm", "ttr"])
    ]
    y += 4
    P += [
        panel("bargauge", "TTR p90 by severity (range)", [(quantile(0.9, "incidentd_incident_duration_seconds", 'metric="ttr"', "le, severity"), "{{severity}}")], 0, y, 12, 6, unit="s"),
        panel("timeseries", "Transitions", [(q("incidentd_incident_transitions_total", "to", kind="increase", rng="1h"), "→ {{to}}")], 12, y, 12, 6),
    ]
    y += 6
    P.append(row("Bot health", y)); y += 1
    P += [
        panel("timeseries", "Commands by result", [(q("incidentd_commands_total", "result"), "{{result}}")], 0, y, 8, 7, unit="ops"),
        panel("timeseries", "Commands by verb", [(q("incidentd_commands_total", "verb"), "{{verb}}")], 8, y, 8, 7, unit="ops"),
        panel("timeseries", "Side-effect failures by step", [(q("incidentd_side_effect_failures_total", "step"), "{{step}}")], 16, y, 8, 7, unit="ops",
              desc="Paging, ticket, war room, invite failures. Recorded in the incident timeline too."),
    ]
    y += 7
    P += [
        panel("timeseries", "HTTP p95 by route", [('histogram_quantile(0.95, sum by (le, route) (rate(incidentd_http_request_duration_seconds_bucket[$__rate_interval])))', "{{route}}")], 0, y, 8, 7, unit="s",
              desc="Time to ack. Slack requires < 3s."),
        panel("timeseries", "HTTP 4xx/5xx", [('sum by (route, code) (rate(incidentd_http_requests_total{code=~"4..|5.."}[$__rate_interval]))', "{{route}} {{code}}")], 8, y, 8, 7, unit="ops",
              desc="401s on chat/webhook routes mean signature failures (misconfigured secret or spoofed requests)."),
        panel("timeseries", "SLO polls and timeline conflicts", [(q("incidentd_slo_polls_total", "result"), "poll {{result}}"), (q("incidentd_timeline_conflicts_total"), "timeline conflicts")], 16, y, 8, 7, unit="ops"),
    ]
    y += 7
    P.append(row("AI triage", y)); y += 1
    P += [
        panel("timeseries", "Triage runs by outcome", [(q("incidentd_triage_runs_total", "outcome", kind="increase", rng="1h"), "{{outcome}}")], 0, y, 6, 7),
        panel("timeseries", "Triage duration p50/p95", [("histogram_quantile(0.5, sum by (le) (rate(incidentd_triage_duration_seconds_bucket[$__rate_interval])))", "p50"),
                                                          ("histogram_quantile(0.95, sum by (le) (rate(incidentd_triage_duration_seconds_bucket[$__rate_interval])))", "p95")], 6, y, 6, 7, unit="s"),
        panel("timeseries", "Guardrail drops", [(q("incidentd_triage_dropped_total", "kind", kind="increase", rng="1h"), "{{kind}}")], 12, y, 6, 7,
              desc="Uncited hypotheses and non-catalog actions discarded in code."),
        panel("timeseries", "LLM tokens per hour", [(q("incidentd_llm_tokens_total", "direction", kind="increase", rng="1h"), "{{direction}}")], 18, y, 6, 7),
    ]
    y += 7
    P.append(row("Remediation", y)); y += 1
    P += [
        panel("timeseries", "Action requests by status", [(q("incidentd_action_requests_total", "status", kind="increase", rng="1h"), "{{status}}")], 0, y, 12, 7),
        panel("bargauge", "Action requests by action and risk (range)", [('sum by (action, risk) (increase(incidentd_action_requests_total{status="pending"}[$__range]))', "{{action}} ({{risk}})")], 12, y, 12, 7),
    ]
    return {
        "title": "incidentd: Incident Management Bot",
        "uid": "incidentd-overview",
        "tags": ["incidentd", "sre", "incidents"],
        "timezone": "browser",
        "schemaVersion": 39,
        "version": 1,
        "refresh": "1m",
        "time": {"from": "now-7d", "to": "now"},
        "templating": {"list": [{"name": "datasource", "type": "datasource", "query": "prometheus", "label": "Prometheus", "current": {}}]},
        "annotations": {"list": []},
        "panels": P,
    }


# --------------------------------------------------------------------- Datadog
# Datadog names come from the explicit mapping in deploy/k8s/deployment.yaml
# (OpenMetrics check, namespace "incidentd"). Counters are queried with the
# ".count" suffix and as_count(); confirm the suffix in Metrics Explorer after
# the first scrape (see docs/dashboards.md, UNVERIFIED).

DD_MAP = {
    "incidentd_incidents_declared": "incidents_declared",
    "incidentd_incident_transitions": "incident_transitions",
    "incidentd_incident_duration_seconds": "incident_duration_seconds",
    "incidentd_incidents_open": "incidents_open",
    "incidentd_signals": "signals",
    "incidentd_commands": "commands",
    "incidentd_interactions": "interactions",
    "incidentd_side_effect_failures": "side_effect_failures",
    "incidentd_timeline_conflicts": "timeline_conflicts",
    "incidentd_triage_runs": "triage_runs",
    "incidentd_triage_duration_seconds": "triage_duration_seconds",
    "incidentd_triage_tool_calls": "triage_tool_calls",
    "incidentd_triage_dropped": "triage_dropped",
    "incidentd_llm_tokens": "llm_tokens",
    "incidentd_action_requests": "action_requests",
    "incidentd_remediation_executor_enabled": "remediation_executor_enabled",
    "incidentd_http_requests": "http_requests",
    "incidentd_http_request_duration_seconds": "http_request_duration_seconds",
    "incidentd_slo_polls": "slo_polls",
    "incidentd_build_info": "build_info",
}


def dd_w(title, q, kind="timeseries", display="line", layout=(0, 0, 4, 2)):
    x, y, w, h = layout
    if kind == "query_value":
        d = {"type": "query_value", "title": title, "requests": [{"q": q, "aggregator": "last"}], "autoscale": True, "precision": 0}
    elif kind == "toplist":
        d = {"type": "toplist", "title": title, "requests": [{"q": q}]}
    else:
        d = {"type": "timeseries", "title": title, "requests": [{"q": q, "display_type": display}]}
    return {"definition": d, "layout": {"x": x, "y": y, "width": w, "height": h}}


def datadog():
    W = []
    W += [
        dd_w("Open incidents", "sum:incidentd.incidents_open{*}", "query_value", layout=(0, 0, 2, 2)),
        dd_w("Open SEV1/SEV2", "sum:incidentd.incidents_open{severity:sev1 OR severity:sev2}", "query_value", layout=(2, 0, 2, 2)),
        dd_w("Declared (window)", "sum:incidentd.incidents_declared.count{*}.as_count()", "query_value", layout=(4, 0, 2, 2)),
        dd_w("Remediation executor", "max:incidentd.remediation_executor_enabled{*}", "query_value", layout=(6, 0, 2, 2)),
        dd_w("Open incidents by severity", "sum:incidentd.incidents_open{*} by {severity}", layout=(0, 2, 4, 2)),
        dd_w("Declarations by severity", "sum:incidentd.incidents_declared.count{*} by {severity}.as_count()", "timeseries", "bars", layout=(4, 2, 4, 2)),
        dd_w("Signals by outcome", "sum:incidentd.signals.count{*} by {source,outcome}.as_count()", "timeseries", "bars", layout=(8, 2, 4, 2)),
        dd_w("Commands by result", "sum:incidentd.commands.count{*} by {result}.as_count()", "timeseries", "bars", layout=(0, 4, 4, 2)),
        dd_w("Side-effect failures by step", "sum:incidentd.side_effect_failures.count{*} by {step}.as_count()", "timeseries", "bars", layout=(4, 4, 4, 2)),
        dd_w("HTTP errors by route", "sum:incidentd.http_requests.count{code:4* OR code:5*} by {route,code}.as_count()", "timeseries", "bars", layout=(8, 4, 4, 2)),
        dd_w("Triage runs by outcome", "sum:incidentd.triage_runs.count{*} by {outcome}.as_count()", "timeseries", "bars", layout=(0, 6, 4, 2)),
        dd_w("Guardrail drops", "sum:incidentd.triage_dropped.count{*} by {kind}.as_count()", "timeseries", "bars", layout=(4, 6, 4, 2)),
        dd_w("LLM tokens", "sum:incidentd.llm_tokens.count{*} by {direction}.as_count()", "timeseries", "bars", layout=(8, 6, 4, 2)),
        dd_w("Action requests by status", "sum:incidentd.action_requests.count{*} by {status}.as_count()", "timeseries", "bars", layout=(0, 8, 6, 2)),
        dd_w("SLO poll results", "sum:incidentd.slo_polls.count{*} by {result}.as_count()", "timeseries", "bars", layout=(6, 8, 6, 2)),
    ]
    return {
        "title": "incidentd: Incident Management Bot",
        "description": "Generated by deploy/dashboards/gen_dashboards.py. Requires the OpenMetrics annotation in deploy/k8s/deployment.yaml.",
        "layout_type": "ordered",
        "widgets": W,
        "template_variables": [],
        "notify_list": [],
        "reflow_type": "fixed",
    }


# --------------------------------------------------------------------- Alerts

RULES = """# Prometheus alerting rules for incidentd. Generated by deploy/dashboards/gen_dashboards.py.
groups:
  - name: incidentd
    rules:
      - alert: IncidentdDown
        expr: up{job="incidentd"} == 0
        for: 5m
        labels: {severity: page}
        annotations:
          summary: incidentd is down - chat commands, paging and SLO ingest are unavailable
          runbook: docs/operations.md#incidentd-down
      - alert: IncidentdSloPollFailing
        expr: sum(rate(incidentd_slo_polls_total{result="error"}[10m])) > 0 and sum(rate(incidentd_slo_polls_total{result="ok"}[10m])) == 0
        for: 10m
        labels: {severity: page}
        annotations:
          summary: Multiplexer polling has failed for 10 minutes - SLO breaches will not auto-declare
          runbook: docs/operations.md#slo-polling-failing
      - alert: IncidentdSideEffectsFailing
        expr: sum by (step) (increase(incidentd_side_effect_failures_total[15m])) > 3
        labels: {severity: ticket}
        annotations:
          summary: 'incidentd side effect "{{ $labels.step }}" failing (paging/ticket/war room)'
          runbook: docs/operations.md#side-effect-failures
      - alert: IncidentdSignatureFailures
        expr: sum by (route) (increase(incidentd_http_requests_total{code="401",route=~".*(slack|teams|webhooks).*"}[15m])) > 10
        labels: {severity: ticket}
        annotations:
          summary: 'Many rejected requests on {{ $labels.route }} - check signing secrets or possible spoofing'
          runbook: docs/operations.md#signature-failures
      - alert: IncidentdTriageErrors
        expr: sum(increase(incidentd_triage_runs_total{outcome="error"}[1h])) > 3
        labels: {severity: ticket}
        annotations:
          summary: AI triage failing repeatedly (LLM API, budget or tool errors)
          runbook: docs/operations.md#triage-errors
      - alert: IncidentdSlowAck
        expr: histogram_quantile(0.95, sum by (le, route) (rate(incidentd_http_request_duration_seconds_bucket{route=~".*slack.*"}[10m]))) > 2
        for: 10m
        labels: {severity: ticket}
        annotations:
          summary: Slack acks are close to the 3 second limit
          runbook: docs/operations.md#slow-acks
      - alert: IncidentdExecutorEnabled
        expr: max(incidentd_remediation_executor_enabled) == 1
        labels: {severity: info}
        annotations:
          summary: Remediation execution is ENABLED (both kill-switch keys on) - confirm this is intended
          runbook: docs/operations.md#kill-switch
"""


def write(path, data):
    full = os.path.join(ROOT, path)
    os.makedirs(os.path.dirname(full), exist_ok=True)
    with open(full, "w") as f:
        if isinstance(data, str):
            f.write(data)
        else:
            json.dump(data, f, indent=2, sort_keys=True)
            f.write("\n")
    print("wrote", path)


if __name__ == "__main__":
    write("deploy/dashboards/grafana/incidentd.json", grafana())
    write("deploy/dashboards/datadog/incidentd.json", datadog())
    write("deploy/dashboards/datadog/openmetrics-mapping.json", DD_MAP)
    write("deploy/monitoring/prometheus-rules.yaml", RULES)
