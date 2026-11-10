// Package multiplexer implements core.SloSource against the Multiplexer
// SLO portal (streetsmartops/Multi-slo-observability-app).
//
// Endpoints and shapes are taken from Multiplexer's source (main@e14d5ca):
//
//	GET /api/slo-guardrails                 -> SloGuardrail[]            (no auth today; gap G1)
//	GET /api/slo-guardrails/:id             -> {guardrail, events}
//	GET /api/slo-guardrails/:id/events?limit=N (N<=500) -> {guardrail_id, events}
//	GET /api/tenants                        -> TenantWithConfigs[] incl. view_ids (needs session or token; gap G2)
//	GET /api/views/:id?full=true            -> {view_services:[{id, service:{name, owner}}]}
package multiplexer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// DedupKey is the dedup key for a guardrail, shared by the poller and the
// PagerDuty webhook normaliser so one breach yields one incident.
func DedupKey(guardrailID string) string { return "multiplexer:guardrail:" + guardrailID }

// PagerDutyDedupKey is the dedup_key Multiplexer sends to PagerDuty
// (src/lib/server/notifications/index.ts: `slo-guardrail-${id}`).
func PagerDutyDedupKey(guardrailID string) string { return "slo-guardrail-" + guardrailID }

// PagerDutyTitlePrefix is the alert summary prefix Multiplexer uses
// (`SLO Breach: ${guardrail.name}`).
const PagerDutyTitlePrefix = "SLO Breach: "

// Client talks to Multiplexer.
type Client struct {
	base     *url.URL
	token    string
	http     *http.Client
	indexTTL time.Duration
	now      func() time.Time

	mu        sync.Mutex
	index     map[int64]vsInfo // view_service_id -> info
	indexedAt time.Time
}

type vsInfo struct {
	tenant  core.Tenant
	hasTen  bool
	service string
	owner   string
}

// New returns a client. token may be empty until Multiplexer gap G2 (service
// tokens) is merged; tenant lookups then fail and severity uses tier=unknown.
func New(baseURL, token string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("multiplexer base url: %w", err)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{base: u, token: token, http: hc, indexTTL: 5 * time.Minute, now: time.Now}, nil
}

func (c *Client) Name() string { return "multiplexer" }

// HTTPError is a non-2xx response.
type HTTPError struct {
	Status int
	Path   string
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("multiplexer %s: HTTP %d: %s", e.Path, e.Status, e.Body)
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("multiplexer %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusNotFound {
			return core.ErrNotFound
		}
		b := string(body)
		if len(b) > 200 {
			b = b[:200]
		}
		return &HTTPError{Status: resp.StatusCode, Path: path, Body: b}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("multiplexer %s: decode: %w", path, err)
	}
	return nil
}

// Wire types (subset of Multiplexer's SloGuardrail / SloEvent / Tenant). We
// deliberately do not decode pagerduty_service_key.
type guardrail struct {
	ID                    int64      `json:"id"`
	Name                  string     `json:"name"`
	ViewServiceID         *int64     `json:"view_service_id"`
	ComponentID           *int64     `json:"component_id"`
	TargetUptimePercent   float64    `json:"target_uptime_percent"`
	EvaluationWindowHours int        `json:"evaluation_window_hours"`
	IsActive              bool       `json:"is_active"`
	CurrentStatus         string     `json:"current_status"`
	LastEvaluatedAt       *time.Time `json:"last_evaluated_at"`
}

type sloEvent struct {
	ID                          int64      `json:"id"`
	EventType                   string     `json:"event_type"`
	PreviousStatus              *string    `json:"previous_status"`
	NewStatus                   string     `json:"new_status"`
	UptimePercent               *float64   `json:"uptime_percent"`
	BurnRate                    *float64   `json:"burn_rate"`
	ErrorBudgetRemainingPercent *float64   `json:"error_budget_remaining_percent"`
	Message                     string     `json:"message"`
	CreatedAt                   time.Time  `json:"created_at"`
}

type tenant struct {
	ID      int64   `json:"id"`
	Slug    string  `json:"slug"`
	Name    string  `json:"name"`
	Tier    string  `json:"tier"`
	ViewIDs []int64 `json:"view_ids"`
}

type viewFull struct {
	ViewServices []struct {
		ID      int64 `json:"id"`
		Service struct {
			Name  string `json:"name"`
			Owner string `json:"owner"`
		} `json:"service"`
	} `json:"view_services"`
}

func parseStatus(s string) core.SloStatus {
	switch core.SloStatus(s) {
	case core.SloOK, core.SloAtRisk, core.SloBreached, core.SloNoData, core.SloTimeout:
		return core.SloStatus(s)
	}
	return core.SloNoData
}

func parseTier(s string) core.TenantTier {
	switch core.TenantTier(s) {
	case core.TierEnterprise, core.TierBusiness, core.TierStarter:
		return core.TenantTier(s)
	}
	return core.TierUnknown
}

func (c *Client) toSlo(ctx context.Context, g guardrail) core.Slo {
	s := core.Slo{
		ID:            strconv.FormatInt(g.ID, 10),
		Name:          g.Name,
		Service:       g.Name,
		TargetPercent: g.TargetUptimePercent,
		WindowHours:   g.EvaluationWindowHours,
		Status:        parseStatus(g.CurrentStatus),
		LastEvaluated: g.LastEvaluatedAt,
		Active:        g.IsActive,
		ViewServiceID: g.ViewServiceID,
	}
	if g.ViewServiceID != nil {
		if info, ok := c.lookupViewService(ctx, *g.ViewServiceID); ok && info.service != "" {
			s.Service = info.service
		}
	}
	return s
}

// ListSlos returns all guardrails.
func (c *Client) ListSlos(ctx context.Context) ([]core.Slo, error) {
	var gs []guardrail
	if err := c.get(ctx, "/api/slo-guardrails", nil, &gs); err != nil {
		return nil, err
	}
	out := make([]core.Slo, 0, len(gs))
	for _, g := range gs {
		out = append(out, c.toSlo(ctx, g))
	}
	return out, nil
}

// GetSlo returns one guardrail.
func (c *Client) GetSlo(ctx context.Context, id string) (core.Slo, error) {
	var resp struct {
		Guardrail guardrail `json:"guardrail"`
	}
	if err := c.get(ctx, "/api/slo-guardrails/"+url.PathEscape(id), nil, &resp); err != nil {
		return core.Slo{}, err
	}
	return c.toSlo(ctx, resp.Guardrail), nil
}

// SloEvents returns events newest first.
func (c *Client) SloEvents(ctx context.Context, id string, limit int) ([]core.SloEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 500 // Multiplexer caps at 500
	}
	var resp struct {
		Events []sloEvent `json:"events"`
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if err := c.get(ctx, "/api/slo-guardrails/"+url.PathEscape(id)+"/events", q, &resp); err != nil {
		return nil, err
	}
	out := make([]core.SloEvent, 0, len(resp.Events))
	for _, e := range resp.Events {
		ev := core.SloEvent{
			At: e.CreatedAt, NewStatus: parseStatus(e.NewStatus), UptimePercent: e.UptimePercent,
			BurnRate: e.BurnRate, BudgetRemainingPct: e.ErrorBudgetRemainingPercent, Message: e.Message,
		}
		if e.PreviousStatus != nil {
			ev.PreviousStatus = parseStatus(*e.PreviousStatus)
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

// TenantFor resolves guardrail -> view_service -> view -> tenant.
func (c *Client) TenantFor(ctx context.Context, sloID string) (core.Tenant, bool, error) {
	slo, err := c.GetSlo(ctx, sloID)
	if err != nil {
		return core.Tenant{}, false, err
	}
	if slo.ViewServiceID == nil {
		return core.Tenant{}, false, nil
	}
	if err := c.refreshIndex(ctx); err != nil {
		return core.Tenant{}, false, err
	}
	info, ok := c.lookupViewService(ctx, *slo.ViewServiceID)
	if !ok || !info.hasTen {
		return core.Tenant{}, false, nil
	}
	return info.tenant, true, nil
}

// ServiceOwner returns the Multiplexer services.owner for a service name.
func (c *Client) ServiceOwner(ctx context.Context, service string) (string, bool) {
	if err := c.refreshIndex(ctx); err != nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, info := range c.index {
		if strings.EqualFold(info.service, service) && info.owner != "" {
			return info.owner, true
		}
	}
	return "", false
}

func (c *Client) lookupViewService(_ context.Context, id int64) (vsInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, ok := c.index[id]
	return info, ok
}

func (c *Client) refreshIndex(ctx context.Context) error {
	c.mu.Lock()
	fresh := c.index != nil && c.now().Sub(c.indexedAt) < c.indexTTL
	c.mu.Unlock()
	if fresh {
		return nil
	}
	var tenants []tenant
	if err := c.get(ctx, "/api/tenants", nil, &tenants); err != nil {
		return fmt.Errorf("tenant index (needs MULTIPLEXER_TOKEN once gap G2 lands): %w", err)
	}
	idx := map[int64]vsInfo{}
	for _, t := range tenants {
		ten := core.Tenant{ID: strconv.FormatInt(t.ID, 10), Slug: t.Slug, Name: t.Name, Tier: parseTier(t.Tier)}
		for _, vid := range t.ViewIDs {
			var v viewFull
			if err := c.get(ctx, "/api/views/"+strconv.FormatInt(vid, 10), url.Values{"full": {"true"}}, &v); err != nil {
				return fmt.Errorf("view %d: %w", vid, err)
			}
			for _, vs := range v.ViewServices {
				idx[vs.ID] = vsInfo{tenant: ten, hasTen: true, service: vs.Service.Name, owner: vs.Service.Owner}
			}
		}
	}
	c.mu.Lock()
	c.index, c.indexedAt = idx, c.now()
	c.mu.Unlock()
	return nil
}

// BudgetBurned estimates error budget consumed in [from,to] from the
// remaining-budget values on status-change events. Multiplexer only writes
// events on transitions, so the result is "stale" (gap G4).
func (c *Client) BudgetBurned(ctx context.Context, sloID string, from, to time.Time) (float64, string, error) {
	evs, err := c.SloEvents(ctx, sloID, 500)
	if err != nil {
		return 0, "unavailable", err
	}
	return BudgetFromEvents(evs, from, to)
}

// BudgetFromEvents implements the estimate; evs newest first.
func BudgetFromEvents(evs []core.SloEvent, from, to time.Time) (float64, string, error) {
	var before, after *float64
	for _, e := range evs { // newest first
		if e.BudgetRemainingPct == nil {
			continue
		}
		if after == nil && !e.At.After(to) {
			after = e.BudgetRemainingPct
		}
		if before == nil && !e.At.After(from) {
			before = e.BudgetRemainingPct
		}
	}
	if before == nil {
		// No reading before the window: use the oldest reading inside it.
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].BudgetRemainingPct != nil && !evs[i].At.After(to) {
				before = evs[i].BudgetRemainingPct
				break
			}
		}
	}
	if before == nil || after == nil {
		return 0, "unavailable", nil
	}
	burned := *before - *after
	if burned < 0 {
		burned = 0
	}
	return burned, "stale", nil
}

var _ core.SloSource = (*Client)(nil)
