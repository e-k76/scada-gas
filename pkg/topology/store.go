// Package topology persists station topologies (see package model) with
// versioning and optimistic concurrency.
package topology

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/example/scada-gas/pkg/model"
)

var (
	// ErrNotFound is returned for an unknown station or version.
	ErrNotFound = errors.New("topology not found")
	// ErrConflict is returned when a save is based on an outdated version.
	ErrConflict = errors.New("topology was modified by someone else")
)

// ConflictError carries the version currently stored.
type ConflictError struct{ Current int64 }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%v (stored version is %d)", ErrConflict, e.Current)
}
func (e *ConflictError) Unwrap() error { return ErrConflict }

// Record is a stored topology with its bookkeeping.
type Record struct {
	StationID       string
	Name            string
	Version         int64
	Document        *model.StationTopology
	UpdatedAt       time.Time
	UpdatedBy       string
	DeployedVersion int64 // 0 = never deployed
	DeployedAt      *time.Time
	DeployedBy      string
}

// Summary is one row of the station list.
type Summary struct {
	StationID       string
	Name            string
	Version         int64
	UpdatedAt       time.Time
	UpdatedBy       string
	DeployedVersion int64
	DeployedAt      *time.Time
	DeployedBy      string
}

// Store is the persistence interface used by the gateway.
type Store interface {
	// Get returns the latest saved version of a station.
	Get(ctx context.Context, stationID string) (*Record, error)
	// GetVersion returns the document saved as the given version.
	GetVersion(ctx context.Context, stationID string, version int64) (*model.StationTopology, error)
	List(ctx context.Context) ([]Summary, error)
	// Save stores doc as version expectedVersion+1. expectedVersion must be 0
	// for a new station, otherwise it must equal the stored version.
	Save(ctx context.Context, doc *model.StationTopology, by string, expectedVersion int64) (*Record, error)
	// MarkDeployed records that version of the station was deployed to the runtime.
	MarkDeployed(ctx context.Context, stationID string, version int64, by string) error
	Ping(ctx context.Context) error
	Close()
}

// cloneDoc deep-copies a topology through its own fields.
func cloneDoc(t *model.StationTopology) *model.StationTopology {
	c := *t
	c.Components = make([]model.EquipmentNode, len(t.Components))
	for i, n := range t.Components {
		n.Display.ColorRules = append([]model.ColorRule(nil), n.Display.ColorRules...)
		if n.Display.Decimals != nil {
			d := *n.Display.Decimals
			n.Display.Decimals = &d
		}
		c.Components[i] = n
	}
	c.Pipelines = make([]model.PipelineSegment, len(t.Pipelines))
	for i, p := range t.Pipelines {
		p.PathPoints = append([]model.Point{}, p.PathPoints...)
		c.Pipelines[i] = p
	}
	c.PressureBands = append([]model.PressureBand(nil), t.PressureBands...)
	return &c
}

// MemoryStore keeps topologies in memory (tests and database-less runs).
type MemoryStore struct {
	mu      sync.Mutex
	current map[string]*Record
	history map[string]map[int64]*model.StationTopology
}

// NewMemoryStore creates an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{current: map[string]*Record{}, history: map[string]map[int64]*model.StationTopology{}}
}

func (m *MemoryStore) copyRecord(r *Record) *Record {
	c := *r
	c.Document = cloneDoc(r.Document)
	return &c
}

func (m *MemoryStore) Get(_ context.Context, id string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.current[id]
	if !ok {
		return nil, ErrNotFound
	}
	return m.copyRecord(r), nil
}

func (m *MemoryStore) GetVersion(_ context.Context, id string, v int64) (*model.StationTopology, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.history[id][v]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneDoc(d), nil
}

func (m *MemoryStore) List(context.Context) ([]Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Summary, 0, len(m.current))
	for _, r := range m.current {
		out = append(out, Summary{r.StationID, r.Name, r.Version, r.UpdatedAt, r.UpdatedBy, r.DeployedVersion, r.DeployedAt, r.DeployedBy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StationID < out[j].StationID })
	return out, nil
}

func (m *MemoryStore) Save(_ context.Context, doc *model.StationTopology, by string, expected int64) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.current[doc.StationID]
	switch {
	case cur == nil && expected != 0:
		return nil, &ConflictError{Current: 0}
	case cur != nil && expected != cur.Version:
		return nil, &ConflictError{Current: cur.Version}
	}
	next := int64(1)
	if cur != nil {
		next = cur.Version + 1
	}
	now := time.Now().UTC()
	d := cloneDoc(doc)
	d.Version, d.UpdatedAtMs, d.UpdatedBy = next, now.UnixMilli(), by
	rec := &Record{StationID: d.StationID, Name: d.Name, Version: next, Document: d, UpdatedAt: now, UpdatedBy: by}
	if cur != nil {
		rec.DeployedVersion, rec.DeployedAt, rec.DeployedBy = cur.DeployedVersion, cur.DeployedAt, cur.DeployedBy
	}
	m.current[d.StationID] = rec
	if m.history[d.StationID] == nil {
		m.history[d.StationID] = map[int64]*model.StationTopology{}
	}
	m.history[d.StationID][next] = cloneDoc(d)
	return m.copyRecord(rec), nil
}

func (m *MemoryStore) MarkDeployed(_ context.Context, id string, version int64, by string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.current[id]
	if !ok || version > r.Version || version < 1 {
		return ErrNotFound
	}
	now := time.Now().UTC()
	r.DeployedVersion, r.DeployedAt, r.DeployedBy = version, &now, by
	return nil
}

func (m *MemoryStore) Ping(context.Context) error { return nil }
func (m *MemoryStore) Close()                     {}
