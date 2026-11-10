// Package pagerduty implements core.PagingProvider with the PagerDuty REST
// API v2 (via github.com/PagerDuty/go-pagerduty) and normalises Webhooks v3.
//
// Verified from go-pagerduty v1.8.0 source:
//   - webhookv3.VerifySignature: header X-PagerDuty-Signature, "v1=" prefix,
//     HMAC-SHA256 over the raw body, comma-separated signatures allowed.
//   - CreateIncidentWithContext(ctx, from, opts) -> POST /incidents (From header)
//   - ManageIncidentsWithContext(ctx, from, []opts) -> PUT /incidents
//   - CreateIncidentNoteWithContext sends note.User.Summary as the From header.
//   - ListOnCallsWithContext with escalation_policy_ids[] and include[]=users.
//   - Webhook v3 envelope {event:{id,event_type,resource_type,occurred_at,data}}
//     (from the SDK's webhookv3 test fixture).
//
// UNVERIFIED (PagerDuty docs host not reachable from the build environment):
// the full list of incident event_type values beyond incident.priority_updated
// and whether data.incident_key is present in v3 incident payloads. The
// normaliser handles both presence and absence of incident_key.
package pagerduty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PagerDuty/go-pagerduty"
	"github.com/PagerDuty/go-pagerduty/webhookv3"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/providers/multiplexer"
)

// Name is the provider name used in dedup keys.
const Name = "pagerduty"

// Config configures the adapter.
type Config struct {
	APIURL         string
	Token          string
	WebhookSecret  string
	FromEmail      string
	Services       map[string]string // our name -> PD service ID
	DefaultService string
}

// Client is the PagerDuty adapter.
type Client struct {
	cfg     Config
	api     *pagerduty.Client
	reverse map[string]string // PD service ID -> our name

	mu      sync.Mutex
	epCache map[string]string // PD service ID -> escalation policy ID
}

// New builds the adapter.
func New(cfg Config, hc *http.Client) *Client {
	opts := []pagerduty.ClientOptions{}
	if cfg.APIURL != "" {
		opts = append(opts, pagerduty.WithAPIEndpoint(strings.TrimRight(cfg.APIURL, "/")))
	}
	api := pagerduty.NewClient(cfg.Token, opts...)
	if hc != nil {
		api.HTTPClient = hc
	}
	rev := map[string]string{}
	for name, id := range cfg.Services {
		rev[id] = name
	}
	return &Client{cfg: cfg, api: api, reverse: rev, epCache: map[string]string{}}
}

func (c *Client) Name() string { return Name }

func (c *Client) serviceID(service string) (string, error) {
	if id, ok := c.cfg.Services[service]; ok {
		return id, nil
	}
	if c.cfg.DefaultService != "" {
		return c.cfg.DefaultService, nil
	}
	return "", fmt.Errorf("no PagerDuty service mapped for %q and no defaultService configured", service)
}

// WhoIsOnCall returns current on-call users for the service's escalation policy.
func (c *Client) WhoIsOnCall(ctx context.Context, service string) ([]core.UserRef, error) {
	sid, err := c.serviceID(service)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	ep, ok := c.epCache[sid]
	c.mu.Unlock()
	if !ok {
		svc, err := c.api.GetServiceWithContext(ctx, sid, &pagerduty.GetServiceOptions{})
		if err != nil {
			return nil, fmt.Errorf("pagerduty get service %s: %w", sid, err)
		}
		ep = svc.EscalationPolicy.ID
		c.mu.Lock()
		c.epCache[sid] = ep
		c.mu.Unlock()
	}
	resp, err := c.api.ListOnCallsWithContext(ctx, pagerduty.ListOnCallOptions{
		EscalationPolicyIDs: []string{ep}, Includes: []string{"users"}, Earliest: true,
	})
	if err != nil {
		return nil, fmt.Errorf("pagerduty list oncalls: %w", err)
	}
	seen := map[string]bool{}
	var out []core.UserRef
	for _, oc := range resp.OnCalls {
		if oc.EscalationLevel != 1 || seen[oc.User.ID] {
			continue
		}
		seen[oc.User.ID] = true
		out = append(out, core.UserRef{Platform: Name, ID: oc.User.ID, Email: oc.User.Email, Name: oc.User.Name})
	}
	return out, nil
}

// Page creates a PagerDuty incident for our incident. Returns the PD incident ID.
func (c *Client) Page(ctx context.Context, service string, inc *core.Incident, urgency string) (string, error) {
	sid, err := c.serviceID(service)
	if err != nil {
		return "", err
	}
	details := fmt.Sprintf("%s %s: %s\nState: %s\nDeclared by incidentd. Correlation: %s", inc.Key, inc.Severity, inc.Title, inc.State, inc.CorrelationID)
	pd, err := c.api.CreateIncidentWithContext(ctx, c.cfg.FromEmail, &pagerduty.CreateIncidentOptions{
		Title:       fmt.Sprintf("[%s] %s %s", inc.Key, inc.Severity, inc.Title),
		Service:     &pagerduty.APIReference{ID: sid, Type: "service_reference"},
		Urgency:     urgency,
		IncidentKey: "incidentd-" + inc.Key + "-" + sid,
		Body:        &pagerduty.APIDetails{Type: "incident_body", Details: details},
	})
	if err != nil {
		return "", fmt.Errorf("pagerduty create incident: %w", err)
	}
	return pd.ID, nil
}

// Resolve resolves a PD incident we opened.
func (c *Client) Resolve(ctx context.Context, ref string) error {
	_, err := c.api.ManageIncidentsWithContext(ctx, c.cfg.FromEmail, []pagerduty.ManageIncidentsOptions{{ID: ref, Status: "resolved"}})
	if err != nil {
		return fmt.Errorf("pagerduty resolve %s: %w", ref, err)
	}
	return nil
}

// AddNote adds a note to a PD incident.
func (c *Client) AddNote(ctx context.Context, ref, text string) error {
	_, err := c.api.CreateIncidentNoteWithContext(ctx, ref, pagerduty.IncidentNote{
		Content: text,
		User:    pagerduty.APIObject{Summary: c.cfg.FromEmail}, // sent as From header
	})
	if err != nil {
		return fmt.Errorf("pagerduty note %s: %w", ref, err)
	}
	return nil
}

// ---------------------------------------------------------------- Webhooks

// ErrBadSignature is returned for unverifiable webhooks.
var ErrBadSignature = errors.New("pagerduty webhook signature invalid")

type webhookEnvelope struct {
	Event struct {
		ID           string          `json:"id"`
		EventType    string          `json:"event_type"`
		ResourceType string          `json:"resource_type"`
		OccurredAt   time.Time       `json:"occurred_at"`
		Data         json.RawMessage `json:"data"`
	} `json:"event"`
}

type incidentData struct {
	ID          string `json:"id"`
	Number      int    `json:"number"`
	Status      string `json:"status"`
	Title       string `json:"title"`
	Urgency     string `json:"urgency"`
	IncidentKey string `json:"incident_key"` // UNVERIFIED in v3; optional
	Service     struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	} `json:"service"`
	Priority *struct {
		Summary string `json:"summary"`
	} `json:"priority"`
}

// GuardrailResolver maps a Multiplexer guardrail name to its ID.
type GuardrailResolver func(ctx context.Context, name string) (id string, ok bool)

var guardrailKeyRe = regexp.MustCompile(`^slo-guardrail-(\d+)$`)

// VerifyAndNormalize verifies the signature and turns an incident webhook
// into a core.Signal. Non-incident events return (nil, nil).
func (c *Client) VerifyAndNormalize(ctx context.Context, r *http.Request, resolve GuardrailResolver) (*core.Signal, error) {
	if err := webhookv3.VerifySignature(r, c.cfg.WebhookSecret); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return c.Normalize(ctx, body, resolve)
}

// Normalize converts a verified webhook body to a Signal.
func (c *Client) Normalize(ctx context.Context, body []byte, resolve GuardrailResolver) (*core.Signal, error) {
	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("pagerduty webhook decode: %w", err)
	}
	if env.Event.ResourceType != "incident" {
		return nil, nil
	}
	var d incidentData
	if err := json.Unmarshal(env.Event.Data, &d); err != nil {
		return nil, fmt.Errorf("pagerduty webhook data: %w", err)
	}
	sig := &core.Signal{
		Source:     Name,
		ExternalID: d.ID + ":" + env.Event.EventType,
		DedupKey:   core.PageDedupKey(Name, d.ID),
		Title:      d.Title,
		Tier:       core.TierUnknown,
		ObservedAt: env.Event.OccurredAt,
		Raw:        body,
	}
	if name, ok := c.reverse[d.Service.ID]; ok {
		sig.Service = name
	} else {
		sig.Service = d.Service.Summary
	}
	switch {
	case env.Event.EventType == "incident.resolved" || d.Status == "resolved":
		sig.Resolved = true
	case env.Event.EventType == "incident.acknowledged" || d.Status == "acknowledged":
		sig.Acknowledged = true
	}
	sig.SeverityHint = severityHint(d)

	// Correlate Multiplexer-originated pages with the guardrail.
	if m := guardrailKeyRe.FindStringSubmatch(d.IncidentKey); m != nil {
		sig.SloID, sig.DedupKey = m[1], multiplexer.DedupKey(m[1])
	} else if strings.HasPrefix(d.Title, multiplexer.PagerDutyTitlePrefix) && resolve != nil {
		if id, ok := resolve(ctx, strings.TrimPrefix(d.Title, multiplexer.PagerDutyTitlePrefix)); ok {
			sig.SloID, sig.DedupKey = id, multiplexer.DedupKey(id)
		}
	}
	return sig, nil
}

func severityHint(d incidentData) core.Severity {
	if d.Priority != nil {
		switch strings.ToUpper(d.Priority.Summary) {
		case "P1":
			return core.Sev1
		case "P2":
			return core.Sev2
		case "P3":
			return core.Sev3
		case "P4", "P5":
			return core.Sev4
		}
	}
	if d.Urgency == "high" {
		return core.Sev2
	}
	return core.Sev3
}

var _ core.PagingProvider = (*Client)(nil)
