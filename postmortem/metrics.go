// Package postmortem computes incident metrics, retrieves similar incidents
// (BM25 in v1) and drafts blameless postmortems.
package postmortem

import (
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// ComputeMetrics derives TTD/TTA/time-to-declare/TTM/TTR (docs §9).
//
//	TTD             = detectedAt − impactStartedAt
//	TTA             = acknowledgedAt − detectedAt
//	time-to-declare = declaredAt − detectedAt
//	TTM             = mitigatedAt − impactStartedAt
//	TTR             = resolvedAt − impactStartedAt
//
// impactStartedAt defaults to detectedAt when unknown.
func ComputeMetrics(inc *core.Incident) core.IncidentMetrics {
	ttd, tta, ttm, ttr := core.Durations(inc)
	var declare *time.Duration
	if inc.DeclaredAt != nil && !inc.DeclaredAt.Before(inc.DetectedAt) {
		d := inc.DeclaredAt.Sub(inc.DetectedAt)
		declare = &d
	}
	return core.IncidentMetrics{TTD: ttd, TTA: tta, TimeToDeclare: declare, TTM: ttm, TTR: ttr}
}

// FormatDuration renders a duration for humans ("—" if nil).
func FormatDuration(d *time.Duration) string {
	if d == nil {
		return "—"
	}
	return d.Round(time.Second).String()
}
