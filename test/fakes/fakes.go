// Package fakes provides in-memory port implementations for tests.
package fakes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

// Chat records posted messages and created channels.
type Chat struct {
	mu          sync.Mutex
	PlatformID  string
	NoWarRooms  bool
	Posted      []Posted
	Channels    []core.ChannelRef
	Invites     map[string][]core.UserRef
	Topics      map[string]string
	UsersByMail map[string]core.UserRef
	n           int
}

// Posted is a recorded post.
type Posted struct {
	Channel core.ChannelRef
	Msg     core.Message
}

// NewChat returns a fake chat for a platform.
func NewChat(platform string) *Chat {
	return &Chat{PlatformID: platform, Invites: map[string][]core.UserRef{}, Topics: map[string]string{}, UsersByMail: map[string]core.UserRef{}}
}

func (c *Chat) Platform() string { return c.PlatformID }

func (c *Chat) PostMessage(_ context.Context, ch core.ChannelRef, m core.Message) (core.MessageRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Posted = append(c.Posted, Posted{ch, m})
	return core.MessageRef{Channel: ch, ID: fmt.Sprint(len(c.Posted))}, nil
}

func (c *Chat) UpdateMessage(context.Context, core.MessageRef, core.Message) error { return nil }

func (c *Chat) CreateWarRoom(_ context.Context, name string, _ bool) (core.ChannelRef, error) {
	if c.NoWarRooms {
		return core.ChannelRef{}, core.ErrNotSupported
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	ch := core.ChannelRef{Platform: c.PlatformID, ID: fmt.Sprintf("C%03d", c.n), Name: name}
	c.Channels = append(c.Channels, ch)
	return ch, nil
}

func (c *Chat) Invite(_ context.Context, ch core.ChannelRef, users []core.UserRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Invites[ch.ID] = append(c.Invites[ch.ID], users...)
	return nil
}

func (c *Chat) SetTopic(_ context.Context, ch core.ChannelRef, topic string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Topics[ch.ID] = topic
	return nil
}

func (c *Chat) LookupUserByEmail(_ context.Context, email string) (core.UserRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.UsersByMail[email]; ok {
		return u, nil
	}
	return core.UserRef{}, core.ErrNotFound
}

// PostsTo returns messages posted to a channel ID.
func (c *Chat) PostsTo(id string) []core.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []core.Message
	for _, p := range c.Posted {
		if p.Channel.ID == id {
			out = append(out, p.Msg)
		}
	}
	return out
}

// Paging is a fake PagingProvider.
type Paging struct {
	mu       sync.Mutex
	OnCall   map[string][]core.UserRef
	Pages    []string
	Resolved []string
	Notes    []string
	Err      error
}

func (p *Paging) Name() string { return "fake-paging" }
func (p *Paging) WhoIsOnCall(_ context.Context, svc string) ([]core.UserRef, error) {
	return p.OnCall[svc], nil
}
func (p *Paging) Page(_ context.Context, svc string, inc *core.Incident, urgency string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return "", p.Err
	}
	ref := fmt.Sprintf("PD-%s-%s-%s", inc.Key, svc, urgency)
	p.Pages = append(p.Pages, ref)
	return ref, nil
}
func (p *Paging) Resolve(_ context.Context, ref string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Resolved = append(p.Resolved, ref)
	return nil
}
func (p *Paging) AddNote(_ context.Context, ref, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Notes = append(p.Notes, ref+":"+text)
	return nil
}

// Itsm is a fake ItsmProvider.
type Itsm struct {
	mu       sync.Mutex
	Tickets  []string
	Comments []string
	Items    []core.ActionItem
}

func (i *Itsm) Name() string { return "fake-itsm" }
func (i *Itsm) CreateIncidentTicket(_ context.Context, inc *core.Incident) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	ref := fmt.Sprintf("OPS-%d", len(i.Tickets)+1)
	i.Tickets = append(i.Tickets, ref)
	return ref, nil
}
func (i *Itsm) Comment(_ context.Context, ref, text string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Comments = append(i.Comments, ref+":"+text)
	return nil
}
func (i *Itsm) CreateActionItem(_ context.Context, _ *core.Incident, item core.ActionItem) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Items = append(i.Items, item)
	return fmt.Sprintf("OPS-AI-%d", len(i.Items)), nil
}

// Slo is a fake SloSource.
type Slo struct {
	Slos    map[string]core.Slo
	Tenants map[string]core.Tenant
	Events  map[string][]core.SloEvent
	Burned  float64
	Quality string
}

func (s *Slo) Name() string { return "fake-slo" }
func (s *Slo) ListSlos(context.Context) ([]core.Slo, error) {
	var out []core.Slo
	for _, v := range s.Slos {
		out = append(out, v)
	}
	return out, nil
}
func (s *Slo) GetSlo(_ context.Context, id string) (core.Slo, error) {
	v, ok := s.Slos[id]
	if !ok {
		return core.Slo{}, core.ErrNotFound
	}
	return v, nil
}
func (s *Slo) SloEvents(_ context.Context, id string, _ int) ([]core.SloEvent, error) {
	return s.Events[id], nil
}
func (s *Slo) TenantFor(_ context.Context, id string) (core.Tenant, bool, error) {
	t, ok := s.Tenants[id]
	return t, ok, nil
}
func (s *Slo) BudgetBurned(context.Context, string, time.Time, time.Time) (float64, string, error) {
	q := s.Quality
	if q == "" {
		q = "stale"
	}
	return s.Burned, q, nil
}

// Clock is a controllable clock.
type Clock struct {
	mu sync.Mutex
	T  time.Time
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.T
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.T = c.T.Add(d)
}

// IDs returns a deterministic ID generator.
func IDs(prefix string) func() string {
	var mu sync.Mutex
	n := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("%s%04d", prefix, n)
	}
}
