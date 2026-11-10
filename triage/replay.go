package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// ReplayRule returns Output when the tool input contains Match ("" matches all).
type ReplayRule struct {
	Match  string `json:"match"`
	Output string `json:"output"`
}

// ReplayExecutor serves recorded tool outputs (eval harness and tests). It
// offers the same tool names and schemas as LiveExecutor for the tools that
// have recordings.
type ReplayExecutor struct {
	Recordings map[string][]ReplayRule
	Context    string // output for get_incident_context
	Redactor   *core.Redactor
}

// Tools lists get_incident_context plus every recorded tool, using the live
// schemas so the model sees exactly what production would offer.
func (r *ReplayExecutor) Tools(inc *core.Incident) []core.LlmTool {
	live := (&LiveExecutor{
		Slo: dummySlo{}, Obs: dummyObs{}, Cloud: dummyCloud{}, Paging: dummyPaging{}, Similar: dummySimilar{},
	}).Tools(inc)
	var out []core.LlmTool
	for _, t := range live {
		if t.Name == "get_incident_context" || len(r.Recordings[t.Name]) > 0 {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Call returns the first matching recording.
func (r *ReplayExecutor) Call(_ context.Context, _ *core.Incident, name string, input json.RawMessage) (string, error) {
	if name == "get_incident_context" {
		return r.redact(r.Context), nil
	}
	rules, ok := r.Recordings[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	in := strings.ToLower(string(input))
	for _, rule := range rules {
		if rule.Match == "" || strings.Contains(in, strings.ToLower(rule.Match)) {
			return r.redact(rule.Output), nil
		}
	}
	return "[]", nil
}

func (r *ReplayExecutor) redact(s string) string {
	if r.Redactor == nil {
		return s
	}
	return r.Redactor.Redact(s)
}
