// Package ingest receives signals: PagerDuty webhooks (push) and the
// Multiplexer SLO poller (pull; Multiplexer has no generic outbound webhook,
// gap G5). Both normalise to core.Signal and call Service.HandleSignal.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/providers/multiplexer"
	"github.com/streetsmartops/incident-management-chatbot/providers/pagerduty"
)

// SignalHandler is implemented by core.Service.
type SignalHandler interface {
	HandleSignal(ctx context.Context, sig core.Signal) (core.SignalOutcome, *core.Incident, error)
}

// PagerDutyWebhook serves POST /webhooks/pagerduty.
type PagerDutyWebhook struct {
	PD      *pagerduty.Client
	Slo     core.SloSource // optional, for guardrail-name correlation
	Service SignalHandler
	Log     *slog.Logger
}

func (h *PagerDutyWebhook) resolve(ctx context.Context, name string) (string, bool) {
	if h.Slo == nil {
		return "", false
	}
	slos, err := h.Slo.ListSlos(ctx)
	if err != nil {
		return "", false
	}
	for _, s := range slos {
		if strings.EqualFold(s.Name, name) {
			return s.ID, true
		}
	}
	return "", false
}

func (h *PagerDutyWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sig, err := h.PD.VerifyAndNormalize(r.Context(), r, h.resolve)
	if errors.Is(err, pagerduty.ErrBadSignature) {
		h.Log.Warn("pagerduty webhook rejected", "err", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if sig == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	out, inc, err := h.Service.HandleSignal(r.Context(), *sig)
	if err != nil {
		h.Log.Error("handle pagerduty signal", "err", err, "dedup_key", sig.DedupKey)
		http.Error(w, "processing failed", http.StatusInternalServerError) // PagerDuty retries
		return
	}
	attrs := []any{"outcome", out, "dedup_key", sig.DedupKey}
	if inc != nil {
		attrs = append(attrs, "incident_id", inc.ID, "correlation_id", inc.CorrelationID)
	}
	h.Log.Info("pagerduty signal", attrs...)
	w.WriteHeader(http.StatusAccepted)
}

// Poller polls Multiplexer guardrails.
type Poller struct {
	Slo      core.SloSource
	Store    core.Store
	Service  SignalHandler
	Interval time.Duration
	Log      *slog.Logger
	Now      func() time.Time
	OnPoll   func(error) // optional metrics hook

	mu   sync.Mutex
	last map[string]core.SloStatus
}

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		err := p.Poll(ctx)
		if err != nil {
			p.Log.Warn("multiplexer poll failed", "err", err)
		}
		if p.OnPoll != nil {
			p.OnPoll(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll runs one cycle and returns how many signals were sent.
func (p *Poller) Poll(ctx context.Context) error {
	slos, err := p.Slo.ListSlos(ctx)
	if err != nil {
		return fmt.Errorf("list slos: %w", err)
	}
	p.mu.Lock()
	if p.last == nil {
		p.last = map[string]core.SloStatus{}
	}
	p.mu.Unlock()
	for _, s := range slos {
		if !s.Active {
			continue
		}
		key := multiplexer.DedupKey(s.ID)
		p.mu.Lock()
		prev, seen := p.last[s.ID]
		p.last[s.ID] = s.Status
		p.mu.Unlock()
		changed := !seen || prev != s.Status
		if !changed {
			if s.Status == core.SloOK {
				continue
			}
			// Unchanged non-ok: keep counting toward auto-declare only while
			// no incident is open; otherwise we'd spam the timeline.
			if _, err := p.Store.FindOpenIncidentByDedupKey(ctx, key); err == nil {
				continue
			}
		}
		if !seen && s.Status == core.SloOK {
			continue
		}
		sig := core.Signal{
			Source:     "multiplexer",
			ExternalID: s.ID + ":" + string(s.Status),
			DedupKey:   key,
			Title:      fmt.Sprintf("SLO %s: %s", strings.ToUpper(string(s.Status)), s.Name),
			Service:    s.Service,
			SloID:      s.ID,
			SloStatus:  s.Status,
			Resolved:   s.Status == core.SloOK,
			ObservedAt: p.Now(),
		}
		out, inc, err := p.Service.HandleSignal(ctx, sig)
		if err != nil {
			p.Log.Error("handle slo signal", "slo", s.ID, "err", err)
			continue
		}
		attrs := []any{"slo", s.ID, "status", s.Status, "outcome", out}
		if inc != nil {
			attrs = append(attrs, "incident_id", inc.ID)
		}
		p.Log.Info("slo signal", attrs...)
	}
	return nil
}
