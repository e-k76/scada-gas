package rtdb

import (
	"fmt"
	"sync"
	"testing"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

func TestUpdateAndGet(t *testing.T) {
	db := New()
	db.UpdateAnalog("FEP", &scadav1.AnalogTagValue{TagId: "A", Value: 1.5, Quality: scadav1.DataQuality_QUALITY_GOOD, TimestampUtcMs: 10})
	s := db.UpdateAnalog("FEP", &scadav1.AnalogTagValue{TagId: "A", Value: 2.5, Quality: scadav1.DataQuality_QUALITY_GOOD, TimestampUtcMs: 20})
	if s.UpdateCount != 2 || s.Value != 2.5 {
		t.Fatalf("unexpected state %+v", s)
	}
	d := db.UpdateDigital("FEP", &scadav1.DigitalTagValue{TagId: "D", State: true, IsSoe: true})
	if !d.State || d.Value != 1 || !d.IsSOE {
		t.Fatalf("unexpected digital %+v", d)
	}
	if _, ok := db.Get("missing"); ok {
		t.Fatal("missing tag found")
	}
	if db.Len() != 2 {
		t.Fatalf("len=%d", db.Len())
	}
}

func TestConcurrent(t *testing.T) {
	db := New()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				db.UpdateAnalog("F", &scadav1.AnalogTagValue{TagId: fmt.Sprintf("T%d", i%10), Value: float64(i)})
				db.Snapshot()
			}
		}(w)
	}
	wg.Wait()
	var total uint64
	for _, s := range db.Snapshot() {
		total += s.UpdateCount
	}
	if total != 8000 {
		t.Fatalf("total=%d", total)
	}
}
