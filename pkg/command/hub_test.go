package command

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

func caps() []*scadav1.CommandCapability {
	return []*scadav1.CommandCapability{
		{TagId: "ST_INLET_VLV", Actions: []string{ActionOpenValve, ActionCloseValve}},
		{TagId: "ST_P_OUT_SP", Actions: []string{ActionSetSetpoint}, MinValue: 50, MaxValue: 70, Unit: "PSI"},
	}
}

func cmd(id, tag, action string) *scadav1.ControlCommand {
	return &scadav1.ControlCommand{CommandId: id, TargetStationId: "ST", TargetTagId: tag, Action: action, InitiatedByUser: "alice", TimestampUtcMs: 1}
}

// fakeFEP answers every command it receives with the given ack factory.
func fakeFEP(t *testing.T, h *Hub, reply func(*scadav1.ControlCommand) *scadav1.CommandAck) (*Session, *sync.WaitGroup) {
	t.Helper()
	s, _ := h.Register("FEP1", "ST", caps())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case c := <-s.Out:
				if r := reply(c); r != nil {
					h.Report(r)
				}
			case <-s.Done():
				return
			}
		}
	}()
	return s, &wg
}

func okReply(c *scadav1.ControlCommand) *scadav1.CommandAck {
	return &scadav1.CommandAck{CommandId: c.CommandId, Success: true, Message: "done", ExecutedTimeUtcMs: 5}
}

func TestValidate(t *testing.T) {
	bad := map[string]*scadav1.ControlCommand{
		"short id":    {CommandId: "x", TargetStationId: "ST", TargetTagId: "T", Action: ActionOpenValve, InitiatedByUser: "a"},
		"bad station": {CommandId: "cmd-00001", TargetStationId: "S T", TargetTagId: "T", Action: ActionOpenValve, InitiatedByUser: "a"},
		"bad tag":     {CommandId: "cmd-00001", TargetStationId: "ST", TargetTagId: "T;x", Action: ActionOpenValve, InitiatedByUser: "a"},
		"bad action":  {CommandId: "cmd-00001", TargetStationId: "ST", TargetTagId: "T", Action: "CMD_EXPLODE", InitiatedByUser: "a"},
		"no user":     {CommandId: "cmd-00001", TargetStationId: "ST", TargetTagId: "T", Action: ActionOpenValve},
		"stray sp":    {CommandId: "cmd-00001", TargetStationId: "ST", TargetTagId: "T", Action: ActionOpenValve, InitiatedByUser: "a", SetpointValue: 5},
	}
	for name, c := range bad {
		if Validate(c) == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	if err := Validate(cmd("cmd-00001", "T", ActionOpenValve)); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchSuccessAndRestamp(t *testing.T) {
	h := NewHub(2 * time.Second)
	var got *scadav1.ControlCommand
	var mu sync.Mutex
	s, wg := fakeFEP(t, h, func(c *scadav1.ControlCommand) *scadav1.CommandAck {
		mu.Lock()
		got = c
		mu.Unlock()
		return okReply(c)
	})
	a := h.Dispatch(context.Background(), cmd("cmd-00001", "ST_INLET_VLV", ActionCloseValve))
	if !a.Success || a.Status != StatusExecuted || a.Message != "done" {
		t.Fatalf("ack: %+v", a)
	}
	mu.Lock()
	if got.TimestampUtcMs <= 1 || got.Action != ActionCloseValve {
		t.Fatalf("FEP received %+v", got)
	}
	mu.Unlock()
	h.Unregister(s)
	wg.Wait()
}

func TestRejections(t *testing.T) {
	h := NewHub(time.Second)
	if a := h.Dispatch(context.Background(), cmd("cmd-00001", "ST_INLET_VLV", ActionOpenValve)); a.Status != StatusNoFEP || a.Success {
		t.Fatalf("no FEP: %+v", a)
	}
	s, wg := fakeFEP(t, h, okReply)
	defer func() { h.Unregister(s); wg.Wait() }()
	if a := h.Dispatch(context.Background(), cmd("cmd-00001", "ST_INLET_VLV", ActionOpenValve)); !a.Success {
		t.Fatalf("a resend after the FEP connected must work (NO_FEP is not sticky): %+v", a)
	}
	cases := []struct {
		c      *scadav1.ControlCommand
		status string
		sub    string
	}{
		{cmd("cmd-00002", "ST_UNKNOWN", ActionOpenValve), StatusRejected, "does not support"},
		{cmd("cmd-00003", "ST_INLET_VLV", ActionResetTrip), StatusRejected, "does not support"},
		{func() *scadav1.ControlCommand {
			c := cmd("cmd-00004", "ST_P_OUT_SP", ActionSetSetpoint)
			c.SetpointValue = 80
			return c
		}(), StatusRejected, "outside the allowed range"},
		{func() *scadav1.ControlCommand {
			c := cmd("cmd-00005", "ST_P_OUT_SP", ActionSetSetpoint)
			c.SetpointValue = 49.9
			return c
		}(), StatusRejected, "outside the allowed range"},
		{cmd("x", "ST_INLET_VLV", ActionOpenValve), StatusRejected, "command_id"},
	}
	for _, c := range cases {
		a := h.Dispatch(context.Background(), c.c)
		if a.Success || a.Status != c.status || !strings.Contains(a.Message, c.sub) {
			t.Errorf("%s: %+v", c.c.CommandId, a)
		}
	}
	ok := cmd("cmd-00006", "ST_P_OUT_SP", ActionSetSetpoint)
	ok.SetpointValue = 62.5
	if a := h.Dispatch(context.Background(), ok); !a.Success {
		t.Fatalf("in-range setpoint: %+v", a)
	}
}

func TestIdempotentResend(t *testing.T) {
	h := NewHub(2 * time.Second)
	var n int
	var mu sync.Mutex
	s, wg := fakeFEP(t, h, func(c *scadav1.ControlCommand) *scadav1.CommandAck { mu.Lock(); n++; mu.Unlock(); return okReply(c) })
	defer func() { h.Unregister(s); wg.Wait() }()
	c := cmd("cmd-00001", "ST_INLET_VLV", ActionOpenValve)
	a1 := h.Dispatch(context.Background(), c)
	a2 := h.Dispatch(context.Background(), c)
	mu.Lock()
	defer mu.Unlock()
	if n != 1 || !a1.Success || !a2.Success || a1.Message != a2.Message {
		t.Fatalf("executed %d times, acks %+v %+v", n, a1, a2)
	}
}

func TestSingleCommandPerTagAndSharedResend(t *testing.T) {
	h := NewHub(3 * time.Second)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	s, _ := h.Register("F", "ST", caps())
	go func() {
		for c := range s.Out {
			started <- struct{}{}
			<-release
			h.Report(okReply(c))
		}
	}()
	first := make(chan *scadav1.CommandAck, 1)
	go func() { first <- h.Dispatch(context.Background(), cmd("cmd-00001", "ST_INLET_VLV", ActionOpenValve)) }()
	<-started
	// different command on the same tag while the first is in flight
	if a := h.Dispatch(context.Background(), cmd("cmd-00002", "ST_INLET_VLV", ActionCloseValve)); a.Status != StatusRejected || !strings.Contains(a.Message, "in progress") {
		t.Fatalf("concurrent command: %+v", a)
	}
	// resend of the in-flight command shares its outcome
	resend := make(chan *scadav1.CommandAck, 1)
	go func() { resend <- h.Dispatch(context.Background(), cmd("cmd-00001", "ST_INLET_VLV", ActionOpenValve)) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if a := <-first; !a.Success {
		t.Fatalf("first: %+v", a)
	}
	if a := <-resend; !a.Success {
		t.Fatalf("resend: %+v", a)
	}
	// the tag is free again
	if a := h.Dispatch(context.Background(), cmd("cmd-00003", "ST_INLET_VLV", ActionCloseValve)); !a.Success {
		t.Fatalf("after completion: %+v", a)
	}
}

func TestTimeoutThenLateResult(t *testing.T) {
	h := NewHub(150 * time.Millisecond)
	s, _ := h.Register("F", "ST", caps())
	defer h.Unregister(s)
	c := cmd("cmd-00001", "ST_INLET_VLV", ActionOpenValve)
	a := h.Dispatch(context.Background(), c)
	if a.Success || a.Status != StatusTimeout || !strings.Contains(a.Message, "may or may not") {
		t.Fatalf("timeout ack: %+v", a)
	}
	<-s.Out // the FEP finally reads it and reports late
	if !h.Report(&scadav1.CommandAck{CommandId: "cmd-00001", Success: true, Message: "ok"}) {
		t.Fatal("late report must be accepted")
	}
	b := h.Dispatch(context.Background(), c) // a resend now returns the truth, and does not execute again
	if !b.Success || b.Status != StatusExecuted || !strings.HasPrefix(b.Message, "late result") {
		t.Fatalf("resend after late result: %+v", b)
	}
	select {
	case <-s.Out:
		t.Fatal("the command must not be sent twice")
	default:
	}
	if h.Report(&scadav1.CommandAck{CommandId: "unknown-id-1", Success: true}) {
		t.Fatal("unknown ids must be refused")
	}
}

func TestSessionReplacedAndDisconnect(t *testing.T) {
	h := NewHub(time.Second)
	s1, _ := h.Register("F1", "ST", caps())
	s2, old := h.Register("F2", "ST", caps())
	if old != s1 {
		t.Fatal("old session not returned")
	}
	select {
	case <-s1.Done():
	default:
		t.Fatal("replaced session must be closed")
	}
	h.Unregister(s1) // must not remove the new session
	if cs := h.Capabilities("ST"); len(cs) != 1 || cs[0].FepId != "F2" {
		t.Fatalf("capabilities after replace: %+v", cs)
	}
	// FEP disconnects while a command is in flight
	go func() { <-s2.Out; h.Unregister(s2) }()
	a := h.Dispatch(context.Background(), cmd("cmd-00001", "ST_INLET_VLV", ActionOpenValve))
	if a.Success || a.Status != StatusTimeout || !strings.Contains(a.Message, "disconnected") {
		t.Fatalf("disconnect ack: %+v", a)
	}
	if len(h.Capabilities("")) != 0 {
		t.Fatal("no sessions should remain")
	}
}

func TestQueueFull(t *testing.T) {
	h := NewHub(time.Second)
	s, _ := h.Register("F", "ST", caps())
	defer h.Unregister(s)
	for i := 0; i < queueSize; i++ {
		s.Out <- &scadav1.ControlCommand{}
	}
	if a := h.Dispatch(context.Background(), cmd("cmd-00001", "ST_INLET_VLV", ActionOpenValve)); a.Status != StatusFailed || !strings.Contains(a.Message, "queue is full") {
		t.Fatalf("queue full: %+v", a)
	}
}
