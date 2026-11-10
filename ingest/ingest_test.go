package ingest_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/ingest"
	"github.com/streetsmartops/incident-management-chatbot/providers/pagerduty"
	"github.com/streetsmartops/incident-management-chatbot/test/fakes"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func setup(t *testing.T) (*core.Service, *core.MemStore, *fakes.Slo) {
	store := core.NewMemStore()
	chat := fakes.NewChat("slack")
	slo := &fakes.Slo{
		Slos:    map[string]core.Slo{"7": {ID: "7", Name: "Checkout availability", Service: "checkout", Status: core.SloBreached, Active: true}},
		Tenants: map[string]core.Tenant{"7": {Slug: "acme", Tier: core.TierEnterprise}},
	}
	svc := core.NewService(store, core.Integrations{Chats: map[string]core.ChatPort{"slack": chat}, Slo: slo},
		core.ServiceConfig{DefaultPlatform: "slack", AnnounceChannels: map[string]core.ChannelRef{"slack": {Platform: "slack", ID: "CANN"}}},
		core.DefaultSeverityPolicy(), quiet)
	return svc, store, slo
}

func TestPollerAutoDeclaresOnceAndDoesNotSpam(t *testing.T) {
	svc, store, slo := setup(t)
	ctx := context.Background()
	p := &ingest.Poller{Slo: slo, Store: store, Service: svc, Interval: time.Minute, Log: quiet, Now: time.Now}
	for i := 0; i < 5; i++ {
		if err := p.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	incs, _ := store.ListIncidents(ctx, core.IncidentFilter{})
	if len(incs) != 1 || incs[0].Severity != core.Sev1 {
		t.Fatalf("want 1 SEV1 incident, got %+v", incs)
	}
	evs, _ := store.Events(ctx, incs[0].ID)
	attached := 0
	for _, e := range evs {
		if e.Kind == core.EvSignalAttached {
			attached++
		}
	}
	if attached != 1 {
		t.Fatalf("unchanged status must not re-attach every poll; attached=%d", attached)
	}
	// Recovery is attached once.
	s := slo.Slos["7"]
	s.Status = core.SloOK
	slo.Slos["7"] = s
	_ = p.Poll(ctx)
	_ = p.Poll(ctx)
	evs, _ = store.Events(ctx, incs[0].ID)
	attached = 0
	for _, e := range evs {
		if e.Kind == core.EvSignalAttached {
			attached++
		}
	}
	if attached != 2 {
		t.Fatalf("recovery should attach exactly once; attached=%d", attached)
	}
}

func TestPagerDutyWebhookCorrelatesWithPoller(t *testing.T) {
	svc, store, slo := setup(t)
	ctx := context.Background()
	p := &ingest.Poller{Slo: slo, Store: store, Service: svc, Interval: time.Minute, Log: quiet, Now: time.Now}
	_ = p.Poll(ctx)
	_ = p.Poll(ctx) // auto-declared
	body, _ := os.ReadFile(filepath.Join("..", "test", "fixtures", "pagerduty", "incident-triggered-multiplexer.json"))
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pagerduty", bytes.NewReader(body))
	req.Header.Set("X-PagerDuty-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
	h := &ingest.PagerDutyWebhook{PD: pagerduty.New(pagerduty.Config{WebhookSecret: "whsec"}, nil), Slo: slo, Service: svc, Log: quiet}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d", w.Code)
	}
	incs, _ := store.ListIncidents(ctx, core.IncidentFilter{})
	if len(incs) != 1 {
		t.Fatalf("PD page for the same guardrail must attach, not create; got %d incidents", len(incs))
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/webhooks/pagerduty", bytes.NewReader(body)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook: %d", w.Code)
	}
}
