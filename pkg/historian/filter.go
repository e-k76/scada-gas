// Package historian stores deadband-filtered telemetry and alarm history in
// TimescaleDB / PostgreSQL.
package historian

import (
	"math"
	"time"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

// FilterConfig controls which samples are worth persisting.
type FilterConfig struct {
	// AnalogDeadband is the minimum absolute change (in engineering units)
	// versus the last stored value. 0.1 stores a sample only when the value
	// deviates by more than +-0.1 PSI / +-0.1 °C.
	AnalogDeadband float64
	// PerTagDeadband overrides AnalogDeadband for specific tags.
	PerTagDeadband map[string]float64
	// Heartbeat forces a sample to be stored when nothing was stored for
	// this tag for at least this long (sample time), so flat signals still
	// leave a trace. Zero disables the heartbeat.
	Heartbeat time.Duration
}

// DefaultFilterConfig returns the standard +-0.1 deadband with a 60 s heartbeat.
func DefaultFilterConfig() FilterConfig {
	return FilterConfig{AnalogDeadband: 0.1, Heartbeat: 60 * time.Second}
}

type lastStored struct {
	value   float64
	state   bool
	quality scadav1.DataQuality
	tsMs    int64
}

// Filter decides whether a sample must be persisted. It compares against the
// last *stored* sample of each tag. It is not safe for concurrent use; the
// Ingester serialises access.
type Filter struct {
	cfg     FilterConfig
	analog  map[string]lastStored
	digital map[string]lastStored
}

// NewFilter creates a filter.
func NewFilter(cfg FilterConfig) *Filter {
	return &Filter{cfg: cfg, analog: make(map[string]lastStored), digital: make(map[string]lastStored)}
}

const epsilon = 1e-9

func (f *Filter) deadband(tag string) float64 {
	if d, ok := f.cfg.PerTagDeadband[tag]; ok {
		return d
	}
	return f.cfg.AnalogDeadband
}

func (f *Filter) heartbeatDue(last, now int64) bool {
	return f.cfg.Heartbeat > 0 && now-last >= f.cfg.Heartbeat.Milliseconds()
}

// ShouldStoreAnalog reports whether the sample must be persisted and, if so,
// records it as the new reference point.
func (f *Filter) ShouldStoreAnalog(tag string, value float64, q scadav1.DataQuality, tsMs int64) bool {
	prev, seen := f.analog[tag]
	store := !seen ||
		q != prev.quality ||
		math.Abs(value-prev.value) > f.deadband(tag)+epsilon ||
		f.heartbeatDue(prev.tsMs, tsMs)
	if store {
		f.analog[tag] = lastStored{value: value, quality: q, tsMs: tsMs}
	}
	return store
}

// ShouldStoreDigital reports whether the sample must be persisted: first
// sample, state toggle, quality change or heartbeat.
func (f *Filter) ShouldStoreDigital(tag string, state bool, q scadav1.DataQuality, tsMs int64) bool {
	prev, seen := f.digital[tag]
	store := !seen || state != prev.state || q != prev.quality || f.heartbeatDue(prev.tsMs, tsMs)
	if store {
		f.digital[tag] = lastStored{state: state, quality: q, tsMs: tsMs}
	}
	return store
}
