// Package alarm implements an ISA-18.2 / IEC 62682 style alarm evaluator.
//
// Analog tags support four limit stages (HH, H, L, LL) with deadband
// hysteresis; digital tags raise an alarm while they are in their active
// state. Every alarm follows the standard lifecycle:
//
//	(normal) --condition--> UNACK_ALARM --ack--> ACK_ALARM
//	ACK_ALARM --return to normal--> ACK_RTN (cleared)
//	UNACK_ALARM --return to normal--> UNACK_RTN --ack--> ACK_RTN (cleared)
//	UNACK_RTN --condition again--> UNACK_ALARM (re-alarm)
//
// Each transition produces an AlarmEvent. ACK_RTN is only ever seen as an
// event: the alarm leaves the active list at that moment.
package alarm

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

var (
	// ErrNotFound is returned when acknowledging an alarm that is not active.
	ErrNotFound = errors.New("alarm not found in active list")
	// ErrAlreadyAcknowledged is returned when an alarm needs no acknowledgement.
	ErrAlreadyAcknowledged = errors.New("alarm already acknowledged")
)

// Limit is one analog threshold.
type Limit struct {
	Value    float64
	Severity scadav1.AlarmSeverity
}

// AnalogRule configures the four-stage limit check of an analog tag. Nil
// limits are disabled. High limits trigger at value >= limit and clear below
// limit-Deadband; low limits trigger at value <= limit and clear above
// limit+Deadband.
type AnalogRule struct {
	TagID       string
	Description string // used in messages, e.g. "Inlet pressure"
	HH, H       *Limit
	L, LL       *Limit
	Deadband    float64
}

// DigitalRule raises an alarm while the tag is in ActiveState.
type DigitalRule struct {
	TagID       string
	AlarmSuffix string // alarm id = TagID + "_" + AlarmSuffix
	ActiveState bool
	Severity    scadav1.AlarmSeverity
	Message     string
}

type condition struct {
	alarmID  string
	tagID    string
	stage    string // "High-High", "High", "Low", "Low-Low"
	desc     string
	high     bool
	limit    float64
	severity scadav1.AlarmSeverity
	deadband float64
}

type digitalCondition struct {
	alarmID  string
	tagID    string
	active   bool
	severity scadav1.AlarmSeverity
	message  string
}

// Engine evaluates tag updates and tracks the active alarm list. It is safe
// for concurrent use.
type Engine struct {
	mu      sync.Mutex
	analog  map[string][]condition
	digital map[string][]digitalCondition
	active  map[string]*scadav1.AlarmEvent
	sink    func(*scadav1.AlarmEvent)
	nowMs   func() int64
	raised  uint64
	cleared uint64
	acked   uint64
}

// NewEngine creates an engine with no rules.
func NewEngine() *Engine {
	return &Engine{
		analog:  make(map[string][]condition),
		digital: make(map[string][]digitalCondition),
		active:  make(map[string]*scadav1.AlarmEvent),
		nowMs:   func() int64 { return time.Now().UTC().UnixMilli() },
	}
}

// SetSink registers a callback invoked for every alarm event. It is called
// while the engine lock is held, which guarantees events of one alarm are
// delivered in order; the callback must be fast and must not call the Engine.
func (e *Engine) SetSink(fn func(*scadav1.AlarmEvent)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sink = fn
}

// SetClock overrides the time source (used by tests).
func (e *Engine) SetClock(fn func() int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.nowMs = fn
}

// AddAnalogRule registers an analog rule and validates limit ordering.
func (e *Engine) AddAnalogRule(r AnalogRule) error {
	if r.TagID == "" {
		return errors.New("alarm: analog rule without tag id")
	}
	if r.Deadband < 0 {
		return fmt.Errorf("alarm: %s: negative deadband", r.TagID)
	}
	if r.HH != nil && r.H != nil && r.HH.Value < r.H.Value {
		return fmt.Errorf("alarm: %s: HH (%.2f) below H (%.2f)", r.TagID, r.HH.Value, r.H.Value)
	}
	if r.LL != nil && r.L != nil && r.LL.Value > r.L.Value {
		return fmt.Errorf("alarm: %s: LL (%.2f) above L (%.2f)", r.TagID, r.LL.Value, r.L.Value)
	}
	hi := r.H
	if hi == nil {
		hi = r.HH
	}
	lo := r.L
	if lo == nil {
		lo = r.LL
	}
	if hi != nil && lo != nil && hi.Value <= lo.Value {
		return fmt.Errorf("alarm: %s: high limit (%.2f) not above low limit (%.2f)", r.TagID, hi.Value, lo.Value)
	}
	desc := r.Description
	if desc == "" {
		desc = r.TagID
	}
	var conds []condition
	add := func(l *Limit, suffix, stage string, high bool) {
		if l == nil {
			return
		}
		conds = append(conds, condition{
			alarmID: r.TagID + "_" + suffix, tagID: r.TagID, stage: stage, desc: desc,
			high: high, limit: l.Value, severity: l.Severity, deadband: r.Deadband,
		})
	}
	add(r.HH, "HH", "High-High", true)
	add(r.H, "H", "High", true)
	add(r.L, "L", "Low", false)
	add(r.LL, "LL", "Low-Low", false)
	if len(conds) == 0 {
		return fmt.Errorf("alarm: %s: no limits configured", r.TagID)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.analog[r.TagID] = conds
	return nil
}

// AddDigitalRule registers a digital rule.
func (e *Engine) AddDigitalRule(r DigitalRule) error {
	if r.TagID == "" {
		return errors.New("alarm: digital rule without tag id")
	}
	suffix := r.AlarmSuffix
	if suffix == "" {
		suffix = "ALM"
	}
	msg := r.Message
	if msg == "" {
		msg = r.TagID + " active"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.digital[r.TagID] = append(e.digital[r.TagID], digitalCondition{
		alarmID: r.TagID + "_" + suffix, tagID: r.TagID, active: r.ActiveState,
		severity: r.Severity, message: msg,
	})
	return nil
}

// EvaluateAnalog evaluates an analog update and returns the resulting alarm
// events. Updates whose quality is not GOOD are ignored so that a
// communication failure neither raises nor clears alarms.
func (e *Engine) EvaluateAnalog(tagID string, value float64, unit string, q scadav1.DataQuality, tsMs int64) []*scadav1.AlarmEvent {
	if q != scadav1.DataQuality_QUALITY_GOOD {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	conds := e.analog[tagID]
	if len(conds) == 0 {
		return nil
	}
	var out []*scadav1.AlarmEvent
	for _, c := range conds {
		cur := e.active[c.alarmID]
		inAlarm := cur != nil && isActiveState(cur.GetState())
		switch {
		case !inAlarm && c.triggered(value):
			out = append(out, e.raise(c.alarmID, c.tagID, c.severity, c.message(value, unit), value, tsMs))
		case inAlarm && c.cleared(value):
			out = append(out, e.returnToNormal(cur, tsMs))
		}
	}
	return out
}

// EvaluateDigital evaluates a digital update and returns the resulting events.
func (e *Engine) EvaluateDigital(tagID string, state bool, q scadav1.DataQuality, tsMs int64) []*scadav1.AlarmEvent {
	if q != scadav1.DataQuality_QUALITY_GOOD {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	conds := e.digital[tagID]
	if len(conds) == 0 {
		return nil
	}
	val := 0.0
	if state {
		val = 1
	}
	var out []*scadav1.AlarmEvent
	for _, c := range conds {
		cur := e.active[c.alarmID]
		inAlarm := cur != nil && isActiveState(cur.GetState())
		switch {
		case !inAlarm && state == c.active:
			out = append(out, e.raise(c.alarmID, c.tagID, c.severity, c.message, val, tsMs))
		case inAlarm && state != c.active:
			out = append(out, e.returnToNormal(cur, tsMs))
		}
	}
	return out
}

// Acknowledge acknowledges one alarm on behalf of user.
func (e *Engine) Acknowledge(alarmID, user string) (*scadav1.AlarmEvent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.acknowledge(alarmID, user)
}

// AcknowledgeAll acknowledges every alarm that still needs it.
func (e *Engine) AcknowledgeAll(user string) []*scadav1.AlarmEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.active))
	for id, ev := range e.active {
		if s := ev.GetState(); s == scadav1.AlarmState_ALARM_UNACK_ALARM || s == scadav1.AlarmState_ALARM_UNACK_RTN {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var out []*scadav1.AlarmEvent
	for _, id := range ids {
		if ev, err := e.acknowledge(id, user); err == nil {
			out = append(out, ev)
		}
	}
	return out
}

// ActiveAlarms returns copies of the active alarms, most severe first, then
// oldest first. An empty tagID matches all tags.
func (e *Engine) ActiveAlarms(tagID string, minSeverity scadav1.AlarmSeverity) []*scadav1.AlarmEvent {
	e.mu.Lock()
	out := make([]*scadav1.AlarmEvent, 0, len(e.active))
	for _, ev := range e.active {
		if tagID != "" && ev.GetTagId() != tagID {
			continue
		}
		if ev.GetSeverity() < minSeverity {
			continue
		}
		out = append(out, proto.Clone(ev).(*scadav1.AlarmEvent))
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		if out[i].TriggerTimeUtcMs != out[j].TriggerTimeUtcMs {
			return out[i].TriggerTimeUtcMs < out[j].TriggerTimeUtcMs
		}
		return out[i].AlarmId < out[j].AlarmId
	})
	return out
}

// Stats reports lifetime counters.
func (e *Engine) Stats() (raised, acknowledged, cleared uint64, active int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.raised, e.acked, e.cleared, len(e.active)
}

// EventTimeMs returns the time at which the transition described by ev
// happened, for use as the history timestamp.
func EventTimeMs(ev *scadav1.AlarmEvent) int64 {
	switch ev.GetState() {
	case scadav1.AlarmState_ALARM_ACK_ALARM:
		return ev.GetAckTimeUtcMs()
	case scadav1.AlarmState_ALARM_UNACK_RTN:
		return ev.GetRtnTimeUtcMs()
	case scadav1.AlarmState_ALARM_ACK_RTN:
		if ev.GetAckTimeUtcMs() > ev.GetRtnTimeUtcMs() {
			return ev.GetAckTimeUtcMs()
		}
		return ev.GetRtnTimeUtcMs()
	default:
		return ev.GetTriggerTimeUtcMs()
	}
}

// --- internals (caller holds e.mu) ---

func isActiveState(s scadav1.AlarmState) bool {
	return s == scadav1.AlarmState_ALARM_UNACK_ALARM || s == scadav1.AlarmState_ALARM_ACK_ALARM
}

func (c condition) triggered(v float64) bool {
	if c.high {
		return v >= c.limit
	}
	return v <= c.limit
}

func (c condition) cleared(v float64) bool {
	if c.high {
		return v < c.limit-c.deadband
	}
	return v > c.limit+c.deadband
}

func (c condition) message(v float64, unit string) string {
	verb := "exceeded"
	if !c.high {
		verb = "fell below"
	}
	return fmt.Sprintf("%s %s %s limit (%.1f %s)", c.desc, verb, c.stage, v, unit)
}

func (e *Engine) emit(ev *scadav1.AlarmEvent) *scadav1.AlarmEvent {
	out := proto.Clone(ev).(*scadav1.AlarmEvent)
	if e.sink != nil {
		e.sink(out)
	}
	return out
}

func (e *Engine) raise(id, tag string, sev scadav1.AlarmSeverity, msg string, v float64, ts int64) *scadav1.AlarmEvent {
	ev := &scadav1.AlarmEvent{
		AlarmId: id, TagId: tag, Severity: sev,
		State:   scadav1.AlarmState_ALARM_UNACK_ALARM,
		Message: msg, TriggerValue: v, TriggerTimeUtcMs: ts,
	}
	e.active[id] = ev // replaces an UNACK_RTN entry on re-alarm
	e.raised++
	return e.emit(ev)
}

func (e *Engine) returnToNormal(cur *scadav1.AlarmEvent, ts int64) *scadav1.AlarmEvent {
	cur.RtnTimeUtcMs = ts
	if cur.State == scadav1.AlarmState_ALARM_ACK_ALARM {
		cur.State = scadav1.AlarmState_ALARM_ACK_RTN
		delete(e.active, cur.AlarmId)
		e.cleared++
	} else {
		cur.State = scadav1.AlarmState_ALARM_UNACK_RTN
	}
	return e.emit(cur)
}

func (e *Engine) acknowledge(id, user string) (*scadav1.AlarmEvent, error) {
	cur := e.active[id]
	if cur == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	switch cur.State {
	case scadav1.AlarmState_ALARM_UNACK_ALARM:
		cur.State = scadav1.AlarmState_ALARM_ACK_ALARM
	case scadav1.AlarmState_ALARM_UNACK_RTN:
		cur.State = scadav1.AlarmState_ALARM_ACK_RTN
		delete(e.active, id)
		e.cleared++
	default:
		return nil, fmt.Errorf("%w: %s", ErrAlreadyAcknowledged, id)
	}
	cur.AckTimeUtcMs = e.nowMs()
	cur.AckByUser = user
	e.acked++
	return e.emit(cur), nil
}

// DefaultRules installs the demo rule set of a city gate station.
func DefaultRules(e *Engine, station string) error {
	lim := func(v float64, s scadav1.AlarmSeverity) *Limit { return &Limit{Value: v, Severity: s} }
	rules := []AnalogRule{
		{
			TagID: station + "_P_IN", Description: "Inlet pressure", Deadband: 0.2,
			HH: lim(251.0, scadav1.AlarmSeverity_SEVERITY_CRITICAL),
			H:  lim(250.5, scadav1.AlarmSeverity_SEVERITY_HIGH),
			L:  lim(248.5, scadav1.AlarmSeverity_SEVERITY_MEDIUM),
			LL: lim(247.5, scadav1.AlarmSeverity_SEVERITY_CRITICAL),
		},
		{
			TagID: station + "_P_OUT", Description: "Outlet pressure", Deadband: 0.2,
			HH: lim(62.0, scadav1.AlarmSeverity_SEVERITY_CRITICAL),
			H:  lim(61.0, scadav1.AlarmSeverity_SEVERITY_HIGH),
			L:  lim(59.0, scadav1.AlarmSeverity_SEVERITY_MEDIUM),
			LL: lim(58.0, scadav1.AlarmSeverity_SEVERITY_CRITICAL),
		},
		{
			TagID: station + "_TEMP", Description: "Gas temperature", Deadband: 0.2,
			HH: lim(18.0, scadav1.AlarmSeverity_SEVERITY_CRITICAL),
			H:  lim(16.5, scadav1.AlarmSeverity_SEVERITY_HIGH),
			L:  lim(13.5, scadav1.AlarmSeverity_SEVERITY_MEDIUM),
			LL: lim(12.0, scadav1.AlarmSeverity_SEVERITY_CRITICAL),
		},
	}
	for _, r := range rules {
		if err := e.AddAnalogRule(r); err != nil {
			return err
		}
	}
	if err := e.AddDigitalRule(DigitalRule{
		TagID: station + "_LEAK", AlarmSuffix: "ALM", ActiveState: true,
		Severity: scadav1.AlarmSeverity_SEVERITY_CRITICAL,
		Message:  "Gas leak detected at " + station,
	}); err != nil {
		return err
	}
	// The slam-shut tag is true while armed (open); false means it has tripped.
	return e.AddDigitalRule(DigitalRule{
		TagID: station + "_SLAM_SHUT", AlarmSuffix: "TRIP", ActiveState: false,
		Severity: scadav1.AlarmSeverity_SEVERITY_HIGH,
		Message:  "Slam-shut valve tripped at " + station,
	})
}
