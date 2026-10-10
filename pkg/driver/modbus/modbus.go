// Package modbus implements a Modbus TCP polling driver that turns raw
// register/discrete-input reads into SCADA telemetry protobuf messages.
package modbus

import (
	"errors"
	"fmt"
	"sync"
	"time"

	mb "github.com/simonvetter/modbus"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

// AnalogPoint maps one holding register to an analog tag.
type AnalogPoint struct {
	TagID    string
	Register uint16
	Unit     string
	Scale    float64 // engineering = raw / Scale
	EngMin   float64 // below this the value is flagged QUALITY_BAD_OUT_OF_RANGE
	EngMax   float64 // above this the value is flagged QUALITY_BAD_OUT_OF_RANGE
}

// DigitalPoint maps one discrete input to a digital tag.
type DigitalPoint struct {
	TagID   string
	Address uint16
}

// Config configures a Driver.
type Config struct {
	Address  string // host:port of the Modbus TCP server
	UnitID   uint8
	Timeout  time.Duration
	Analogs  []AnalogPoint
	Digitals []DigitalPoint
}

// DefaultStationPoints returns the tag map of the demo city gate station.
func DefaultStationPoints(station string) ([]AnalogPoint, []DigitalPoint) {
	analogs := []AnalogPoint{
		{TagID: station + "_P_IN", Register: 0, Unit: "PSI", Scale: 10, EngMin: 0, EngMax: 1500},
		{TagID: station + "_P_OUT", Register: 1, Unit: "PSI", Scale: 10, EngMin: 0, EngMax: 500},
		{TagID: station + "_TEMP", Register: 2, Unit: "°C", Scale: 10, EngMin: -40, EngMax: 100},
	}
	digitals := []DigitalPoint{
		{TagID: station + "_LEAK", Address: 0},
	}
	return analogs, digitals
}

// Driver polls a Modbus TCP device. It is safe for use by a single polling
// goroutine at a time; Close may be called concurrently.
type Driver struct {
	cfg Config

	mu        sync.Mutex
	client    *mb.ModbusClient
	connected bool
	lastDig   map[string]bool // previous digital states for SOE detection
	span      uint16          // number of holding registers to read
}

// New validates cfg and builds a Driver. No connection is made until Poll.
func New(cfg Config) (*Driver, error) {
	if cfg.Address == "" {
		return nil, errors.New("modbus: empty address")
	}
	if len(cfg.Analogs) == 0 && len(cfg.Digitals) == 0 {
		return nil, errors.New("modbus: no points configured")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	for _, a := range cfg.Analogs {
		if a.Scale == 0 {
			return nil, fmt.Errorf("modbus: analog point %q has zero scale", a.TagID)
		}
	}
	return &Driver{cfg: cfg, lastDig: make(map[string]bool), span: cfg.analogSpan()}, nil
}

func (c Config) analogSpan() uint16 {
	var span uint16
	for _, a := range c.Analogs {
		if a.Register+1 > span {
			span = a.Register + 1
		}
	}
	return span
}

func (c Config) digitalSpan() uint16 {
	var span uint16
	for _, p := range c.Digitals {
		if p.Address+1 > span {
			span = p.Address + 1
		}
	}
	return span
}

func (d *Driver) connect() error {
	if d.connected {
		return nil
	}
	cl, err := mb.NewClient(&mb.ClientConfiguration{
		URL:     "tcp://" + d.cfg.Address,
		Timeout: d.cfg.Timeout,
	})
	if err != nil {
		return err
	}
	if err := cl.Open(); err != nil {
		return err
	}
	if err := cl.SetUnitId(d.cfg.UnitID); err != nil {
		_ = cl.Close()
		return err
	}
	d.client = cl
	d.connected = true
	return nil
}

func (d *Driver) disconnect() {
	if d.client != nil {
		_ = d.client.Close()
	}
	d.client = nil
	d.connected = false
}

// Close releases the connection.
func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disconnect()
	return nil
}

// Poll performs one read cycle and returns the analog and digital values.
// On communication failure the error is returned together with values for
// every configured tag flagged QUALITY_BAD_COMM_FAILURE (last digital states
// are kept), so the caller can still publish the loss of quality upstream.
func (d *Driver) Poll() ([]*scadav1.AnalogTagValue, []*scadav1.DigitalTagValue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().UTC().UnixMilli()

	pollErr := d.readAndConvert(now)
	if pollErr.err != nil {
		d.disconnect()
		return d.badSnapshot(now), d.badDigitals(now), pollErr.err
	}
	return pollErr.analogs, pollErr.digitals, nil
}

type pollResult struct {
	analogs  []*scadav1.AnalogTagValue
	digitals []*scadav1.DigitalTagValue
	err      error
}

func (d *Driver) readAndConvert(now int64) pollResult {
	if err := d.connect(); err != nil {
		return pollResult{err: fmt.Errorf("connect %s: %w", d.cfg.Address, err)}
	}

	var regs []uint16
	if d.span > 0 {
		var err error
		regs, err = d.client.ReadRegisters(0, d.span, mb.HOLDING_REGISTER)
		if err != nil {
			return pollResult{err: fmt.Errorf("read holding registers: %w", err)}
		}
	}
	var bits []bool
	if n := d.cfg.digitalSpan(); n > 0 {
		var err error
		bits, err = d.client.ReadDiscreteInputs(0, n)
		if err != nil {
			return pollResult{err: fmt.Errorf("read discrete inputs: %w", err)}
		}
	}

	analogs := make([]*scadav1.AnalogTagValue, 0, len(d.cfg.Analogs))
	for _, a := range d.cfg.Analogs {
		raw := regs[a.Register]
		eng := float64(raw) / a.Scale
		q := scadav1.DataQuality_QUALITY_GOOD
		if eng < a.EngMin || eng > a.EngMax {
			q = scadav1.DataQuality_QUALITY_BAD_OUT_OF_RANGE
		}
		analogs = append(analogs, &scadav1.AnalogTagValue{
			TagId:           a.TagID,
			Value:           eng,
			RawValue:        float64(raw),
			EngineeringUnit: a.Unit,
			Quality:         q,
			TimestampUtcMs:  now,
		})
	}

	digitals := make([]*scadav1.DigitalTagValue, 0, len(d.cfg.Digitals))
	for _, p := range d.cfg.Digitals {
		st := bits[p.Address]
		prev, seen := d.lastDig[p.TagID]
		d.lastDig[p.TagID] = st
		digitals = append(digitals, &scadav1.DigitalTagValue{
			TagId:          p.TagID,
			State:          st,
			Quality:        scadav1.DataQuality_QUALITY_GOOD,
			TimestampUtcMs: now,
			IsSoe:          seen && prev != st,
		})
	}
	return pollResult{analogs: analogs, digitals: digitals}
}

func (d *Driver) badSnapshot(now int64) []*scadav1.AnalogTagValue {
	out := make([]*scadav1.AnalogTagValue, 0, len(d.cfg.Analogs))
	for _, a := range d.cfg.Analogs {
		out = append(out, &scadav1.AnalogTagValue{
			TagId:           a.TagID,
			EngineeringUnit: a.Unit,
			Quality:         scadav1.DataQuality_QUALITY_BAD_COMM_FAILURE,
			TimestampUtcMs:  now,
		})
	}
	return out
}

func (d *Driver) badDigitals(now int64) []*scadav1.DigitalTagValue {
	out := make([]*scadav1.DigitalTagValue, 0, len(d.cfg.Digitals))
	for _, p := range d.cfg.Digitals {
		out = append(out, &scadav1.DigitalTagValue{
			TagId:          p.TagID,
			State:          d.lastDig[p.TagID],
			Quality:        scadav1.DataQuality_QUALITY_BAD_COMM_FAILURE,
			TimestampUtcMs: now,
		})
	}
	return out
}
