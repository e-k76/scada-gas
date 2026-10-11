// Package audit records every supervisory control command attempt.
package audit

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Status of an audited command.
type Status string

const (
	Pending  Status = "PENDING"  // dispatched, no outcome yet
	Executed Status = "EXECUTED" // confirmed by the field device
	Failed   Status = "FAILED"   // the FEP or device refused / could not execute
	Rejected Status = "REJECTED" // refused by validation in the core
	NoFEP    Status = "NO_FEP"   // the station was offline
	Timeout  Status = "TIMEOUT"  // no result: outcome unknown
	Denied   Status = "DENIED"   // refused by the gateway (authentication, privileges, limits)
)

// Entry is one audit record.
type Entry struct {
	ID         int64
	Time       time.Time
	CommandID  string
	Operator   string
	Role       string
	RemoteAddr string
	StationID  string
	TagID      string
	Action     string
	Setpoint   *float64
	Status     Status
	Success    bool
	Message    string
	ExecutedAt *time.Time
	LatencyMs  *int64
}

// Result completes a PENDING entry.
type Result struct {
	Status     Status
	Success    bool
	Message    string
	ExecutedAt *time.Time
	LatencyMs  int64
}

// ErrNotPending is returned when completing an entry that is not PENDING.
var ErrNotPending = errors.New("audit entry is not pending")

// Store persists audit entries.
type Store interface {
	// Begin inserts e (normally with Status Pending, or a final refused status)
	// and returns its id. Commands must not be dispatched if Begin fails.
	Begin(ctx context.Context, e Entry) (int64, error)
	// Complete records the outcome of a pending entry.
	Complete(ctx context.Context, id int64, r Result) error
	// Recent returns the newest entries, optionally for one tag.
	Recent(ctx context.Context, tag string, limit int) ([]Entry, error)
	Ping(ctx context.Context) error
	Close()
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Sanitize bounds the free-text fields so a hostile request cannot bloat the log.
func Sanitize(e Entry) Entry {
	e.CommandID = clip(e.CommandID, 64)
	e.Operator = clip(e.Operator, 64)
	e.Role = clip(e.Role, 32)
	e.RemoteAddr = clip(e.RemoteAddr, 64)
	e.StationID = clip(e.StationID, 128)
	e.TagID = clip(e.TagID, 128)
	e.Action = clip(e.Action, 64)
	e.Message = clip(e.Message, 1000)
	return e
}

// MemoryStore keeps the audit log in memory (tests and database-less runs).
type MemoryStore struct {
	mu      sync.Mutex
	entries []Entry
	// FailBegin makes Begin fail (tests of the fail-closed behaviour).
	FailBegin bool
}

// NewMemoryStore creates an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (m *MemoryStore) Begin(_ context.Context, e Entry) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailBegin {
		return 0, errors.New("audit store unavailable")
	}
	e = Sanitize(e)
	e.ID = int64(len(m.entries) + 1)
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	m.entries = append(m.entries, e)
	return e.ID, nil
}

func (m *MemoryStore) Complete(_ context.Context, id int64, r Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || int(id) > len(m.entries) {
		return errors.New("audit entry not found")
	}
	e := &m.entries[id-1]
	if e.Status != Pending {
		return ErrNotPending
	}
	e.Status, e.Success, e.Message = r.Status, r.Success, clip(r.Message, 1000)
	e.ExecutedAt = r.ExecutedAt
	lat := r.LatencyMs
	e.LatencyMs = &lat
	return nil
}

func (m *MemoryStore) Recent(_ context.Context, tag string, limit int) ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Entry
	for i := len(m.entries) - 1; i >= 0 && len(out) < limit; i-- {
		if tag == "" || m.entries[i].TagID == tag {
			out = append(out, m.entries[i])
		}
	}
	return out, nil
}

// All returns a copy of every entry, oldest first (tests).
func (m *MemoryStore) All() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Entry(nil), m.entries...)
}

func (m *MemoryStore) Ping(context.Context) error { return nil }
func (m *MemoryStore) Close()                     {}
