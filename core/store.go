package core

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// ErrNotFound is returned by stores for missing records.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when an append races (duplicate seq).
var ErrConflict = errors.New("conflict")

// IncidentFilter narrows ListIncidents.
type IncidentFilter struct {
	States []State
	Limit  int
}

// Store persists incidents, the append-only timeline and related records.
// The Postgres implementation lives in db/; MemStore is used in tests and
// for local runs without a database.
type Store interface {
	NextIncidentNumber(ctx context.Context, year int) (int, error)
	CreateIncident(ctx context.Context, inc *Incident) error
	UpdateIncident(ctx context.Context, inc *Incident) error
	GetIncident(ctx context.Context, id string) (*Incident, error)
	FindIncidentByChannel(ctx context.Context, ch ChannelRef) (*Incident, error)
	FindIncidentByKey(ctx context.Context, key string) (*Incident, error)
	FindOpenIncidentByDedupKey(ctx context.Context, key string) (*Incident, error)
	ListIncidents(ctx context.Context, f IncidentFilter) ([]*Incident, error)

	// AppendEvent must fail with ErrConflict if (incidentID, seq) exists.
	AppendEvent(ctx context.Context, ev TimelineEvent) error
	LastEvent(ctx context.Context, incidentID string) (*TimelineEvent, error)
	Events(ctx context.Context, incidentID string) ([]TimelineEvent, error)

	SaveSignal(ctx context.Context, s Signal) error
	GetSignal(ctx context.Context, id string) (*Signal, error)
	// RecordObservation stores the latest status for a dedup key and returns
	// how many consecutive observations have had that status.
	RecordObservation(ctx context.Context, dedupKey string, status SloStatus) (int, error)

	SaveAction(ctx context.Context, a *ActionRequest) error
	GetAction(ctx context.Context, id string) (*ActionRequest, error)

	SavePostmortem(ctx context.Context, p *Postmortem) error
	GetPostmortem(ctx context.Context, incidentID string) (*Postmortem, error)

	GetFlag(ctx context.Context, name string) (bool, error)
	SetFlag(ctx context.Context, name string, v bool) error
}

// isOpen reports whether an incident is still active for dedupe.
func isOpen(s State) bool {
	switch s {
	case StateResolved, StatePostmortem, StateClosed, StateDismissed:
		return false
	}
	return true
}

// IsOpen is the exported form, used by stores in other packages.
func IsOpen(s State) bool { return isOpen(s) }

type observation struct {
	status SloStatus
	count  int
}

// MemStore is an in-memory Store. Safe for concurrent use.
type MemStore struct {
	mu          sync.Mutex
	counters    map[int]int
	incidents   map[string]*Incident
	events      map[string][]TimelineEvent
	signals     []Signal
	obs         map[string]observation
	actions     map[string]*ActionRequest
	postmortems map[string]*Postmortem
	flags       map[string]bool
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{
		counters:    map[int]int{},
		incidents:   map[string]*Incident{},
		events:      map[string][]TimelineEvent{},
		obs:         map[string]observation{},
		actions:     map[string]*ActionRequest{},
		postmortems: map[string]*Postmortem{},
		flags:       map[string]bool{},
	}
}

func clone[T any](v *T) *T {
	c := *v
	return &c
}

func cloneIncident(i *Incident) *Incident {
	c := *i
	c.Services = slices.Clone(i.Services)
	c.Tenants = slices.Clone(i.Tenants)
	c.PageRefs = slices.Clone(i.PageRefs)
	c.SloRefs = slices.Clone(i.SloRefs)
	c.DedupKeys = slices.Clone(i.DedupKeys)
	c.Roles = make(map[Role]UserRef, len(i.Roles))
	for k, v := range i.Roles {
		c.Roles[k] = v
	}
	if i.WarRoom != nil {
		c.WarRoom = clone(i.WarRoom)
	}
	return &c
}

func (m *MemStore) NextIncidentNumber(_ context.Context, year int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[year]++
	return m.counters[year], nil
}

func (m *MemStore) CreateIncident(_ context.Context, inc *Incident) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.incidents[inc.ID]; ok {
		return ErrConflict
	}
	m.incidents[inc.ID] = cloneIncident(inc)
	return nil
}

func (m *MemStore) UpdateIncident(_ context.Context, inc *Incident) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.incidents[inc.ID]; !ok {
		return ErrNotFound
	}
	m.incidents[inc.ID] = cloneIncident(inc)
	return nil
}

func (m *MemStore) GetIncident(_ context.Context, id string) (*Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inc, ok := m.incidents[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneIncident(inc), nil
}

func (m *MemStore) FindIncidentByChannel(_ context.Context, ch ChannelRef) (*Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inc := range m.incidents {
		if inc.WarRoom != nil && inc.WarRoom.Platform == ch.Platform && inc.WarRoom.ID == ch.ID {
			return cloneIncident(inc), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) FindIncidentByKey(_ context.Context, key string) (*Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inc := range m.incidents {
		if inc.Key == key {
			return cloneIncident(inc), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) FindOpenIncidentByDedupKey(_ context.Context, key string) (*Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inc := range m.incidents {
		if isOpen(inc.State) && slices.Contains(inc.DedupKeys, key) {
			return cloneIncident(inc), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListIncidents(_ context.Context, f IncidentFilter) ([]*Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Incident
	for _, inc := range m.incidents {
		if len(f.States) > 0 && !slices.Contains(f.States, inc.State) {
			continue
		}
		out = append(out, cloneIncident(inc))
	}
	slices.SortFunc(out, func(a, b *Incident) int { return b.DetectedAt.Compare(a.DetectedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *MemStore) AppendEvent(_ context.Context, ev TimelineEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs := m.events[ev.IncidentID]
	if int64(len(evs))+1 != ev.Seq {
		return ErrConflict
	}
	m.events[ev.IncidentID] = append(evs, ev)
	return nil
}

func (m *MemStore) LastEvent(_ context.Context, id string) (*TimelineEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs := m.events[id]
	if len(evs) == 0 {
		return nil, nil
	}
	e := evs[len(evs)-1]
	return &e, nil
}

func (m *MemStore) Events(_ context.Context, id string) ([]TimelineEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.events[id]), nil
}

func (m *MemStore) SaveSignal(_ context.Context, s Signal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signals = append(m.signals, s)
	return nil
}

func (m *MemStore) GetSignal(_ context.Context, id string) (*Signal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.signals) - 1; i >= 0; i-- {
		if m.signals[i].ID == id {
			s := m.signals[i]
			return &s, nil
		}
	}
	return nil, ErrNotFound
}

// Signals returns stored signals (test helper).
func (m *MemStore) Signals() []Signal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.signals)
}

func (m *MemStore) RecordObservation(_ context.Context, key string, status SloStatus) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.obs[key]
	if o.status == status {
		o.count++
	} else {
		o = observation{status: status, count: 1}
	}
	m.obs[key] = o
	return o.count, nil
}

func (m *MemStore) SaveAction(_ context.Context, a *ActionRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *a
	c.Approvals = slices.Clone(a.Approvals)
	m.actions[a.ID] = &c
	return nil
}

func (m *MemStore) GetAction(_ context.Context, id string) (*ActionRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.actions[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *a
	c.Approvals = slices.Clone(a.Approvals)
	return &c, nil
}

func (m *MemStore) SavePostmortem(_ context.Context, p *Postmortem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.postmortems[p.IncidentID] = clone(p)
	return nil
}

func (m *MemStore) GetPostmortem(_ context.Context, id string) (*Postmortem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.postmortems[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(p), nil
}

func (m *MemStore) GetFlag(_ context.Context, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flags[name], nil
}

func (m *MemStore) SetFlag(_ context.Context, name string, v bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flags[name] = v
	return nil
}
