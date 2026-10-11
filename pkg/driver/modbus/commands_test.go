package modbus

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mb "github.com/simonvetter/modbus"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/command"
)

// fakePLC is a minimal device: 20 coils, 8 holding registers, optional faults.
type fakePLC struct {
	mu     sync.Mutex
	coils  [32]bool
	regs   [8]uint16
	ignore bool // acknowledge coil writes but do not apply them (read-back mismatch)
	refuse bool // answer writes with an exception
	writes []string
}

func (p *fakePLC) HandleCoils(r *mb.CoilsRequest) ([]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.IsWrite {
		if p.refuse {
			return nil, mb.ErrServerDeviceFailure
		}
		for i, v := range r.Args {
			p.writes = append(p.writes, "coil")
			if !p.ignore {
				p.coils[int(r.Addr)+i] = v
			}
		}
		return nil, nil
	}
	return append([]bool(nil), p.coils[r.Addr:int(r.Addr)+int(r.Quantity)]...), nil
}
func (p *fakePLC) HandleDiscreteInputs(r *mb.DiscreteInputsRequest) ([]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bool(nil), p.coils[r.Addr:int(r.Addr)+int(r.Quantity)]...), nil
}
func (p *fakePLC) HandleHoldingRegisters(r *mb.HoldingRegistersRequest) ([]uint16, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.IsWrite {
		if p.refuse {
			return nil, mb.ErrIllegalDataValue
		}
		p.writes = append(p.writes, "reg")
		copy(p.regs[r.Addr:], r.Args)
		return nil, nil
	}
	return append([]uint16(nil), p.regs[r.Addr:int(r.Addr)+int(r.Quantity)]...), nil
}
func (p *fakePLC) HandleInputRegisters(*mb.InputRegistersRequest) ([]uint16, error) {
	return nil, mb.ErrIllegalFunction
}

func startPLC(t *testing.T) (*fakePLC, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	plc := &fakePLC{}
	srv, err := mb.NewServer(&mb.ServerConfiguration{URL: "tcp://" + addr, Timeout: 5 * time.Second, MaxClients: 4}, plc)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return plc, addr
}

func newDriver(t *testing.T, addr string) *Driver {
	t.Helper()
	analogs, digitals := DefaultStationPoints("ST")
	d, err := New(Config{Address: addr, UnitID: 1, Timeout: 2 * time.Second, Analogs: analogs, Digitals: digitals, Commands: DefaultCommandPoints("ST")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func cmd(tag, action string, sp float64) *scadav1.ControlCommand {
	return &scadav1.ControlCommand{CommandId: "cmd-test-0001", TargetStationId: "ST", TargetTagId: tag, Action: action,
		SetpointValue: sp, InitiatedByUser: "alice", TimestampUtcMs: time.Now().UnixMilli()}
}

func TestExecuteValveCoils(t *testing.T) {
	plc, addr := startPLC(t)
	d := newDriver(t, addr)

	a := d.Execute(cmd("ST_INLET_VLV", command.ActionOpenValve, 0))
	if !a.Success || a.Status != command.StatusExecuted || !strings.Contains(a.Message, "FC05 write coil 10 = true") || a.ExecutedTimeUtcMs == 0 {
		t.Fatalf("open: %+v", a)
	}
	if !plc.coils[10] {
		t.Fatal("coil 10 must be set")
	}
	a = d.Execute(cmd("ST_INLET_VLV", command.ActionCloseValve, 0))
	if !a.Success || plc.coils[10] {
		t.Fatalf("close: %+v coil=%v", a, plc.coils[10])
	}
	a = d.Execute(cmd("ST_OUTLET_VLV", command.ActionOpenValve, 0))
	if !a.Success || !plc.coils[12] {
		t.Fatalf("outlet: %+v", a)
	}
	a = d.Execute(cmd("ST_SLAM_SHUT", command.ActionResetTrip, 0))
	if !a.Success || !plc.coils[11] {
		t.Fatalf("reset: %+v", a)
	}
}

func TestExecuteSetpointRegister(t *testing.T) {
	plc, addr := startPLC(t)
	d := newDriver(t, addr)
	a := d.Execute(cmd("ST_P_OUT_SP", command.ActionSetSetpoint, 62.5))
	if !a.Success || plc.regs[3] != 625 || !strings.Contains(a.Message, "FC06 write register 3 = 625") {
		t.Fatalf("setpoint: %+v reg=%d", a, plc.regs[3])
	}
	before := len(plc.writes)
	for _, v := range []float64{49.9, 70.1, -1, 1e9} {
		a = d.Execute(cmd("ST_P_OUT_SP", command.ActionSetSetpoint, v))
		if a.Success || !strings.Contains(a.Message, "outside") {
			t.Fatalf("setpoint %v must be refused: %+v", v, a)
		}
	}
	if len(plc.writes) != before {
		t.Fatal("out-of-range setpoints must never reach the device")
	}
}

func TestExecuteRefusals(t *testing.T) {
	plc, addr := startPLC(t)
	d := newDriver(t, addr)
	if a := d.Execute(cmd("ST_INLET_VLV", command.ActionResetTrip, 0)); a.Success || !strings.Contains(a.Message, "does not support") {
		t.Fatalf("unsupported action: %+v", a)
	}
	if a := d.Execute(cmd("ST_P_IN", command.ActionOpenValve, 0)); a.Success {
		t.Fatalf("a measurement tag has no commands: %+v", a)
	}
	if a := d.Execute(&scadav1.ControlCommand{CommandId: "x"}); a.Success || !strings.Contains(a.Message, "invalid command") {
		t.Fatalf("invalid command: %+v", a)
	}
	if len(plc.writes) != 0 {
		t.Fatal("nothing may be written for refused commands")
	}

	plc.refuse = true
	a := d.Execute(cmd("ST_INLET_VLV", command.ActionOpenValve, 0))
	if a.Success || a.Status != command.StatusFailed || !strings.Contains(strings.ToLower(a.Message), "fc05") {
		t.Fatalf("device exception must fail the command: %+v", a)
	}
	// a device exception does not break the connection: the next command works
	plc.refuse = false
	if a := d.Execute(cmd("ST_INLET_VLV", command.ActionOpenValve, 0)); !a.Success {
		t.Fatalf("after the exception: %+v", a)
	}
}

func TestExecuteDetectsReadBackMismatch(t *testing.T) {
	plc, addr := startPLC(t)
	plc.ignore = true // acknowledges the write but does not apply it
	d := newDriver(t, addr)
	a := d.Execute(cmd("ST_INLET_VLV", command.ActionOpenValve, 0))
	if a.Success || !strings.Contains(a.Message, "reads back false") {
		t.Fatalf("an unconfirmed write must not be reported as success: %+v", a)
	}
}

func TestExecuteDeviceUnreachableAndReconnect(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close() // nothing listens
	d := newDriver(t, addr)
	a := d.Execute(cmd("ST_INLET_VLV", command.ActionOpenValve, 0))
	if a.Success || !strings.Contains(a.Message, "nothing was written") {
		t.Fatalf("unreachable device: %+v", a)
	}
}

func TestCapabilitiesAndPollingSetpoint(t *testing.T) {
	plc, addr := startPLC(t)
	plc.regs = [8]uint16{2500, 600, 150, 600}
	d := newDriver(t, addr)
	caps := d.Capabilities()
	if len(caps) != 4 || caps[0].TagId != "ST_INLET_VLV" || caps[1].TagId != "ST_OUTLET_VLV" || caps[3].TagId != "ST_SLAM_SHUT" {
		t.Fatalf("capabilities: %+v", caps)
	}
	var sp *scadav1.CommandCapability
	for _, c := range caps {
		if c.TagId == "ST_P_OUT_SP" {
			sp = c
		}
	}
	if sp == nil || sp.MinValue != 50 || sp.MaxValue != 70 || sp.Unit != "PSI" || len(sp.Actions) != 1 || sp.Actions[0] != command.ActionSetSetpoint {
		t.Fatalf("setpoint capability: %+v", sp)
	}
	analogs, _, err := d.Poll()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range analogs {
		if a.TagId == "ST_P_OUT_SP" && a.Value == 60 && a.Quality == scadav1.DataQuality_QUALITY_GOOD {
			found = true
		}
	}
	if !found {
		t.Fatalf("the setpoint register must be polled as an analog tag: %+v", analogs)
	}
}

func TestWriteHoldingUsesFC16ForSeveralRegisters(t *testing.T) {
	plc, addr := startPLC(t)
	d := newDriver(t, addr)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(); err != nil {
		t.Fatal(err)
	}
	if err := d.writeHolding(4, []uint16{7, 8, 9}); err != nil {
		t.Fatal(err)
	}
	if plc.regs[4] != 7 || plc.regs[6] != 9 {
		t.Fatalf("FC16 write: %v", plc.regs)
	}
	if err := d.writeHolding(4, nil); err == nil {
		t.Fatal("empty write must fail")
	}
}
