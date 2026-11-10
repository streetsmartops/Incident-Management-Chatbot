package multiplexer_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/providers/multiplexer"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "multiplexer", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func server(t *testing.T, requireToken bool) *httptest.Server {
	routes := map[string]string{
		"/api/slo-guardrails":          "slo-guardrails.json",
		"/api/slo-guardrails/7":        "slo-guardrail-7.json",
		"/api/slo-guardrails/7/events": "slo-guardrail-7-events.json",
		"/api/tenants":                 "tenants.json",
		"/api/views/12":                "view-12-full.json",
		"/api/views/13":                "view-13-full.json",
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("client must only GET, got %s", r.Method)
		}
		if r.URL.Path == "/api/tenants" || len(r.URL.Path) > 10 && r.URL.Path[:10] == "/api/views" {
			if requireToken && r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Authentication required"}`))
				return
			}
			if r.URL.Path != "/api/tenants" && r.URL.Query().Get("full") != "true" {
				t.Errorf("view fetch must use full=true")
			}
		}
		name, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, name))
	}))
}

func TestListAndGetMapRealShapes(t *testing.T) {
	srv := server(t, false)
	defer srv.Close()
	c, _ := multiplexer.New(srv.URL, "", srv.Client())
	slos, err := c.ListSlos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(slos) != 2 || slos[0].ID != "7" || slos[0].Status != core.SloBreached || slos[0].TargetPercent != 99.9 || slos[0].WindowHours != 720 {
		t.Fatalf("mapping: %+v", slos)
	}
	if slos[1].LastEvaluated != nil {
		t.Fatal("null last_evaluated_at must map to nil")
	}
	slo, err := c.GetSlo(context.Background(), "7")
	if err != nil || slo.Name != "Checkout availability" {
		t.Fatalf("get: %+v %v", slo, err)
	}
	if _, err := c.GetSlo(context.Background(), "999"); err != core.ErrNotFound {
		t.Fatalf("404 should map to ErrNotFound, got %v", err)
	}
}

func TestEventsNewestFirstWithNumbers(t *testing.T) {
	srv := server(t, false)
	defer srv.Close()
	c, _ := multiplexer.New(srv.URL, "", srv.Client())
	evs, err := c.SloEvents(context.Background(), "7", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 || evs[0].NewStatus != core.SloBreached || *evs[0].BurnRate != 5.8 || evs[0].PreviousStatus != core.SloAtRisk {
		t.Fatalf("events: %+v", evs)
	}
}

func TestTenantForAndOwnerWithToken(t *testing.T) {
	srv := server(t, true)
	defer srv.Close()
	c, _ := multiplexer.New(srv.URL, "tok", srv.Client())
	ten, ok, err := c.TenantFor(context.Background(), "7")
	if err != nil || !ok || ten.Slug != "acme" || ten.Tier != core.TierEnterprise {
		t.Fatalf("tenant: %+v ok=%v err=%v", ten, ok, err)
	}
	owner, ok := c.ServiceOwner(context.Background(), "checkout")
	if !ok || owner != "checkout-owner@example.com" {
		t.Fatalf("owner %q %v", owner, ok)
	}
	slo, _ := c.GetSlo(context.Background(), "7")
	if slo.Service != "checkout" {
		t.Fatalf("service name should resolve via view_service, got %q", slo.Service)
	}
}

func TestTenantForWithoutTokenFailsSafe(t *testing.T) {
	srv := server(t, true)
	defer srv.Close()
	c, _ := multiplexer.New(srv.URL, "", srv.Client())
	_, ok, err := c.TenantFor(context.Background(), "7")
	if ok || err == nil {
		t.Fatalf("without a token tenant lookup must fail (=> tier unknown, no auto-declare); ok=%v err=%v", ok, err)
	}
}

func TestBudgetFromEvents(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	base := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	evs := []core.SloEvent{ // newest first
		{At: base.Add(4 * time.Hour), BudgetRemainingPct: f(10)},
		{At: base.Add(2 * time.Hour), BudgetRemainingPct: f(30)},
		{At: base.Add(-time.Hour), BudgetRemainingPct: f(50)},
	}
	burned, q, _ := multiplexer.BudgetFromEvents(evs, base, base.Add(5*time.Hour))
	if burned != 40 || q != "stale" {
		t.Fatalf("burned=%v q=%s", burned, q)
	}
	burned, _, _ = multiplexer.BudgetFromEvents(evs, base, base.Add(3*time.Hour))
	if burned != 20 {
		t.Fatalf("window end should cap at last reading <= to; got %v", burned)
	}
	if _, q, _ := multiplexer.BudgetFromEvents(nil, base, base); q != "unavailable" {
		t.Fatal("no events => unavailable")
	}
}

func TestTenantForUsesEmbeddedTenantWithoutToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/slo-guardrails/7" {
			t.Errorf("embedded tenant must avoid extra calls, got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"guardrail":{"id":7,"name":"Checkout availability","view_service_id":31,"current_status":"breached","is_active":true,"tenant":{"id":1,"slug":"acme","tier":"enterprise"}},"events":[]}`))
	}))
	defer srv.Close()
	c, _ := multiplexer.New(srv.URL, "", srv.Client())
	ten, ok, err := c.TenantFor(context.Background(), "7")
	if err != nil || !ok || ten.Tier != core.TierEnterprise || ten.Slug != "acme" {
		t.Fatalf("%+v %v %v", ten, ok, err)
	}
}
