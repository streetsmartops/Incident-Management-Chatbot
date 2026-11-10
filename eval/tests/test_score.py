import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import score  # noqa: E402


def rec(i, cause, tool="list_events", source="synthetic", ms=1000, needs=False, err=None):
    r = {"id": f"x{i}", "source": source, "durationMs": ms,
         "groundTruth": {"rootCause": "bad deploy", "keywords": [["deploy", "release"]], "relevantTools": ["list_events"], "needsLabel": needs},
         "stats": {"droppedHypotheses": 1, "droppedActions": 0, "inputTokens": 100, "outputTokens": 50}}
    if err:
        r["error"] = err
    else:
        r["result"] = {"hypotheses": [{"cause": cause, "confidence": 0.8, "evidence": [{"tool": tool, "ref": "c1", "finding": "f"}]}]}
    return r


def test_keyword_matching_requires_all_groups():
    assert score.matches("Bad DEPLOY of v2", [["deploy"], ["v2", "v3"]])
    assert not score.matches("bad deploy", [["deploy"], ["v2"]])
    assert not score.matches("anything", [])


def test_metrics_and_not_representative():
    recs = [rec(i, "Bad deploy v2.3.1") for i in range(18)] + [rec(18, "DNS"), rec(19, "x", err="timeout"), rec(20, "deploy", needs=True)]
    m = score.score(recs)
    assert m["incidents"] == 20
    assert abs(m["top1"] - 18 / 20) < 1e-9
    assert m["evidence_precision"] == 1.0
    assert m["errors"] == 1
    assert not m["representative"] and not m["gate_pass"]
    assert "NOT REPRESENTATIVE" in score.report(m)


def test_gate_passes_with_historical():
    recs = [rec(i, "deploy", source="historical" if i < 10 else "synthetic") for i in range(20)]
    m = score.score(recs)
    assert m["representative"] and m["gate_pass"]


def test_evidence_precision_counts_irrelevant_tools():
    recs = [rec(0, "deploy", tool="query_metric"), rec(1, "deploy")]
    assert score.score(recs)["evidence_precision"] == 0.5


def test_fixtures_are_well_formed():
    d = os.path.join(os.path.dirname(__file__), "..", "..", "test", "fixtures", "incidents")
    files = sorted(f for f in os.listdir(d) if f.endswith(".json"))
    assert len(files) >= 20
    for f in files:
        fx = json.load(open(os.path.join(d, f)))
        gt = fx["groundTruth"]
        assert fx["source"] == "synthetic"
        assert gt["keywords"] and gt["relevantTools"]
        assert score.matches(gt["rootCause"], gt["keywords"]), f"{f}: ground truth must satisfy its own keywords"
        for t in gt["relevantTools"]:
            assert t in fx["recordings"], f"{f}: {t} has no recording"
