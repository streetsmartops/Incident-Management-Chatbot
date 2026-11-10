// Package stubs holds typed placeholders for planned adapters. Every method
// fails loudly with ErrPlanned so a misconfiguration is obvious at the first
// call rather than silently doing nothing.
package stubs

import (
	"context"
	"fmt"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// ErrPlanned wraps core.ErrNotConfigured with the adapter name.
type ErrPlanned struct{ Adapter string }

func (e *ErrPlanned) Error() string {
	return fmt.Sprintf("%s adapter is planned (v2) and not implemented; see docs/architecture.md §3", e.Adapter)
}

// Unwrap lets errors.Is(err, core.ErrNotConfigured) succeed.
func (e *ErrPlanned) Unwrap() error { return core.ErrNotConfigured }

func planned(name string) error { return &ErrPlanned{Adapter: name} }

// ---------------------------------------------------------------- Paging

// Opsgenie is a planned PagingProvider.
type Opsgenie struct{}

func (Opsgenie) Name() string { return "opsgenie" }
func (Opsgenie) WhoIsOnCall(context.Context, string) ([]core.UserRef, error) {
	return nil, planned("opsgenie")
}
func (Opsgenie) Page(context.Context, string, *core.Incident, string) (string, error) {
	return "", planned("opsgenie")
}
func (Opsgenie) Resolve(context.Context, string) error         { return planned("opsgenie") }
func (Opsgenie) AddNote(context.Context, string, string) error { return planned("opsgenie") }

// ---------------------------------------------------------------- ITSM

// ServiceNow is a planned ItsmProvider.
type ServiceNow struct{}

func (ServiceNow) Name() string { return "servicenow" }
func (ServiceNow) CreateIncidentTicket(context.Context, *core.Incident) (string, error) {
	return "", planned("servicenow")
}
func (ServiceNow) Comment(context.Context, string, string) error { return planned("servicenow") }
func (ServiceNow) CreateActionItem(context.Context, *core.Incident, core.ActionItem) (string, error) {
	return "", planned("servicenow")
}

// ---------------------------------------------------------------- Observability

// Observability is a planned ObservabilityProvider (Prometheus/Alertmanager, Grafana).
type Observability struct{ Kind string }

func (o Observability) Name() string                          { return o.Kind }
func (Observability) Capabilities() map[core.Capability]bool  { return map[core.Capability]bool{} }
func (o Observability) QueryMetric(context.Context, string, time.Time, time.Time) ([]core.Series, error) {
	return nil, planned(o.Kind)
}
func (o Observability) SearchLogs(context.Context, string, time.Time, time.Time, int) ([]core.LogLine, error) {
	return nil, planned(o.Kind)
}
func (o Observability) MonitorsInState(context.Context, []string, []string) ([]core.Monitor, error) {
	return nil, planned(o.Kind)
}
func (o Observability) Events(context.Context, string, time.Time, time.Time) ([]core.ObsEvent, error) {
	return nil, planned(o.Kind)
}

// ---------------------------------------------------------------- Cloud

// Cloud is a planned CloudProvider (aws, gcp, azure).
type Cloud struct{ Kind string }

func (c Cloud) Name() string     { return c.Kind }
func (Cloud) Scopes() []string   { return nil }
func (c Cloud) ListWorkloads(context.Context, string, string) ([]core.Workload, error) {
	return nil, planned(c.Kind)
}
func (c Cloud) WorkloadEvents(context.Context, core.WorkloadRef, time.Time) ([]core.ObsEvent, error) {
	return nil, planned(c.Kind)
}
func (c Cloud) RolloutHistory(context.Context, core.WorkloadRef) ([]core.Revision, error) {
	return nil, planned(c.Kind)
}
func (c Cloud) PodLogs(context.Context, core.WorkloadRef, time.Time, int) ([]core.LogLine, error) {
	return nil, planned(c.Kind)
}
func (c Cloud) DryRun(context.Context, core.ActionSpec) (string, error)  { return "", planned(c.Kind) }
func (c Cloud) Execute(context.Context, core.ActionSpec) (string, error) { return "", planned(c.Kind) }
func (c Cloud) Verify(context.Context, core.ActionSpec) (string, error)  { return "", planned(c.Kind) }

// ---------------------------------------------------------------- SLO

// SloSource is a planned SloSource (datadog-slo, nobl9).
type SloSource struct{ Kind string }

func (s SloSource) Name() string { return s.Kind }
func (s SloSource) ListSlos(context.Context) ([]core.Slo, error) { return nil, planned(s.Kind) }
func (s SloSource) GetSlo(context.Context, string) (core.Slo, error) {
	return core.Slo{}, planned(s.Kind)
}
func (s SloSource) SloEvents(context.Context, string, int) ([]core.SloEvent, error) {
	return nil, planned(s.Kind)
}
func (s SloSource) TenantFor(context.Context, string) (core.Tenant, bool, error) {
	return core.Tenant{}, false, planned(s.Kind)
}
func (s SloSource) BudgetBurned(context.Context, string, time.Time, time.Time) (float64, string, error) {
	return 0, "unavailable", planned(s.Kind)
}

// ---------------------------------------------------------------- LLM

// LLM is a planned LlmPort (bedrock, vertex). Both must be reached through
// private endpoints (AWS PrivateLink / GCP Private Service Connect) when
// implemented; see docs/architecture.md §10.
type LLM struct{ Kind string }

func (l LLM) Name() string { return l.Kind }
func (l LLM) NewConversation(string, []core.LlmTool, int) core.LlmConversation {
	return failingConv{l.Kind}
}

type failingConv struct{ kind string }

func (f failingConv) Send(context.Context, string) (core.LlmTurn, error) {
	return core.LlmTurn{}, planned(f.kind)
}
func (f failingConv) SendToolResults(context.Context, []core.LlmToolResult) (core.LlmTurn, error) {
	return core.LlmTurn{}, planned(f.kind)
}

var (
	_ core.PagingProvider        = Opsgenie{}
	_ core.ItsmProvider          = ServiceNow{}
	_ core.ObservabilityProvider = Observability{}
	_ core.CloudProvider         = Cloud{}
	_ core.SloSource             = SloSource{}
	_ core.LlmPort               = LLM{}
)
