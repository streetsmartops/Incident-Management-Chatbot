#!/usr/bin/env python3
"""Score a triage evaluation run (JSONL from `incidentd eval run`).

Metrics (docs/architecture.md §7.4):
  top1 / top3 accuracy  - ground-truth keyword groups found in the top / any of top-3 hypotheses
  evidence precision    - cited evidence whose tool is in groundTruth.relevantTools / all cited evidence
  dropped hypothesis %  - hypotheses discarded for invalid citations (model quality signal)
  non-catalog actions   - recommendations dropped because they were not in the catalog
  p95 latency, tokens

Gate (remediation enablement): >= 20 scored incidents, >= 10 historical,
top1 >= 0.60, top3 >= 0.80, evidence precision >= 0.90, p95 <= 90 s.
Reports with fewer than 10 historical incidents are NOT REPRESENTATIVE.

Usage: python3 eval/score.py RUN.jsonl [--report out.md] [--json]
"""
import argparse
import json
import math
import sys

GATE = {"min_incidents": 20, "min_historical": 10, "top1": 0.60, "top3": 0.80, "evidence_precision": 0.90, "p95_seconds": 90.0}


def matches(text, keyword_groups):
    t = (text or "").lower()
    return bool(keyword_groups) and all(any(k.lower() in t for k in group) for group in keyword_groups)


def p95(values):
    if not values:
        return 0.0
    v = sorted(values)
    return v[min(len(v) - 1, math.ceil(0.95 * len(v)) - 1)]


def score(records):
    n = top1 = top3 = 0
    cited = relevant = 0
    kept_h = dropped_h = dropped_a = errors = 0
    historical = 0
    latencies, tokens = [], []
    per = []
    for r in records:
        gt = r.get("groundTruth", {})
        if gt.get("needsLabel"):
            continue
        n += 1
        if r.get("source") == "historical":
            historical += 1
        latencies.append(r.get("durationMs", 0) / 1000.0)
        if r.get("error"):
            errors += 1
            per.append({"id": r["id"], "top1": False, "top3": False, "error": r["error"]})
            continue
        res = r.get("result") or {}
        st = r.get("stats") or {}
        hyps = res.get("hypotheses") or []
        kw = gt.get("keywords") or []
        t1 = bool(hyps) and matches(hyps[0]["cause"], kw)
        t3 = any(matches(h["cause"], kw) for h in hyps[:3])
        top1 += t1
        top3 += t3
        rel = set(gt.get("relevantTools") or [])
        for h in hyps:
            for ev in h.get("evidence") or []:
                cited += 1
                relevant += ev.get("tool") in rel
        kept_h += len(hyps)
        dropped_h += st.get("droppedHypotheses", 0)
        dropped_a += st.get("droppedActions", 0)
        tokens.append(st.get("inputTokens", 0) + st.get("outputTokens", 0))
        per.append({"id": r["id"], "top1": t1, "top3": t3, "top": hyps[0]["cause"] if hyps else None, "truth": gt.get("rootCause")})
    m = {
        "incidents": n,
        "historical": historical,
        "errors": errors,
        "top1": top1 / n if n else 0.0,
        "top3": top3 / n if n else 0.0,
        "evidence_precision": relevant / cited if cited else 0.0,
        "dropped_hypothesis_rate": dropped_h / (kept_h + dropped_h) if (kept_h + dropped_h) else 0.0,
        "non_catalog_actions_dropped": dropped_a,
        "p95_seconds": p95(latencies),
        "avg_tokens": sum(tokens) / len(tokens) if tokens else 0,
        "representative": historical >= GATE["min_historical"],
        "per_incident": per,
    }
    checks = {
        "incidents": n >= GATE["min_incidents"],
        "historical": historical >= GATE["min_historical"],
        "top1": m["top1"] >= GATE["top1"],
        "top3": m["top3"] >= GATE["top3"],
        "evidence_precision": m["evidence_precision"] >= GATE["evidence_precision"],
        "p95": m["p95_seconds"] <= GATE["p95_seconds"],
    }
    m["gate_checks"] = checks
    m["gate_pass"] = all(checks.values())
    return m


def report(m):
    lines = ["# Triage evaluation report", ""]
    if not m["representative"]:
        lines += ["> **NOT REPRESENTATIVE** - fewer than %d historical incidents. Synthetic results cannot open the remediation gate." % GATE["min_historical"], ""]
    lines += [
        "| Metric | Value | Gate |", "|---|---|---|",
        f"| Incidents scored | {m['incidents']} ({m['historical']} historical, {m['errors']} errors) | >= {GATE['min_incidents']} (>= {GATE['min_historical']} historical) |",
        f"| Top-1 accuracy | {m['top1']:.0%} | >= {GATE['top1']:.0%} |",
        f"| Top-3 accuracy | {m['top3']:.0%} | >= {GATE['top3']:.0%} |",
        f"| Evidence precision | {m['evidence_precision']:.0%} | >= {GATE['evidence_precision']:.0%} |",
        f"| Dropped (uncited) hypotheses | {m['dropped_hypothesis_rate']:.0%} | info |",
        f"| Non-catalog actions dropped | {m['non_catalog_actions_dropped']} | info |",
        f"| p95 latency | {m['p95_seconds']:.1f}s | <= {GATE['p95_seconds']:.0f}s |",
        f"| Avg tokens / run | {m['avg_tokens']:.0f} | info |",
        "", f"**Gate: {'PASS' if m['gate_pass'] else 'FAIL'}** ({', '.join(k for k, v in m['gate_checks'].items() if not v) or 'all checks met'})", "",
        "| Incident | Top-1 | Top-3 | Top hypothesis | Ground truth |", "|---|---|---|---|---|",
    ]
    for p in m["per_incident"]:
        lines.append(f"| {p['id']} | {'Y' if p['top1'] else 'N'} | {'Y' if p['top3'] else 'N'} | {p.get('top') or p.get('error', '')} | {p.get('truth', '')} |")
    return "\n".join(lines) + "\n"


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("run")
    ap.add_argument("--report")
    ap.add_argument("--json", action="store_true")
    a = ap.parse_args(argv)
    with open(a.run) as f:
        records = [json.loads(line) for line in f if line.strip()]
    m = score(records)
    if a.json:
        print(json.dumps({k: v for k, v in m.items() if k != "per_incident"}, indent=2))
    else:
        print(report(m))
    if a.report:
        with open(a.report, "w") as f:
            f.write(report(m))
    return 0


if __name__ == "__main__":
    sys.exit(main())
