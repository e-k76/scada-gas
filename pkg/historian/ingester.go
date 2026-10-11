package historian

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/alarm"
)

// IngesterConfig tunes batching and buffering.
type IngesterConfig struct {
	Filter        FilterConfig
	BatchSize     int           // flush as soon as this many rows are pending
	FlushInterval time.Duration // flush at least this often
	MaxPending    int           // per-table cap while the database is down
}

// DefaultIngesterConfig returns production-style defaults.
func DefaultIngesterConfig() IngesterConfig {
	return IngesterConfig{
		Filter:        DefaultFilterConfig(),
		BatchSize:     1000,
		FlushInterval: time.Second,
		MaxPending:    100000,
	}
}

// Stats are lifetime counters of an Ingester.
type Stats struct {
	Received  uint64 // analog+digital samples seen
	Stored    uint64 // analog+digital rows written
	Filtered  uint64 // samples suppressed by the deadband filter
	Alarms    uint64 // alarm rows written
	Dropped   uint64 // rows discarded because the pending cap was hit
	FlushErrs uint64
}

// Ingester applies the deadband filter, batches rows and writes them to a Store.
type Ingester struct {
	cfg   IngesterConfig
	store Store

	mu      sync.Mutex
	filter  *Filter
	analog  []AnalogRow
	digital []DigitalRow
	alarms  []AlarmRow

	wake chan struct{}

	received, stored, filtered, alarmRows, dropped, flushErrs atomic.Uint64
}

// NewIngester creates an Ingester writing to store.
func NewIngester(cfg IngesterConfig, store Store) *Ingester {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Second
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 100000
	}
	return &Ingester{cfg: cfg, store: store, filter: NewFilter(cfg.Filter), wake: make(chan struct{}, 1)}
}

// HandleTelemetry filters one telemetry batch and queues the rows to store.
func (g *Ingester) HandleTelemetry(u *scadav1.TelemetryStreamUpdate) {
	g.mu.Lock()
	for _, a := range u.GetAnalogUpdates() {
		g.received.Add(1)
		if !g.filter.ShouldStoreAnalog(a.GetTagId(), a.GetValue(), a.GetQuality(), a.GetTimestampUtcMs()) {
			g.filtered.Add(1)
			continue
		}
		g.analog = append(g.analog, AnalogRow{
			TimeMs: a.GetTimestampUtcMs(), TagID: a.GetTagId(), Value: a.GetValue(), Quality: int32(a.GetQuality()),
		})
	}
	for _, d := range u.GetDigitalUpdates() {
		g.received.Add(1)
		if !g.filter.ShouldStoreDigital(d.GetTagId(), d.GetState(), d.GetQuality(), d.GetTimestampUtcMs()) {
			g.filtered.Add(1)
			continue
		}
		g.digital = append(g.digital, DigitalRow{
			TimeMs: d.GetTimestampUtcMs(), TagID: d.GetTagId(), State: d.GetState(), Quality: int32(d.GetQuality()),
		})
	}
	g.capLocked()
	full := len(g.analog)+len(g.digital) >= g.cfg.BatchSize
	g.mu.Unlock()
	if full {
		g.signal()
	}
}

// HandleAlarm queues one alarm transition for the alarm_history table.
func (g *Ingester) HandleAlarm(ev *scadav1.AlarmEvent) {
	g.mu.Lock()
	g.alarms = append(g.alarms, AlarmRow{
		TimeMs: alarm.EventTimeMs(ev), AlarmID: ev.GetAlarmId(), TagID: ev.GetTagId(),
		Severity: int32(ev.GetSeverity()), State: int32(ev.GetState()), Message: ev.GetMessage(),
		TriggerValue: ev.GetTriggerValue(), AckByUser: ev.GetAckByUser(),
	})
	g.capLocked()
	g.mu.Unlock()
	g.signal() // alarms are rare and important: write them promptly
}

// capLocked drops the oldest rows beyond MaxPending. Caller holds g.mu.
func (g *Ingester) capLocked() {
	if n := len(g.analog) - g.cfg.MaxPending; n > 0 {
		g.analog = g.analog[n:]
		g.dropped.Add(uint64(n))
	}
	if n := len(g.digital) - g.cfg.MaxPending; n > 0 {
		g.digital = g.digital[n:]
		g.dropped.Add(uint64(n))
	}
	if n := len(g.alarms) - g.cfg.MaxPending; n > 0 {
		g.alarms = g.alarms[n:]
		g.dropped.Add(uint64(n))
	}
}

func (g *Ingester) signal() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Run flushes periodically and on demand until ctx is cancelled, then does a
// final flush.
func (g *Ingester) Run(ctx context.Context) {
	t := time.NewTicker(g.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := g.Flush(fctx); err != nil {
				log.Printf("[HIST] final flush failed: %v", err)
			}
			cancel()
			return
		case <-t.C:
		case <-g.wake:
		}
		fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := g.Flush(fctx); err != nil {
			log.Printf("[HIST] flush failed (rows kept for retry): %v", err)
		}
		cancel()
	}
}

// Flush writes all pending rows. Rows that could not be written are put back
// in front of the queue so they are retried on the next flush.
func (g *Ingester) Flush(ctx context.Context) error {
	g.mu.Lock()
	a, d, al := g.analog, g.digital, g.alarms
	g.analog, g.digital, g.alarms = nil, nil, nil
	g.mu.Unlock()

	var firstErr error
	fail := func(err error) {
		g.flushErrs.Add(1)
		if firstErr == nil {
			firstErr = err
		}
	}

	if len(a) > 0 {
		if err := g.store.InsertAnalog(ctx, a); err != nil {
			fail(err)
			g.mu.Lock()
			g.analog = append(a, g.analog...)
			g.mu.Unlock()
		} else {
			g.stored.Add(uint64(len(a)))
		}
	}
	if len(d) > 0 {
		if err := g.store.InsertDigital(ctx, d); err != nil {
			fail(err)
			g.mu.Lock()
			g.digital = append(d, g.digital...)
			g.mu.Unlock()
		} else {
			g.stored.Add(uint64(len(d)))
		}
	}
	if len(al) > 0 {
		if err := g.store.InsertAlarms(ctx, al); err != nil {
			fail(err)
			g.mu.Lock()
			g.alarms = append(al, g.alarms...)
			g.mu.Unlock()
		} else {
			g.alarmRows.Add(uint64(len(al)))
		}
	}
	if firstErr != nil {
		g.mu.Lock()
		g.capLocked()
		g.mu.Unlock()
	}
	return firstErr
}

// Stats returns a snapshot of the counters.
func (g *Ingester) Stats() Stats {
	return Stats{
		Received: g.received.Load(), Stored: g.stored.Load(), Filtered: g.filtered.Load(),
		Alarms: g.alarmRows.Load(), Dropped: g.dropped.Load(), FlushErrs: g.flushErrs.Load(),
	}
}

// String formats the stats for log output.
func (s Stats) String() string {
	pct := 0.0
	if s.Received > 0 {
		pct = 100 * float64(s.Filtered) / float64(s.Received)
	}
	return fmt.Sprintf("received=%d stored=%d filtered=%d (%.1f%%) alarm_rows=%d dropped=%d flush_errors=%d",
		s.Received, s.Stored, s.Filtered, pct, s.Alarms, s.Dropped, s.FlushErrs)
}

// LogStore is a Store that prints rows instead of writing a database; it is
// handy for trying the historian without PostgreSQL.
type LogStore struct{}

func (LogStore) InsertAnalog(_ context.Context, rows []AnalogRow) error {
	for _, r := range rows {
		log.Printf("[HIST] analog  %s %-22s %8.1f q=%d", ts(r.TimeMs).Format("15:04:05.000"), r.TagID, r.Value, r.Quality)
	}
	return nil
}

func (LogStore) InsertDigital(_ context.Context, rows []DigitalRow) error {
	for _, r := range rows {
		log.Printf("[HIST] digital %s %-22s %8t q=%d", ts(r.TimeMs).Format("15:04:05.000"), r.TagID, r.State, r.Quality)
	}
	return nil
}

func (LogStore) InsertAlarms(_ context.Context, rows []AlarmRow) error {
	for _, r := range rows {
		log.Printf("[HIST] alarm   %s %-22s sev=%d state=%d %s", ts(r.TimeMs).Format("15:04:05.000"), r.AlarmID, r.Severity, r.State, r.Message)
	}
	return nil
}

func (LogStore) Close() {}
