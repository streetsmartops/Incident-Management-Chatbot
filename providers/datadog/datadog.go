// Package datadog implements core.ObservabilityProvider (read-only) against
// the Datadog API.
//
// Verified against datadog-api-client-go v2.67.0 source and docs.datadoghq.com:
//
//	auth headers DD-API-KEY, DD-APPLICATION-KEY
//	GET  /api/v1/query?from=&to=&query=            -> {series:[{metric,scope,pointlist:[[ms,val]],tag_set}]}
//	POST /api/v2/logs/events/search {filter:{query,from,to},page:{limit},sort} -> {data:[{attributes:{timestamp,service,status,message,host,tags}}]}
//	GET  /api/v1/monitor?group_states=&monitor_tags= -> [{id,name,overall_state,query,tags}]
//	GET  /api/v2/events?filter[query]=&filter[from]=&filter[to]= -> {data:[{attributes:{timestamp,message,tags,attributes:{title}}}]}
//
// The adapter uses net/http rather than the (very large) generated SDK; the
// request/response field names above are copied from the SDK's models.
package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Client is the Datadog adapter.
type Client struct {
	base   string
	apiKey string
	appKey string
	http   *http.Client
}

// New builds a client. apiURL overrides the site-derived base URL.
func New(site, apiURL, apiKey, appKey string, hc *http.Client) *Client {
	base := strings.TrimRight(apiURL, "/")
	if base == "" {
		base = "https://api." + strings.TrimPrefix(site, "app.")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: base, apiKey: apiKey, appKey: appKey, http: hc}
}

func (c *Client) Name() string { return "datadog" }

// Capabilities: all four are supported.
func (c *Client) Capabilities() map[core.Capability]bool {
	return map[core.Capability]bool{core.CapMetrics: true, core.CapLogs: true, core.CapMonitors: true, core.CapEvents: true}
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("datadog %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("datadog %s: HTTP %d: %s", path, resp.StatusCode, msg)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("datadog %s: decode: %w", path, err)
	}
	return nil
}

// QueryMetric runs a timeseries query.
func (c *Client) QueryMetric(ctx context.Context, query string, from, to time.Time) ([]core.Series, error) {
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Series []struct {
			Metric    string       `json:"metric"`
			Scope     string       `json:"scope"`
			Pointlist [][]*float64 `json:"pointlist"`
			TagSet    []string     `json:"tag_set"`
		} `json:"series"`
	}
	q := url.Values{
		"from":  {strconv.FormatInt(from.Unix(), 10)},
		"to":    {strconv.FormatInt(to.Unix(), 10)},
		"query": {query},
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/query", q, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Status == "error" {
		return nil, fmt.Errorf("datadog query error: %s", resp.Error)
	}
	out := make([]core.Series, 0, len(resp.Series))
	for _, s := range resp.Series {
		cs := core.Series{Metric: s.Metric, Scope: s.Scope, Tags: map[string]string{}}
		for _, t := range s.TagSet {
			k, v, _ := strings.Cut(t, ":")
			cs.Tags[k] = v
		}
		for _, p := range s.Pointlist {
			if len(p) == 2 && p[0] != nil && p[1] != nil {
				cs.Points = append(cs.Points, [2]float64{*p[0], *p[1]})
			}
		}
		out = append(out, cs)
	}
	return out, nil
}

// SearchLogs searches logs (newest first).
func (c *Client) SearchLogs(ctx context.Context, query string, from, to time.Time, limit int) ([]core.LogLine, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	body := map[string]any{
		"filter": map[string]any{"query": query, "from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339)},
		"page":   map[string]any{"limit": limit},
		"sort":   "-timestamp",
	}
	var resp struct {
		Data []struct {
			Attributes struct {
				Timestamp time.Time `json:"timestamp"`
				Service   string    `json:"service"`
				Status    string    `json:"status"`
				Message   string    `json:"message"`
				Host      string    `json:"host"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v2/logs/events/search", nil, body, &resp); err != nil {
		return nil, err
	}
	out := make([]core.LogLine, 0, len(resp.Data))
	for _, d := range resp.Data {
		a := d.Attributes
		out = append(out, core.LogLine{At: a.Timestamp, Service: a.Service, Status: a.Status, Message: a.Message, Source: a.Host})
	}
	return out, nil
}

// MonitorsInState lists monitors whose groups are in the given states.
func (c *Client) MonitorsInState(ctx context.Context, states, tags []string) ([]core.Monitor, error) {
	q := url.Values{}
	if len(states) > 0 {
		q.Set("group_states", strings.Join(states, ","))
	}
	if len(tags) > 0 {
		q.Set("monitor_tags", strings.Join(tags, ","))
	}
	var resp []struct {
		ID           int64    `json:"id"`
		Name         string   `json:"name"`
		OverallState string   `json:"overall_state"`
		Query        string   `json:"query"`
		Tags         []string `json:"tags"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/monitor", q, nil, &resp); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, s := range states {
		want[strings.ToLower(s)] = true
	}
	var out []core.Monitor
	for _, m := range resp {
		if len(want) > 0 && !want[strings.ToLower(m.OverallState)] {
			continue
		}
		out = append(out, core.Monitor{ID: strconv.FormatInt(m.ID, 10), Name: m.Name, State: m.OverallState, Query: m.Query, Tags: m.Tags})
	}
	return out, nil
}

// Events lists events (deploys, config changes, alerts).
func (c *Client) Events(ctx context.Context, query string, from, to time.Time) ([]core.ObsEvent, error) {
	q := url.Values{
		"filter[from]": {from.UTC().Format(time.RFC3339)},
		"filter[to]":   {to.UTC().Format(time.RFC3339)},
		"page[limit]":  {"100"},
	}
	if query != "" {
		q.Set("filter[query]", query)
	}
	var resp struct {
		Data []struct {
			Attributes struct {
				Timestamp time.Time `json:"timestamp"`
				Message   string    `json:"message"`
				Tags      []string  `json:"tags"`
				Inner     struct {
					Title          string `json:"title"`
					SourceTypeName string `json:"source_type_name"`
				} `json:"attributes"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v2/events", q, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]core.ObsEvent, 0, len(resp.Data))
	for _, d := range resp.Data {
		a := d.Attributes
		out = append(out, core.ObsEvent{At: a.Timestamp, Title: a.Inner.Title, Text: a.Message, Source: a.Inner.SourceTypeName, Tags: a.Tags})
	}
	return out, nil
}

var _ core.ObservabilityProvider = (*Client)(nil)
