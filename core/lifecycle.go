package core

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// State is an incident lifecycle state.
type State string

const (
	StateDetected      State = "detected"
	StateDeclared      State = "declared"
	StateInvestigating State = "investigating"
	StateIdentified    State = "identified"
	StateMitigating    State = "mitigating"
	StateMonitoring    State = "monitoring"
	StateResolved      State = "resolved"
	StatePostmortem    State = "postmortem"
	StateClosed        State = "closed"
	StateDismissed     State = "dismissed"
)

// ReopenWindow is how long after resolution an incident may be reopened.
const ReopenWindow = 72 * time.Hour

var transitions = map[State][]State{
	StateDetected:      {StateDeclared, StateDismissed},
	StateDeclared:      {StateInvestigating, StateResolved},
	StateInvestigating: {StateIdentified, StateMitigating, StateResolved},
	StateIdentified:    {StateMitigating, StateResolved},
	StateMitigating:    {StateMonitoring, StateInvestigating},
	StateMonitoring:    {StateResolved, StateInvestigating},
	StateResolved:      {StatePostmortem, StateInvestigating, StateClosed},
	StatePostmortem:    {StateClosed},
	StateClosed:        {},
	StateDismissed:     {},
}

// AllowedTransitions returns the states reachable from s.
func AllowedTransitions(s State) []State { return slices.Clone(transitions[s]) }

// ParseState validates a state name.
func ParseState(s string) (State, bool) {
	st := State(strings.ToLower(strings.TrimSpace(s)))
	_, ok := transitions[st]
	return st, ok
}

// TransitionError explains a rejected transition.
type TransitionError struct {
	From, To State
	Reason   string
	Allowed  []State
}

func (e *TransitionError) Error() string {
	allowed := make([]string, len(e.Allowed))
	for i, s := range e.Allowed {
		allowed[i] = string(s)
	}
	msg := fmt.Sprintf("cannot move %s → %s", e.From, e.To)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg + fmt.Sprintf(" (allowed: %s)", strings.Join(allowed, ", "))
}

// CheckTransition validates moving inc to `to` at time now. It enforces the
// graph plus the business rules in docs/architecture.md §5.1.
func CheckTransition(inc *Incident, to State, now time.Time) error {
	from := inc.State
	allowed := transitions[from]
	if !slices.Contains(allowed, to) {
		return &TransitionError{From: from, To: to, Allowed: allowed}
	}
	if from == StateResolved && to == StateClosed && (inc.Severity == Sev1 || inc.Severity == Sev2) {
		return &TransitionError{From: from, To: to, Allowed: allowed, Reason: "postmortem is mandatory for SEV1/SEV2"}
	}
	if from == StateResolved && to == StateInvestigating {
		if inc.ResolvedAt == nil || now.Sub(*inc.ResolvedAt) > ReopenWindow {
			return &TransitionError{From: from, To: to, Allowed: allowed, Reason: "reopen window (72h) has passed; declare a new linked incident"}
		}
	}
	return nil
}

// ApplyTransition sets the state and the lifecycle timestamps. Call
// CheckTransition first.
func ApplyTransition(inc *Incident, to State, now time.Time) {
	t := now
	switch to {
	case StateDeclared:
		inc.DeclaredAt = &t
	case StateMonitoring:
		if inc.MitigatedAt == nil {
			inc.MitigatedAt = &t
		}
	case StateResolved:
		inc.ResolvedAt = &t
	case StateClosed, StateDismissed:
		inc.ClosedAt = &t
	case StateInvestigating:
		if inc.State == StateResolved {
			inc.ResolvedAt = nil
		}
	}
	inc.State = to
}
