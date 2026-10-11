package main

import (
	"errors"
	"math/rand"
	"testing"

	mb "github.com/simonvetter/modbus"
)

func coilWrite(addr uint16, vals ...bool) *mb.CoilsRequest {
	return &mb.CoilsRequest{Addr: addr, Quantity: uint16(len(vals)), IsWrite: true, Args: vals, ClientAddr: "test"}
}

func TestValveCoilsApplyImmediately(t *testing.T) {
	s := newStation(1, false)
	if _, err := s.HandleCoils(coilWrite(coilInlet, false)); err != nil {
		t.Fatal(err)
	}
	if s.inlet {
		t.Fatal("inlet valve must be closed")
	}
	got, err := s.HandleCoils(&mb.CoilsRequest{Addr: coilInlet, Quantity: 1})
	if err != nil || got[0] {
		t.Fatalf("read-back of coil 10: %v %v", got, err)
	}
	disc, _ := s.HandleDiscreteInputs(&mb.DiscreteInputsRequest{Addr: 1, Quantity: 1})
	if disc[0] {
		t.Fatal("discrete input 1 must show the closed valve")
	}
	if _, err := s.HandleCoils(coilWrite(coilOutlet, false)); err != nil || s.outlet {
		t.Fatalf("outlet: %v %v", s.outlet, err)
	}
	if _, err := s.HandleCoils(coilWrite(coilInlet, true)); err != nil || !s.inlet {
		t.Fatal("inlet must reopen")
	}
}

func TestSlamShutOnlyResets(t *testing.T) {
	s := newStation(1, false)
	s.slam = false // tripped
	if _, err := s.HandleCoils(coilWrite(coilSlam, false)); !errors.Is(err, mb.ErrIllegalDataValue) {
		t.Fatalf("tripping by command must be refused, got %v", err)
	}
	if _, err := s.HandleCoils(coilWrite(coilSlam, true)); err != nil || !s.slam {
		t.Fatalf("reset: %v %v", s.slam, err)
	}
	if _, err := s.HandleCoils(coilWrite(coilSlam, true)); err != nil { // already armed: harmless
		t.Fatalf("second reset: %v", err)
	}
}

func TestCoilWriteValidation(t *testing.T) {
	s := newStation(1, false)
	if _, err := s.HandleCoils(coilWrite(0, true)); !errors.Is(err, mb.ErrIllegalDataAddress) {
		t.Fatalf("leak coil is read-only: %v", err)
	}
	if _, err := s.HandleCoils(coilWrite(99, true)); !errors.Is(err, mb.ErrIllegalDataAddress) {
		t.Fatalf("unknown coil: %v", err)
	}
	// FC15 spanning inlet(10), slam(11 <- false is refused): nothing may be applied
	if _, err := s.HandleCoils(coilWrite(coilInlet, false, false)); err == nil {
		t.Fatal("expected refusal")
	}
	if !s.inlet {
		t.Fatal("a refused multi-coil write must not change anything")
	}
	if _, err := s.HandleCoils(&mb.CoilsRequest{Addr: 5, Quantity: 1}); !errors.Is(err, mb.ErrIllegalDataAddress) {
		t.Fatalf("read of a hole: %v", err)
	}
}

func TestSetpointRegister(t *testing.T) {
	s := newStation(1, false)
	w := func(addr uint16, v uint16) error {
		_, err := s.HandleHoldingRegisters(&mb.HoldingRegistersRequest{Addr: addr, Quantity: 1, IsWrite: true, Args: []uint16{v}, ClientAddr: "test"})
		return err
	}
	if err := w(regSetpt, 625); err != nil || s.setpoint != 625 {
		t.Fatalf("setpoint write: %v %d", err, s.setpoint)
	}
	r, _ := s.HandleHoldingRegisters(&mb.HoldingRegistersRequest{Addr: regSetpt, Quantity: 1})
	if r[0] != 625 {
		t.Fatalf("setpoint read-back: %v", r)
	}
	for _, v := range []uint16{499, 701, 0, 65535} {
		if err := w(regSetpt, v); !errors.Is(err, mb.ErrIllegalDataValue) {
			t.Fatalf("setpoint %d must be refused: %v", v, err)
		}
	}
	if s.setpoint != 625 {
		t.Fatal("a refused write must not change the setpoint")
	}
	if err := w(0, 100); !errors.Is(err, mb.ErrIllegalDataAddress) {
		t.Fatalf("measurement registers are read-only: %v", err)
	}
}

func TestProcessFollowsSetpointAndSupply(t *testing.T) {
	s := newStation(0, false)
	rng := rand.New(rand.NewSource(1))
	run := func(n int) {
		for i := 0; i < n; i++ {
			s.step(float64(i)/2, rng)
		}
	}
	run(60)
	if s.holding[1] < 560 || s.holding[1] > 640 || s.holding[0] < 2400 {
		t.Fatalf("normal operation: pIn=%d pOut=%d", s.holding[0], s.holding[1])
	}
	s.setpoint = 650
	run(60)
	if s.holding[1] < 610 || s.holding[1] > 690 {
		t.Fatalf("outlet pressure must follow the new setpoint, got %d", s.holding[1])
	}
	s.inlet = false
	run(80)
	if s.holding[0] > 100 || s.holding[1] > 100 {
		t.Fatalf("pressures must decay when the inlet valve is closed: pIn=%d pOut=%d", s.holding[0], s.holding[1])
	}
	s.inlet = true
	run(80)
	if s.holding[0] < 2400 || s.holding[1] < 600 {
		t.Fatalf("pressures must recover: pIn=%d pOut=%d", s.holding[0], s.holding[1])
	}
	s.slam = false
	run(80)
	if s.holding[1] > 100 || s.holding[0] < 2400 {
		t.Fatalf("a tripped slam-shut cuts only the outlet side: pIn=%d pOut=%d", s.holding[0], s.holding[1])
	}
}

func TestUnattendedModeRearmsSlamShut(t *testing.T) {
	s := newStation(0, true)
	s.slam = false
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 400 && !s.slam; i++ {
		s.step(float64(i)/2, rng)
	}
	if !s.slam {
		t.Fatal("unattended mode must re-arm the slam-shut")
	}
	manual := newStation(0, false)
	manual.slam = false
	for i := 0; i < 400; i++ {
		manual.step(float64(i)/2, rng)
	}
	if manual.slam {
		t.Fatal("in the default mode the slam-shut stays tripped until reset")
	}
}
