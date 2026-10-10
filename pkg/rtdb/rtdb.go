// Package rtdb implements a concurrent in-memory real-time database that
// holds the last-known state of every SCADA tag.
package rtdb

import (
	"sort"
	"sync"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

// TagState is the last-known state of a single tag.
type TagState struct {
	TagID       string
	IsDigital   bool
	Value       float64 // analog engineering value (0/1 for digital tags)
	RawValue    float64
	State       bool // digital state
	Unit        string
	Quality     scadav1.DataQuality
	TimestampMs int64
	UpdateCount uint64
	IsSOE       bool
	SourceFEP   string
}

// RTDB is a thread-safe last-value store keyed by tag ID.
type RTDB struct {
	mu   sync.RWMutex
	tags map[string]*TagState
}

// New creates an empty RTDB.
func New() *RTDB {
	return &RTDB{tags: make(map[string]*TagState, 1024)}
}

// UpdateAnalog stores an analog update and returns a copy of the new state.
func (db *RTDB) UpdateAnalog(fepID string, v *scadav1.AnalogTagValue) TagState {
	db.mu.Lock()
	defer db.mu.Unlock()
	t := db.slot(v.GetTagId())
	t.IsDigital = false
	t.Value = v.GetValue()
	t.RawValue = v.GetRawValue()
	t.Unit = v.GetEngineeringUnit()
	t.Quality = v.GetQuality()
	t.TimestampMs = v.GetTimestampUtcMs()
	t.IsSOE = false
	t.SourceFEP = fepID
	t.UpdateCount++
	return *t
}

// UpdateDigital stores a digital update and returns a copy of the new state.
func (db *RTDB) UpdateDigital(fepID string, v *scadav1.DigitalTagValue) TagState {
	db.mu.Lock()
	defer db.mu.Unlock()
	t := db.slot(v.GetTagId())
	t.IsDigital = true
	t.State = v.GetState()
	t.Value = 0
	if t.State {
		t.Value = 1
	}
	t.Quality = v.GetQuality()
	t.TimestampMs = v.GetTimestampUtcMs()
	t.IsSOE = v.GetIsSoe()
	t.SourceFEP = fepID
	t.UpdateCount++
	return *t
}

// slot returns the entry for id, creating it if needed. Caller holds db.mu.
func (db *RTDB) slot(id string) *TagState {
	t, ok := db.tags[id]
	if !ok {
		t = &TagState{TagID: id}
		db.tags[id] = t
	}
	return t
}

// Get returns a copy of the state of a tag.
func (db *RTDB) Get(tagID string) (TagState, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	t, ok := db.tags[tagID]
	if !ok {
		return TagState{}, false
	}
	return *t, true
}

// Snapshot returns copies of all tags sorted by tag ID.
func (db *RTDB) Snapshot() []TagState {
	db.mu.RLock()
	out := make([]TagState, 0, len(db.tags))
	for _, t := range db.tags {
		out = append(out, *t)
	}
	db.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].TagID < out[j].TagID })
	return out
}

// Len returns the number of tags held.
func (db *RTDB) Len() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return len(db.tags)
}
