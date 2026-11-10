package stubs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/providers/stubs"
)

func TestStubsFailLoudly(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	var errs []error
	_, e := stubs.Opsgenie{}.Page(ctx, "x", &core.Incident{}, "high")
	errs = append(errs, e)
	_, e = stubs.ServiceNow{}.CreateIncidentTicket(ctx, &core.Incident{})
	errs = append(errs, e)
	_, e = stubs.Observability{Kind: "prometheus"}.QueryMetric(ctx, "up", now, now)
	errs = append(errs, e)
	_, e = stubs.Cloud{Kind: "aws"}.ListWorkloads(ctx, "a", "b")
	errs = append(errs, e)
	_, e = stubs.Cloud{Kind: "gcp"}.Execute(ctx, core.ActionSpec{})
	errs = append(errs, e)
	_, e = stubs.SloSource{Kind: "nobl9"}.ListSlos(ctx)
	errs = append(errs, e)
	_, e = stubs.LLM{Kind: "bedrock"}.NewConversation("", nil, 0).Send(ctx, "x")
	errs = append(errs, e)
	_, e = stubs.LLM{Kind: "vertex"}.NewConversation("", nil, 0).SendToolResults(ctx, nil)
	errs = append(errs, e)
	for i, err := range errs {
		if !errors.Is(err, core.ErrNotConfigured) {
			t.Errorf("stub %d must fail with ErrNotConfigured, got %v", i, err)
		}
	}
	if len(stubs.Observability{Kind: "grafana"}.Capabilities()) != 0 {
		t.Error("stub observability must expose no capabilities (hides triage tools)")
	}
}
