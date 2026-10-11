package alarm

import (
	"errors"
	"sync"
	"testing"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

const good = scadav1.DataQuality_QUALITY_GOOD

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine()
	var clock int64 = 1000
	e.SetClock(func() int64 { clock += 10; return clock })
	if err := DefaultRules(e, "S"); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestHighLimitLifecycleAckThenRTN(t *testing.T) {
	e := newTestEngine(t)
	if ev := e.EvaluateAnalog("S_P_IN", 250.0, "PSI", good, 1); len(ev) != 0 {
		t.Fatalf("unexpected events %v", ev)
	}
	ev := e.EvaluateAnalog("S_P_IN", 250.6, "PSI", good, 2)
	if len(ev) != 1 || ev[0].AlarmId != "S_P_IN_H" || ev[0].State != scadav1.AlarmState_ALARM_UNACK_ALARM ||
		ev[0].Severity != scadav1.AlarmSeverity_SEVERITY_HIGH {
		t.Fatalf("expected H raise, got %v", ev)
	}
	// still active, no new event
	if ev := e.EvaluateAnalog("S_P_IN", 250.7, "PSI", good, 3); len(ev) != 0 {
		t.Fatalf("unexpected %v", ev)
	}
	ack, err := e.Acknowledge("S_P_IN_H", "op1")
	if err != nil || ack.State != scadav1.AlarmState_ALARM_ACK_ALARM || ack.AckByUser != "op1" || ack.AckTimeUtcMs == 0 {
		t.Fatalf("ack failed: %v %v", ack, err)
	}
	if _, err := e.Acknowledge("S_P_IN_H", "op1"); !errors.Is(err, ErrAlreadyAcknowledged) {
		t.Fatalf("want ErrAlreadyAcknowledged, got %v", err)
	}
	// inside deadband (250.5-0.2=250.3): remains active
	if ev := e.EvaluateAnalog("S_P_IN", 250.4, "PSI", good, 4); len(ev) != 0 {
		t.Fatalf("chatter inside deadband: %v", ev)
	}
	ev = e.EvaluateAnalog("S_P_IN", 250.2, "PSI", good, 5)
	if len(ev) != 1 || ev[0].State != scadav1.AlarmState_ALARM_ACK_RTN {
		t.Fatalf("expected ACK_RTN clear, got %v", ev)
	}
	if len(e.ActiveAlarms("", 0)) != 0 {
		t.Fatal("alarm should be cleared from active list")
	}
}

func TestReturnToNormalBeforeAck(t *testing.T) {
	e := newTestEngine(t)
	e.EvaluateAnalog("S_P_IN", 250.6, "PSI", good, 1)
	ev := e.EvaluateAnalog("S_P_IN", 250.0, "PSI", good, 2)
	if len(ev) != 1 || ev[0].State != scadav1.AlarmState_ALARM_UNACK_RTN || ev[0].RtnTimeUtcMs != 2 {
		t.Fatalf("expected UNACK_RTN, got %v", ev)
	}
	if got := e.ActiveAlarms("", 0); len(got) != 1 {
		t.Fatalf("UNACK_RTN must stay in list, got %d", len(got))
	}
	// re-alarm before ack
	ev = e.EvaluateAnalog("S_P_IN", 250.9, "PSI", good, 3)
	if len(ev) != 1 || ev[0].State != scadav1.AlarmState_ALARM_UNACK_ALARM || ev[0].TriggerTimeUtcMs != 3 || ev[0].RtnTimeUtcMs != 0 {
		t.Fatalf("expected re-alarm, got %v", ev)
	}
	e.EvaluateAnalog("S_P_IN", 250.0, "PSI", good, 4)
	ack, err := e.Acknowledge("S_P_IN_H", "op")
	if err != nil || ack.State != scadav1.AlarmState_ALARM_ACK_RTN {
		t.Fatalf("ack of UNACK_RTN: %v %v", ack, err)
	}
	if len(e.ActiveAlarms("", 0)) != 0 {
		t.Fatal("should be cleared")
	}
}

func TestHighHighAndHighBothActive(t *testing.T) {
	e := newTestEngine(t)
	ev := e.EvaluateAnalog("S_P_IN", 251.5, "PSI", good, 1)
	if len(ev) != 2 {
		t.Fatalf("expected HH and H, got %v", ev)
	}
	act := e.ActiveAlarms("", 0)
	if act[0].AlarmId != "S_P_IN_HH" || act[0].Severity != scadav1.AlarmSeverity_SEVERITY_CRITICAL {
		t.Fatalf("critical must sort first: %v", act)
	}
	if act[0].Message != "Inlet pressure exceeded High-High limit (251.5 PSI)" {
		t.Fatalf("message: %q", act[0].Message)
	}
	if got := e.ActiveAlarms("", scadav1.AlarmSeverity_SEVERITY_CRITICAL); len(got) != 1 {
		t.Fatalf("severity filter: %d", len(got))
	}
}

func TestLowLimits(t *testing.T) {
	e := newTestEngine(t)
	ev := e.EvaluateAnalog("S_P_IN", 248.4, "PSI", good, 1)
	if len(ev) != 1 || ev[0].AlarmId != "S_P_IN_L" || ev[0].Message != "Inlet pressure fell below Low limit (248.4 PSI)" {
		t.Fatalf("got %v", ev)
	}
	if ev := e.EvaluateAnalog("S_P_IN", 248.6, "PSI", good, 2); len(ev) != 0 { // 248.5+0.2=248.7
		t.Fatalf("deadband violated: %v", ev)
	}
	if ev := e.EvaluateAnalog("S_P_IN", 248.8, "PSI", good, 3); len(ev) != 1 {
		t.Fatalf("expected RTN: %v", ev)
	}
	ev = e.EvaluateAnalog("S_P_IN", 247.0, "PSI", good, 4)
	if len(ev) != 2 { // L and LL
		t.Fatalf("expected L+LL, got %v", ev)
	}
}

func TestDigitalLeak(t *testing.T) {
	e := newTestEngine(t)
	if ev := e.EvaluateDigital("S_LEAK", false, good, 1); len(ev) != 0 {
		t.Fatal("no alarm expected")
	}
	ev := e.EvaluateDigital("S_LEAK", true, good, 2)
	if len(ev) != 1 || ev[0].Severity != scadav1.AlarmSeverity_SEVERITY_CRITICAL || ev[0].AlarmId != "S_LEAK_ALM" {
		t.Fatalf("got %v", ev)
	}
	if ev := e.EvaluateDigital("S_LEAK", true, good, 3); len(ev) != 0 {
		t.Fatal("duplicate raise")
	}
	e.Acknowledge("S_LEAK_ALM", "op")
	ev = e.EvaluateDigital("S_LEAK", false, good, 4)
	if len(ev) != 1 || ev[0].State != scadav1.AlarmState_ALARM_ACK_RTN {
		t.Fatalf("got %v", ev)
	}
}

func TestBadQualityIgnored(t *testing.T) {
	e := newTestEngine(t)
	e.EvaluateAnalog("S_P_IN", 250.6, "PSI", good, 1)
	if ev := e.EvaluateAnalog("S_P_IN", 0, "PSI", scadav1.DataQuality_QUALITY_BAD_COMM_FAILURE, 2); len(ev) != 0 {
		t.Fatalf("bad quality must not clear alarms: %v", ev)
	}
	if len(e.ActiveAlarms("", 0)) != 1 {
		t.Fatal("alarm lost")
	}
}

func TestAckErrorsAndAckAll(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.Acknowledge("nope", "u"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	e.EvaluateAnalog("S_P_IN", 251.5, "PSI", good, 1)
	e.EvaluateDigital("S_LEAK", true, good, 1)
	if evs := e.AcknowledgeAll("op"); len(evs) != 3 {
		t.Fatalf("ack all: %d", len(evs))
	}
}

func TestSinkOrderAndRuleValidation(t *testing.T) {
	e := newTestEngine(t)
	var mu sync.Mutex
	var states []scadav1.AlarmState
	e.SetSink(func(ev *scadav1.AlarmEvent) { mu.Lock(); states = append(states, ev.State); mu.Unlock() })
	e.EvaluateAnalog("S_P_IN", 250.6, "PSI", good, 1)
	e.Acknowledge("S_P_IN_H", "u")
	e.EvaluateAnalog("S_P_IN", 249.0, "PSI", good, 2)
	want := []scadav1.AlarmState{scadav1.AlarmState_ALARM_UNACK_ALARM, scadav1.AlarmState_ALARM_ACK_ALARM, scadav1.AlarmState_ALARM_ACK_RTN}
	if len(states) != 3 || states[0] != want[0] || states[1] != want[1] || states[2] != want[2] {
		t.Fatalf("sink order %v", states)
	}
	bad := AnalogRule{TagID: "X", HH: &Limit{Value: 1}, H: &Limit{Value: 2}}
	if err := e.AddAnalogRule(bad); err == nil {
		t.Fatal("HH<H must be rejected")
	}
}

func TestConcurrentUse(t *testing.T) {
	e := newTestEngine(t)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				v := 249.0
				if i%2 == 0 {
					v = 251.5
				}
				e.EvaluateAnalog("S_P_IN", v, "PSI", good, int64(i))
				e.ActiveAlarms("", 0)
				e.AcknowledgeAll("u")
			}
		}(w)
	}
	wg.Wait()
}
