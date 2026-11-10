// Package postmortem computes incident metrics, retrieves similar incidents
// (BM25 in v1) and drafts blameless postmortems.
package postmortem

import (
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

func dur(from, to *time.Time) *time.Duration {
	if from == nil || to == nil || to.Before(*from) {
		return nil
	}
	d := to.Sub(*from)
	return &d
}

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
	detected := inc.DetectedAt
	impact := &detected
	if inc.ImpactStartedAt != nil {
		impact = inc.ImpactStartedAt
	}
	return core.IncidentMetrics{
		TTD:           dur(impact, &detected),
		TTA:           dur(&detected, inc.AcknowledgedAt),
		TimeToDeclare: dur(&detected, inc.DeclaredAt),
		TTM:           dur(impact, inc.MitigatedAt),
		TTR:           dur(impact, inc.ResolvedAt),
	}
}

// FormatDuration renders a duration for humans ("—" if nil).
func FormatDuration(d *time.Duration) string {
	if d == nil {
		return "—"
	}
	return d.Round(time.Second).String()
}
