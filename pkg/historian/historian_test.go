package historian

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

const good = scadav1.DataQuality_QUALITY_GOOD

func TestAnalogDeadband(t *testing.T) {
	f := NewFilter(FilterConfig{AnalogDeadband: 0.1, Heartbeat: 60 * time.Second})
	steps := []struct {
		v     float64
		q     scadav1.DataQuality
		ts    int64
		store bool
	}{
		{250.0, good, 0, true},      // first sample
		{250.1, good, 500, false},   // +0.1 is not "more than" 0.1
		{250.05, good, 1000, false}, // inside deadband
		{250.2, good, 1500, true},   // deviates 0.2 from stored 250.0
		{250.2, good, 2000, false},  // unchanged
		{250.2, scadav1.DataQuality_QUALITY_BAD_COMM_FAILURE, 2500, true}, // quality change
		{250.2, scadav1.DataQuality_QUALITY_BAD_COMM_FAILURE, 3000, false},
		{250.2, scadav1.DataQuality_QUALITY_BAD_COMM_FAILURE, 63000, true}, // heartbeat (>=60s since 2500)
	}
	for i, s := range steps {
		if got := f.ShouldStoreAnalog("T", s.v, s.q, s.ts); got != s.store {
			t.Fatalf("step %d (%+v): got %v want %v", i, s, got, s.store)
		}
	}
}

func TestDigitalFilter(t *testing.T) {
	f := NewFilter(DefaultFilterConfig())
	if !f.ShouldStoreDigital("D", false, good, 0) {
		t.Fatal("first sample must store")
	}
	if f.ShouldStoreDigital("D", false, good, 500) {
		t.Fatal("unchanged must be filtered")
	}
	if !f.ShouldStoreDigital("D", true, good, 1000) {
		t.Fatal("toggle must store")
	}
	if !f.ShouldStoreDigital("D", true, good, 61000) {
		t.Fatal("heartbeat must store")
	}
}

func TestPerTagDeadband(t *testing.T) {
	f := NewFilter(FilterConfig{AnalogDeadband: 0.1, PerTagDeadband: map[string]float64{"X": 5}})
	f.ShouldStoreAnalog("X", 100, good, 0)
	if f.ShouldStoreAnalog("X", 103, good, 1) {
		t.Fatal("override deadband ignored")
	}
}

type fakeStore struct {
	mu      sync.Mutex
	analog  []AnalogRow
	digital []DigitalRow
	alarms  []AlarmRow
	failN   int
}

func (s *fakeStore) InsertAnalog(_ context.Context, r []AnalogRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failN > 0 {
		s.failN--
		return errors.New("db down")
	}
	s.analog = append(s.analog, r...)
	return nil
}
func (s *fakeStore) InsertDigital(_ context.Context, r []DigitalRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.digital = append(s.digital, r...)
	return nil
}
func (s *fakeStore) InsertAlarms(_ context.Context, r []AlarmRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alarms = append(s.alarms, r...)
	return nil
}
func (s *fakeStore) Close() {}

func upd(seq int64, ts int64, v float64) *scadav1.TelemetryStreamUpdate {
	return &scadav1.TelemetryStreamUpdate{
		SourceFepId: "F", PacketSequence: seq,
		AnalogUpdates:  []*scadav1.AnalogTagValue{{TagId: "A", Value: v, Quality: good, TimestampUtcMs: ts}},
		DigitalUpdates: []*scadav1.DigitalTagValue{{TagId: "D", State: false, Quality: good, TimestampUtcMs: ts}},
	}
}

func TestIngesterFiltersBatchesAndRetries(t *testing.T) {
	st := &fakeStore{failN: 1}
	g := NewIngester(IngesterConfig{Filter: DefaultFilterConfig(), BatchSize: 10, FlushInterval: time.Hour, MaxPending: 100}, st)
	g.HandleTelemetry(upd(1, 0, 250.0))
	g.HandleTelemetry(upd(2, 500, 250.05)) // filtered (both tags)
	g.HandleTelemetry(upd(3, 1000, 250.5)) // analog stored
	g.HandleAlarm(&scadav1.AlarmEvent{AlarmId: "A_H", TagId: "A", TriggerTimeUtcMs: 1000, Message: "m"})

	if err := g.Flush(context.Background()); err == nil {
		t.Fatal("expected first flush to fail")
	}
	if err := g.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.analog) != 2 || st.analog[0].Value != 250.0 || st.analog[1].Value != 250.5 {
		t.Fatalf("analog rows after retry: %+v", st.analog)
	}
	if len(st.digital) != 1 || len(st.alarms) != 1 {
		t.Fatalf("digital=%d alarms=%d", len(st.digital), len(st.alarms))
	}
	s := g.Stats()
	if s.Received != 6 || s.Filtered != 3 || s.Stored != 3 || s.Alarms != 1 || s.FlushErrs != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestIngesterPendingCap(t *testing.T) {
	g := NewIngester(IngesterConfig{Filter: FilterConfig{AnalogDeadband: 0}, BatchSize: 1 << 30, MaxPending: 3}, &fakeStore{})
	for i := 0; i < 10; i++ {
		g.HandleTelemetry(upd(int64(i), int64(i), float64(i)))
	}
	if len(g.analog) != 3 || g.Stats().Dropped == 0 {
		t.Fatalf("cap not enforced: %d %+v", len(g.analog), g.Stats())
	}
}
