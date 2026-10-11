package modbus

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	mb "github.com/simonvetter/modbus"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/command"
)

const holdingRegister = mb.HOLDING_REGISTER

// isDeviceException reports whether err is a Modbus exception response, which
// means the connection itself is healthy.
func isDeviceException(err error) bool {
	for _, e := range []error{mb.ErrIllegalFunction, mb.ErrIllegalDataAddress, mb.ErrIllegalDataValue, mb.ErrServerDeviceFailure,
		mb.ErrAcknowledge, mb.ErrServerDeviceBusy, mb.ErrMemoryParityError, mb.ErrGWPathUnavailable, mb.ErrGWTargetFailedToRespond} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// WriteKind selects the Modbus function used to execute an action.
type WriteKind int

const (
	// WriteCoil uses function code 05 (Write Single Coil).
	WriteCoil WriteKind = iota
	// WriteRegister uses function code 06 (Write Single Register).
	WriteRegister
)

// CommandAction describes how one action on one tag is written to the device.
type CommandAction struct {
	Kind    WriteKind
	Address uint16
	Coil    bool    // WriteCoil: the value written (true = ON)
	Scale   float64 // WriteRegister: raw = round(setpoint * Scale)
}

// CommandPoint lists the actions a tag accepts.
type CommandPoint struct {
	TagID   string
	Actions map[string]CommandAction
	Min     float64 // CMD_SET_SETPOINT limits in engineering units
	Max     float64
	Unit    string
}

// DefaultCommandPoints returns the command map of the demo station / simulator:
//
//	coil 10 INLET_VLV   (true = open)            CMD_OPEN_VALVE / CMD_CLOSE_VALVE
//	coil 11 SLAM_SHUT   (true = re-arm)          CMD_RESET_TRIP
//	coil 12 OUTLET_VLV  (true = open)            CMD_OPEN_VALVE / CMD_CLOSE_VALVE
//	holding register 3  P_OUT_SP (PSI x 10)      CMD_SET_SETPOINT, 50.0 .. 70.0
func DefaultCommandPoints(station string) []CommandPoint {
	valve := func(tag string, addr uint16) CommandPoint {
		return CommandPoint{TagID: tag, Actions: map[string]CommandAction{
			command.ActionOpenValve:  {Kind: WriteCoil, Address: addr, Coil: true},
			command.ActionCloseValve: {Kind: WriteCoil, Address: addr, Coil: false},
		}}
	}
	return []CommandPoint{
		valve(station+"_INLET_VLV", 10),
		valve(station+"_OUTLET_VLV", 12),
		{TagID: station + "_SLAM_SHUT", Actions: map[string]CommandAction{
			command.ActionResetTrip: {Kind: WriteCoil, Address: 11, Coil: true},
		}},
		{TagID: station + "_P_OUT_SP", Unit: "PSI", Min: 50, Max: 70, Actions: map[string]CommandAction{
			command.ActionSetSetpoint: {Kind: WriteRegister, Address: 3, Scale: 10},
		}},
	}
}

// Capabilities advertises what this driver can execute (sorted by tag id).
func (d *Driver) Capabilities() []*scadav1.CommandCapability {
	out := make([]*scadav1.CommandCapability, 0, len(d.cfg.Commands))
	for _, p := range d.cfg.Commands {
		actions := make([]string, 0, len(p.Actions))
		for a := range p.Actions {
			actions = append(actions, a)
		}
		sort.Strings(actions)
		out = append(out, &scadav1.CommandCapability{TagId: p.TagID, Actions: actions, MinValue: p.Min, MaxValue: p.Max, Unit: p.Unit})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TagId < out[j].TagId })
	return out
}

func (d *Driver) findAction(tag, action string) (*CommandPoint, CommandAction, bool) {
	for i := range d.cfg.Commands {
		if d.cfg.Commands[i].TagID == tag {
			a, ok := d.cfg.Commands[i].Actions[action]
			return &d.cfg.Commands[i], a, ok
		}
	}
	return nil, CommandAction{}, false
}

func failAck(id, msg string) *scadav1.CommandAck {
	return &scadav1.CommandAck{CommandId: id, Success: false, Status: command.StatusFailed, Message: msg, ExecutedTimeUtcMs: time.Now().UTC().UnixMilli()}
}

// Execute writes the command to the device and verifies it by reading the
// target back. The returned ack always describes the outcome; success is only
// reported when the device accepted the write AND the read-back matches.
func (d *Driver) Execute(cmd *scadav1.ControlCommand) *scadav1.CommandAck {
	id := cmd.GetCommandId()
	if err := command.Validate(cmd); err != nil {
		return failAck(id, "invalid command: "+err.Error())
	}
	point, act, ok := d.findAction(cmd.GetTargetTagId(), cmd.GetAction())
	if !ok {
		return failAck(id, fmt.Sprintf("%s does not support %s on this device", cmd.GetTargetTagId(), cmd.GetAction()))
	}

	var raw uint16
	if act.Kind == WriteRegister {
		sp := cmd.GetSetpointValue()
		if sp < point.Min || sp > point.Max {
			return failAck(id, fmt.Sprintf("setpoint %.2f is outside %.2f..%.2f %s", sp, point.Min, point.Max, point.Unit))
		}
		v := math.Round(sp * act.Scale)
		if v < 0 || v > math.MaxUint16 {
			return failAck(id, "setpoint does not fit the device register")
		}
		raw = uint16(v)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.connect(); err != nil {
		return failAck(id, fmt.Sprintf("device unreachable (%s): %v - nothing was written", d.cfg.Address, err))
	}

	var msg string
	var err error
	switch act.Kind {
	case WriteCoil:
		if err = d.client.WriteCoil(act.Address, act.Coil); err != nil {
			err = fmt.Errorf("FC05 write coil %d: %w", act.Address, err)
			break
		}
		var back bool
		if back, err = d.client.ReadCoil(act.Address); err != nil {
			err = fmt.Errorf("FC05 write coil %d succeeded but the read-back failed: %w", act.Address, err)
			break
		}
		if back != act.Coil {
			err = fmt.Errorf("FC05 write coil %d was acknowledged but reads back %t, expected %t", act.Address, back, act.Coil)
			break
		}
		msg = fmt.Sprintf("FC05 write coil %d = %t confirmed by read-back", act.Address, act.Coil)
	case WriteRegister:
		if err = d.writeHolding(act.Address, []uint16{raw}); err != nil {
			err = fmt.Errorf("write holding register %d: %w", act.Address, err)
			break
		}
		var back uint16
		if back, err = d.client.ReadRegister(act.Address, holdingRegister); err != nil {
			err = fmt.Errorf("write holding register %d succeeded but the read-back failed: %w", act.Address, err)
			break
		}
		if back != raw {
			err = fmt.Errorf("write holding register %d was acknowledged but reads back %d, expected %d", act.Address, back, raw)
			break
		}
		msg = fmt.Sprintf("FC06 write register %d = %d (%.2f %s) confirmed by read-back", act.Address, raw, cmd.GetSetpointValue(), point.Unit)
	}
	if err != nil {
		if !isDeviceException(err) { // transport problem: reconnect on the next use
			d.disconnect()
		}
		return failAck(id, err.Error())
	}
	return &scadav1.CommandAck{CommandId: id, Success: true, Status: command.StatusExecuted, Message: msg, ExecutedTimeUtcMs: time.Now().UTC().UnixMilli()}
}

// writeHolding writes consecutive holding registers: function code 06 for a
// single register, function code 16 for several. Caller holds d.mu.
func (d *Driver) writeHolding(addr uint16, vals []uint16) error {
	switch len(vals) {
	case 0:
		return errors.New("no values")
	case 1:
		return d.client.WriteRegister(addr, vals[0])
	default:
		return d.client.WriteRegisters(addr, vals)
	}
}
