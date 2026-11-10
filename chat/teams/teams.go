// Package teams implements core.ChatPort for Microsoft Teams by speaking the
// Bot Framework REST protocol directly (no maintained Go SDK exists; see
// docs/architecture.md risk R2). Optional: enabled by teams.enabled.
//
// Verified against learn.microsoft.com (Bot Framework REST connector):
//   - Inbound JWT: OpenID metadata https://login.botframework.com/v1/.well-known/openidconfiguration,
//     issuer "https://api.botframework.com", audience = bot app ID, 5 minute
//     clock skew, "serviceUrl" claim must match activity.serviceUrl, keys carry
//     "endorsements" that must include activity.channelId when present.
//   - Outbound token: POST https://login.microsoftonline.com/botframework.com/oauth2/v2.0/token
//     (or /{tenantId}/ for single-tenant), grant_type=client_credentials,
//     scope=https://api.botframework.com/.default.
//   - POST /v3/conversations/{conversationId}/activities,
//     PUT  /v3/conversations/{conversationId}/activities/{activityId},
//     GET  /v3/conversations/{conversationId}/members/{memberId}.
//   - Mentions appear in text as <at>Name</at> plus entities of type "mention".
//
// UNVERIFIED: the member response fields "email"/"userPrincipalName" (the
// docs page shows SDK samples only). If absent, the user is treated as
// unmapped (can read and declare, cannot approve).
//
// Bots cannot create channels or add members without Microsoft Graph, so
// CreateWarRoom/Invite/SetTopic/LookupUserByEmail return core.ErrNotSupported
// and incidents run in the announce conversation (use --incident <key>).
package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Platform name.
const Platform = "teams"

// Client is the Teams adapter.
type Client struct {
	AppID       string
	AppPassword string
	TenantID    string
	TokenURL    string // override for tests
	HTTP        *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	emails  map[string]core.UserRef
}

// New builds a client.
func New(appID, appPassword, tenantID string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	tenant := "botframework.com"
	if tenantID != "" {
		tenant = tenantID
	}
	return &Client{AppID: appID, AppPassword: appPassword, TenantID: tenantID, HTTP: hc,
		TokenURL: "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/token", emails: map[string]core.UserRef{}}
}

func (c *Client) Platform() string { return Platform }

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.AppID},
		"client_secret": {c.AppPassword},
		"scope":         {"https://api.botframework.com/.default"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("teams token: %w", err)
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil || resp.StatusCode/100 != 2 || tr.AccessToken == "" {
		return "", fmt.Errorf("teams token: HTTP %d %s", resp.StatusCode, tr.Error)
	}
	c.token = tr.AccessToken
	c.expires = time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - 5*time.Minute)
	return c.token, nil
}

func (c *Client) do(ctx context.Context, method, serviceURL, path string, body any, out any) error {
	if serviceURL == "" {
		return fmt.Errorf("teams: missing serviceUrl for conversation")
	}
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(serviceURL, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("teams %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("teams %s %s: HTTP %d: %.200s", method, path, resp.StatusCode, b)
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Card renders a core.Message as an Adaptive Card attachment.
func Card(m core.Message) map[string]any {
	body := []any{}
	if m.Title != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": m.Title, "weight": "Bolder", "size": "Medium", "wrap": true})
	}
	if m.Text != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": m.Text, "wrap": true})
	}
	if len(m.Fields) > 0 {
		var facts []any
		for _, f := range m.Fields {
			facts = append(facts, map[string]any{"title": f.Label, "value": f.Value})
		}
		body = append(body, map[string]any{"type": "FactSet", "facts": facts})
	}
	card := map[string]any{"type": "AdaptiveCard", "$schema": "http://adaptivecards.io/schemas/adaptive-card.json", "version": "1.4", "body": body}
	if len(m.Buttons) > 0 {
		var acts []any
		for _, b := range m.Buttons {
			a := map[string]any{"type": "Action.Submit", "title": b.Label, "data": map[string]string{"actionId": b.ActionID, "value": b.Value}}
			if b.Style == "danger" {
				a["style"] = "destructive"
			} else if b.Style == "primary" {
				a["style"] = "positive"
			}
			acts = append(acts, a)
		}
		card["actions"] = acts
	}
	return map[string]any{"contentType": "application/vnd.microsoft.card.adaptive", "content": card}
}

func activity(m core.Message) map[string]any {
	return map[string]any{"type": "message", "text": m.Title, "attachments": []any{Card(m)}}
}

func (c *Client) PostMessage(ctx context.Context, ch core.ChannelRef, m core.Message) (core.MessageRef, error) {
	var resp struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, ch.ServiceURL, "/v3/conversations/"+url.PathEscape(ch.ID)+"/activities", activity(m), &resp); err != nil {
		return core.MessageRef{}, err
	}
	return core.MessageRef{Channel: ch, ID: resp.ID}, nil
}

func (c *Client) UpdateMessage(ctx context.Context, ref core.MessageRef, m core.Message) error {
	return c.do(ctx, http.MethodPut, ref.Channel.ServiceURL, "/v3/conversations/"+url.PathEscape(ref.Channel.ID)+"/activities/"+url.PathEscape(ref.ID), activity(m), nil)
}

func (c *Client) CreateWarRoom(context.Context, string, bool) (core.ChannelRef, error) {
	return core.ChannelRef{}, core.ErrNotSupported
}
func (c *Client) Invite(context.Context, core.ChannelRef, []core.UserRef) error { return core.ErrNotSupported }
func (c *Client) SetTopic(context.Context, core.ChannelRef, string) error       { return core.ErrNotSupported }
func (c *Client) LookupUserByEmail(context.Context, string) (core.UserRef, error) {
	return core.UserRef{}, core.ErrNotSupported
}

// Identify resolves a member to a UserRef with email via the conversation
// members API (cached).
func (c *Client) Identify(ctx context.Context, serviceURL, conversationID, memberID, name string) core.UserRef {
	c.mu.Lock()
	u, ok := c.emails[memberID]
	c.mu.Unlock()
	if ok {
		return u
	}
	var m struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		Email             string `json:"email"`
		UserPrincipalName string `json:"userPrincipalName"`
	}
	ref := core.UserRef{Platform: Platform, ID: memberID, Name: name}
	if err := c.do(ctx, http.MethodGet, serviceURL, "/v3/conversations/"+url.PathEscape(conversationID)+"/members/"+url.PathEscape(memberID), nil, &m); err == nil {
		ref.Email = m.Email
		if ref.Email == "" && strings.Contains(m.UserPrincipalName, "@") {
			ref.Email = m.UserPrincipalName
		}
		c.mu.Lock()
		c.emails[memberID] = ref
		c.mu.Unlock()
	}
	return ref
}

var _ core.ChatPort = (*Client)(nil)
