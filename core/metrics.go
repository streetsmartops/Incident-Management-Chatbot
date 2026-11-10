package core

import "time"

// Metrics receives operational events. The Prometheus implementation lives
// in package metrics; core depends only on this interface.
type Metrics interface {
	IncidentDeclared(sev Severity, auto bool)
	IncidentTransition(from, to State)
	IncidentResolved(sev Severity, ttd, tta, ttm, ttr *time.Duration)
	SignalHandled(source string, outcome SignalOutcome, err error)
	Command(verb string, err error)
	Interaction(action string, err error)
	SideEffectFailed(step string)
	TimelineConflict()
	TriageRun(outcome string, d time.Duration, toolCalls, droppedHypotheses, droppedActions int, inTokens, outTokens int64)
	ActionRequest(actionID string, risk Risk, status ActionStatus)
}

// NopMetrics discards everything.
type NopMetrics struct{}

func (NopMetrics) IncidentDeclared(Severity, bool) {}
func (NopMetrics) IncidentTransition(State, State) {}
func (NopMetrics) IncidentResolved(Severity, *time.Duration, *time.Duration, *time.Duration, *time.Duration) {
}
func (NopMetrics) SignalHandled(string, SignalOutcome, error)                   {}
func (NopMetrics) Command(string, error)                                        {}
func (NopMetrics) Interaction(string, error)                                    {}
func (NopMetrics) SideEffectFailed(string)                                      {}
func (NopMetrics) TimelineConflict()                                            {}
func (NopMetrics) TriageRun(string, time.Duration, int, int, int, int64, int64) {}
func (NopMetrics) ActionRequest(string, Risk, ActionStatus)                     {}

// Durations computes TTD/TTA/TTM/TTR from incident timestamps (impact start
// defaults to detection). Shared by metrics and postmortems.
func Durations(inc *Incident) (ttd, tta, ttm, ttr *time.Duration) {
	d := func(from, to *time.Time) *time.Duration {
		if from == nil || to == nil || to.Before(*from) {
			return nil
		}
		x := to.Sub(*from)
		return &x
	}
	detected := inc.DetectedAt
	impact := &detected
	if inc.ImpactStartedAt != nil {
		impact = inc.ImpactStartedAt
	}
	return d(impact, &detected), d(&detected, inc.AcknowledgedAt), d(impact, inc.MitigatedAt), d(impact, inc.ResolvedAt)
}
