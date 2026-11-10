// Package jira implements core.ItsmProvider against Jira Cloud REST API v3.
//
// Verified against developer.atlassian.com:
//
//	POST /rest/api/3/issue                         (fields.project.key, summary, issuetype.name, description as ADF)
//	POST /rest/api/3/issue/{issueIdOrKey}/comment  (body as ADF)
//	Auth: Basic base64("email:api_token")
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Config configures the adapter (Q6: project and issue types configurable).
type Config struct {
	BaseURL             string
	Email               string
	APIToken            string
	ProjectKey          string
	IssueType           string
	ActionItemIssueType string
}

// Client is the Jira adapter.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds the adapter.
func New(cfg Config, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Client{cfg: cfg, http: hc}
}

func (c *Client) Name() string { return "jira" }

// adf wraps plain text paragraphs as an Atlassian Document Format doc.
func adf(text string) map[string]any {
	var paras []any
	for _, p := range strings.Split(text, "\n") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		paras = append(paras, map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": p}}})
	}
	if len(paras) == 0 {
		paras = []any{map[string]any{"type": "paragraph", "content": []any{}}}
	}
	return map[string]any{"type": "doc", "version": 1, "content": paras}
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.cfg.Email, c.cfg.APIToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira %s: %w", path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		msg := string(rb)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("jira %s: HTTP %d: %s", path, resp.StatusCode, msg)
	}
	if out != nil {
		return json.Unmarshal(rb, out)
	}
	return nil
}

func (c *Client) create(ctx context.Context, summary, description, issueType string, labels []string) (string, error) {
	var resp struct {
		Key string `json:"key"`
	}
	body := map[string]any{"fields": map[string]any{
		"project":     map[string]any{"key": c.cfg.ProjectKey},
		"summary":     summary,
		"issuetype":   map[string]any{"name": issueType},
		"description": adf(description),
		"labels":      labels,
	}}
	if err := c.post(ctx, "/rest/api/3/issue", body, &resp); err != nil {
		return "", err
	}
	return resp.Key, nil
}

// CreateIncidentTicket files the incident ticket.
func (c *Client) CreateIncidentTicket(ctx context.Context, inc *core.Incident) (string, error) {
	desc := fmt.Sprintf("%s %s: %s\nState: %s\nServices: %s\nTenants: %s\nCorrelation ID: %s\nFiled automatically by incidentd.",
		inc.Key, inc.Severity, inc.Title, inc.State, strings.Join(inc.Services, ", "), strings.Join(inc.Tenants, ", "), inc.CorrelationID)
	return c.create(ctx, fmt.Sprintf("[%s] %s %s", inc.Key, inc.Severity, inc.Title), desc, c.cfg.IssueType,
		[]string{"incident", strings.ToLower(inc.Severity.String()), strings.ToLower(inc.Key)})
}

// Comment adds a comment.
func (c *Client) Comment(ctx context.Context, ticketRef, text string) error {
	return c.post(ctx, "/rest/api/3/issue/"+url.PathEscape(ticketRef)+"/comment", map[string]any{"body": adf(text)}, nil)
}

// CreateActionItem files a postmortem action item.
func (c *Client) CreateActionItem(ctx context.Context, inc *core.Incident, item core.ActionItem) (string, error) {
	desc := fmt.Sprintf("Postmortem action item for %s (%s).\nOwner: %s\nDue: %s", inc.Key, inc.Title, item.Owner, item.Due)
	if inc.TicketRef != "" {
		desc += "\nIncident ticket: " + inc.TicketRef
	}
	return c.create(ctx, item.Title, desc, c.cfg.ActionItemIssueType, []string{"postmortem-action", strings.ToLower(inc.Key)})
}

var _ core.ItsmProvider = (*Client)(nil)
