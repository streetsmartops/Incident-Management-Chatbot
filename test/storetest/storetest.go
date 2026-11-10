// Package storetest is a conformance suite every core.Store must pass.
package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Run executes the suite. newStore must return an empty store.
func Run(t *testing.T, newStore func(t *testing.T) core.Store) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	mk := func(id, key string) *core.Incident {
		return &core.Incident{ID: id, Key: key, Title: "t", Severity: core.Sev2, State: core.StateDeclared,
			Roles: map[core.Role]core.UserRef{core.RoleIC: {Platform: "slack", ID: "U1", Email: "a@example.com"}},
			DedupKeys: []string{"k-" + id}, WarRoom: &core.ChannelRef{Platform: "slack", ID: "C-" + id}, DetectedAt: t0}
	}

	t.Run("incidents", func(t *testing.T) {
		s := newStore(t)
		n1, _ := s.NextIncidentNumber(ctx, 2026)
		n2, _ := s.NextIncidentNumber(ctx, 2026)
		n3, _ := s.NextIncidentNumber(ctx, 2027)
		if n1 != 1 || n2 != 2 || n3 != 1 {
			t.Fatalf("counters %d %d %d", n1, n2, n3)
		}
		inc := mk("i1", "INC-2026-0001")
		if err := s.CreateIncident(ctx, inc); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateIncident(ctx, inc); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("duplicate create: %v", err)
		}
		got, err := s.GetIncident(ctx, "i1")
		if err != nil || got.Roles[core.RoleIC].Email != "a@example.com" || !got.DetectedAt.Equal(t0) {
			t.Fatalf("get: %+v %v", got, err)
		}
		if _, err := s.FindIncidentByKey(ctx, "INC-2026-0001"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FindIncidentByChannel(ctx, core.ChannelRef{Platform: "slack", ID: "C-i1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FindOpenIncidentByDedupKey(ctx, "k-i1"); err != nil {
			t.Fatal(err)
		}
		got.State = core.StateResolved
		got.DedupKeys = append(got.DedupKeys, "extra")
		if err := s.UpdateIncident(ctx, got); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FindOpenIncidentByDedupKey(ctx, "k-i1"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("resolved incidents are not open: %v", err)
		}
		if _, err := s.GetIncident(ctx, "nope"); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if err := s.UpdateIncident(ctx, mk("ghost", "INC-X")); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("update missing: %v", err)
		}
		_ = s.CreateIncident(ctx, mk("i2", "INC-2026-0002"))
		list, _ := s.ListIncidents(ctx, core.IncidentFilter{States: []core.State{core.StateDeclared}})
		if len(list) != 1 || list[0].ID != "i2" {
			t.Fatalf("list: %+v", list)
		}
	})

	t.Run("timeline", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateIncident(ctx, mk("i1", "INC-1"))
		if last, err := s.LastEvent(ctx, "i1"); err != nil || last != nil {
			t.Fatalf("empty last: %v %v", last, err)
		}
		var prev *core.TimelineEvent
		for i := 0; i < 3; i++ {
			ev, _ := core.NewEvent("i1", prev, t0.Add(time.Duration(i)*time.Second), core.Actor{Type: core.ActorHuman, ID: "slack:U1", Email: "a@example.com"}, core.EvNote, map[string]any{"z": i, "a": "unicode ✓", "nested": map[string]int{"b": 2, "a": 1}})
			if err := s.AppendEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
			prev = &ev
		}
		dup, _ := core.NewEvent("i1", &core.TimelineEvent{Seq: 1, Hash: "x"}, t0, core.SystemActor, core.EvNote, nil)
		if err := s.AppendEvent(ctx, dup); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("duplicate seq must conflict: %v", err)
		}
		evs, _ := s.Events(ctx, "i1")
		if len(evs) != 3 {
			t.Fatalf("events %d", len(evs))
		}
		if bad, err := core.VerifyChain(evs); err != nil {
			t.Fatalf("hash chain must survive a store round trip (bad=%d): %v", bad, err)
		}
	})

	t.Run("concurrent appends", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateIncident(ctx, mk("i1", "INC-1"))
		svc := core.NewService(s, core.Integrations{}, core.ServiceConfig{}, core.DefaultSeverityPolicy(), nil)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, err := svc.Record(ctx, "i1", core.SystemActor, core.EvNote, map[string]int{"i": i}); err != nil {
					t.Errorf("record %d: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		evs, _ := s.Events(ctx, "i1")
		if len(evs) != 8 {
			t.Fatalf("lost events: %d", len(evs))
		}
		if _, err := core.VerifyChain(evs); err != nil {
			t.Fatalf("concurrent appends broke the chain: %v", err)
		}
	})

	t.Run("signals, observations, actions, postmortems, flags", func(t *testing.T) {
		s := newStore(t)
		_ = s.CreateIncident(ctx, mk("i1", "INC-1"))
		if err := s.SaveSignal(ctx, core.Signal{ID: "s1", DedupKey: "k", Title: "x", ObservedAt: t0}); err != nil {
			t.Fatal(err)
		}
		if sig, err := s.GetSignal(ctx, "s1"); err != nil || sig.Title != "x" {
			t.Fatalf("signal: %+v %v", sig, err)
		}
		for i, want := range []int{1, 2, 3} {
			n, err := s.RecordObservation(ctx, "k", core.SloBreached)
			if err != nil || n != want {
				t.Fatalf("obs %d: %d %v", i, n, err)
			}
		}
		if n, _ := s.RecordObservation(ctx, "k", core.SloOK); n != 1 {
			t.Fatalf("status change resets streak, got %d", n)
		}
		a := &core.ActionRequest{ID: "a1", IncidentID: "i1", ActionID: "k8s.rollout_restart", Status: core.ActionPending, ExpiresAt: t0}
		_ = s.SaveAction(ctx, a)
		a.Status = core.ActionApproved
		a.Approvals = []core.Approval{{By: core.UserRef{Email: "ic@example.com"}, Role: "ic", At: t0}}
		_ = s.SaveAction(ctx, a)
		if got, err := s.GetAction(ctx, "a1"); err != nil || got.Status != core.ActionApproved || len(got.Approvals) != 1 {
			t.Fatalf("action: %+v %v", got, err)
		}
		if _, err := s.GetPostmortem(ctx, "i1"); !errors.Is(err, core.ErrNotFound) {
			t.Fatal("missing postmortem")
		}
		_ = s.SavePostmortem(ctx, &core.Postmortem{IncidentID: "i1", Status: "draft", Sections: map[string]string{"summary": "s"}})
		if pm, err := s.GetPostmortem(ctx, "i1"); err != nil || pm.Sections["summary"] != "s" {
			t.Fatalf("pm: %+v %v", pm, err)
		}
		if v, _ := s.GetFlag(ctx, "f"); v {
			t.Fatal("flags default false")
		}
		_ = s.SetFlag(ctx, "f", true)
		if v, _ := s.GetFlag(ctx, "f"); !v {
			t.Fatal("flag not set")
		}
	})

}

// RunMultiReplica simulates two replicas (separate Services) appending to the
// same incident concurrently through the same store.
func RunMultiReplica(t *testing.T, s core.Store) {
	ctx := context.Background()
	_ = s.CreateIncident(ctx, &core.Incident{ID: "mr", Key: "INC-MR", State: core.StateDeclared, Roles: map[core.Role]core.UserRef{}, DetectedAt: time.Now()})
	a := core.NewService(s, core.Integrations{}, core.ServiceConfig{}, core.DefaultSeverityPolicy(), nil)
	b := core.NewService(s, core.Integrations{}, core.ServiceConfig{}, core.DefaultSeverityPolicy(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		for _, svc := range []*core.Service{a, b} {
			wg.Add(1)
			go func(svc *core.Service, i int) {
				defer wg.Done()
				if _, err := svc.Record(ctx, "mr", core.SystemActor, core.EvNote, map[string]int{"i": i}); err != nil {
					t.Errorf("record: %v", err)
				}
			}(svc, i)
		}
	}
	wg.Wait()
	evs, _ := s.Events(ctx, "mr")
	if len(evs) != 20 {
		t.Fatalf("lost events across replicas: %d", len(evs))
	}
	if _, err := core.VerifyChain(evs); err != nil {
		t.Fatalf("multi-replica chain: %v", err)
	}
}
