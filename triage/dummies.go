package triage

import (
	"context"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Dummies exist only so ReplayExecutor can reuse LiveExecutor's tool schemas.
type (
	dummySlo     struct{ core.SloSource }
	dummyObs     struct{ core.ObservabilityProvider }
	dummyCloud   struct{ core.CloudProvider }
	dummyPaging  struct{ core.PagingProvider }
	dummySimilar struct{}
)

func (dummyObs) Capabilities() map[core.Capability]bool {
	return map[core.Capability]bool{core.CapMetrics: true, core.CapLogs: true, core.CapMonitors: true, core.CapEvents: true}
}
func (dummyCloud) Scopes() []string { return []string{"(recorded)"} }
func (dummySimilar) Search(context.Context, string, int) ([]core.SimilarIncident, error) {
	return nil, nil
}
