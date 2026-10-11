// Package command routes supervisory control commands from the core to the FEP
// that serves the target station and returns the FEP's result.
//
// Safety properties:
//   - a command is only forwarded if the FEP advertised that action for that tag;
//   - setpoints are range-checked against the advertised limits;
//   - a command_id executes at most once (resends return the first outcome);
//   - only one command per tag can be in flight;
//   - when no result arrives in time the outcome is reported as TIMEOUT
//     ("unknown"), never as success, and a late result still updates the record.
package command

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

// Supported actions.
const (
	ActionOpenValve   = "CMD_OPEN_VALVE"
	ActionCloseValve  = "CMD_CLOSE_VALVE"
	ActionResetTrip   = "CMD_RESET_TRIP"
	ActionSetSetpoint = "CMD_SET_SETPOINT"
)

// Ack statuses.
const (
	StatusExecuted = "EXECUTED"
	StatusFailed   = "FAILED"
	StatusRejected = "REJECTED"
	StatusNoFEP    = "NO_FEP"
	StatusTimeout  = "TIMEOUT"
)

var (
	idPattern      = regexp.MustCompile(`^[A-Za-z0-9_.\-]{8,64}$`)
	stationPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,64}$`)
	tagPattern     = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,128}$`)
)

// ValidAction reports whether a is a known action.
func ValidAction(a string) bool {
	switch a {
	case ActionOpenValve, ActionCloseValve, ActionResetTrip, ActionSetSetpoint:
		return true
	}
	return false
}

// Validate checks the shape of a command (not its target's capabilities).
func Validate(c *scadav1.ControlCommand) error {
	switch {
	case !idPattern.MatchString(c.GetCommandId()):
		return fmt.Errorf("command_id must be 8-64 characters of letters, digits, _ . -")
	case !stationPattern.MatchString(c.GetTargetStationId()):
		return fmt.Errorf("invalid target_station_id")
	case !tagPattern.MatchString(c.GetTargetTagId()):
		return fmt.Errorf("invalid target_tag_id")
	case !ValidAction(c.GetAction()):
		return fmt.Errorf("unknown action %q", c.GetAction())
	}
	if u := strings.TrimSpace(c.GetInitiatedByUser()); u == "" || len(u) > 64 {
		return fmt.Errorf("initiated_by_user is required (1-64 characters)")
	}
	if c.GetAction() == ActionSetSetpoint {
		if math.IsNaN(c.GetSetpointValue()) || math.IsInf(c.GetSetpointValue(), 0) {
			return fmt.Errorf("setpoint_value must be a finite number")
		}
	} else if c.GetSetpointValue() != 0 {
		return fmt.Errorf("setpoint_value is only allowed for %s", ActionSetSetpoint)
	}
	return nil
}

// Session is one connected FEP serving one station.
type Session struct {
	FEPID     string
	StationID string
	Caps      []*scadav1.CommandCapability
	// Out carries commands to the FEP's stream handler.
	Out chan *scadav1.ControlCommand

	done chan struct{}
	once sync.Once
}

// Done is closed when the session was replaced or removed.
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) close() { s.once.Do(func() { close(s.done) }) }

type pendingCmd struct {
	done chan struct{}
	ack  *scadav1.CommandAck
}

type recentEntry struct {
	ack *scadav1.CommandAck
	at  time.Time
}

// Hub is the command router. It is safe for concurrent use.
type Hub struct {
	timeout time.Duration
	now     func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session     // station -> session
	pending  map[string]*pendingCmd  // command_id -> in-flight command
	busyTag  map[string]string       // station/tag -> command_id in flight
	recent   map[string]*recentEntry // command_id -> final outcome
	order    []string                // FIFO of recent ids
}

const (
	recentMax = 1000
	recentTTL = 10 * time.Minute
	queueSize = 16
)

// NewHub creates a hub; timeout is how long Dispatch waits for the FEP's result.
func NewHub(timeout time.Duration) *Hub {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Hub{
		timeout: timeout, now: time.Now,
		sessions: map[string]*Session{}, pending: map[string]*pendingCmd{},
		busyTag: map[string]string{}, recent: map[string]*recentEntry{},
	}
}

// Register adds (or replaces) the FEP session of a station. The replaced
// session, if any, is closed and returned.
func (h *Hub) Register(fepID, station string, caps []*scadav1.CommandCapability) (*Session, *Session) {
	s := &Session{FEPID: fepID, StationID: station, Caps: caps, Out: make(chan *scadav1.ControlCommand, queueSize), done: make(chan struct{})}
	h.mu.Lock()
	old := h.sessions[station]
	h.sessions[station] = s
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
	return s, old
}

// Unregister removes s unless it was already replaced.
func (h *Hub) Unregister(s *Session) {
	h.mu.Lock()
	if h.sessions[s.StationID] == s {
		delete(h.sessions, s.StationID)
	}
	h.mu.Unlock()
	s.close()
}

// Capabilities lists the advertised capabilities. An empty station means all.
func (h *Hub) Capabilities(station string) []*scadav1.StationCapabilities {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*scadav1.StationCapabilities
	for id, s := range h.sessions {
		if station != "" && id != station {
			continue
		}
		caps := make([]*scadav1.CommandCapability, len(s.Caps))
		for i, c := range s.Caps {
			caps[i] = proto.Clone(c).(*scadav1.CommandCapability)
		}
		out = append(out, &scadav1.StationCapabilities{StationId: id, FepId: s.FEPID, Online: true, Capabilities: caps})
	}
	return out
}

func ack(id, status, msg string, ok bool) *scadav1.CommandAck {
	return &scadav1.CommandAck{CommandId: id, Success: ok, Status: status, Message: msg}
}

func findCap(caps []*scadav1.CommandCapability, tag string) *scadav1.CommandCapability {
	for _, c := range caps {
		if c.GetTagId() == tag {
			return c
		}
	}
	return nil
}

func hasAction(c *scadav1.CommandCapability, a string) bool {
	for _, x := range c.GetActions() {
		if x == a {
			return true
		}
	}
	return false
}

func (h *Hub) remember(a *scadav1.CommandAck) {
	// caller holds h.mu
	now := h.now()
	h.recent[a.GetCommandId()] = &recentEntry{ack: proto.Clone(a).(*scadav1.CommandAck), at: now}
	h.order = append(h.order, a.GetCommandId())
	for len(h.order) > 0 {
		id := h.order[0]
		e, ok := h.recent[id]
		if len(h.order) > recentMax || !ok || now.Sub(e.at) > recentTTL {
			delete(h.recent, id)
			h.order = h.order[1:]
			continue
		}
		break
	}
}

// Dispatch validates cmd, forwards it to the station's FEP and waits for the
// result. It always returns an ack (never an error) describing the outcome.
func (h *Hub) Dispatch(ctx context.Context, cmd *scadav1.ControlCommand) *scadav1.CommandAck {
	id := cmd.GetCommandId()
	if err := Validate(cmd); err != nil {
		return ack(id, StatusRejected, err.Error(), false)
	}

	h.mu.Lock()
	if e, ok := h.recent[id]; ok { // resend of a finished command
		a := proto.Clone(e.ack).(*scadav1.CommandAck)
		h.mu.Unlock()
		return a
	}
	if p, ok := h.pending[id]; ok { // resend of a command in flight: share its outcome
		h.mu.Unlock()
		select {
		case <-p.done:
			return proto.Clone(p.ack).(*scadav1.CommandAck)
		case <-ctx.Done():
			return ack(id, StatusTimeout, "gave up waiting for the command already in progress (outcome unknown)", false)
		}
	}
	sess := h.sessions[cmd.GetTargetStationId()]
	if sess == nil {
		h.mu.Unlock() // nothing was sent, so the outcome is not remembered: a resend may succeed later
		return ack(id, StatusNoFEP, fmt.Sprintf("no FEP is connected for station %s, nothing was sent", cmd.GetTargetStationId()), false)
	}
	capab := findCap(sess.Caps, cmd.GetTargetTagId())
	switch {
	case capab == nil || !hasAction(capab, cmd.GetAction()):
		h.mu.Unlock()
		return ack(id, StatusRejected, fmt.Sprintf("%s does not support %s", cmd.GetTargetTagId(), cmd.GetAction()), false)
	case cmd.GetAction() == ActionSetSetpoint && (cmd.GetSetpointValue() < capab.GetMinValue() || cmd.GetSetpointValue() > capab.GetMaxValue()):
		h.mu.Unlock()
		return ack(id, StatusRejected, fmt.Sprintf("setpoint %.2f is outside the allowed range %.2f to %.2f %s",
			cmd.GetSetpointValue(), capab.GetMinValue(), capab.GetMaxValue(), capab.GetUnit()), false)
	}
	busyKey := cmd.GetTargetStationId() + "/" + cmd.GetTargetTagId()
	if other, busy := h.busyTag[busyKey]; busy {
		h.mu.Unlock()
		return ack(id, StatusRejected, fmt.Sprintf("another command (%s) for %s is still in progress", other, cmd.GetTargetTagId()), false)
	}
	p := &pendingCmd{done: make(chan struct{})}
	h.pending[id] = p
	h.busyTag[busyKey] = id
	h.mu.Unlock()

	out := proto.Clone(cmd).(*scadav1.ControlCommand)
	out.TimestampUtcMs = h.now().UTC().UnixMilli() // re-stamped at dispatch
	var final *scadav1.CommandAck
	select {
	case sess.Out <- out:
		wctx, cancel := context.WithTimeout(ctx, h.timeout)
		select {
		case <-p.done:
			final = p.ack
		case <-wctx.Done():
			final = ack(id, StatusTimeout, fmt.Sprintf("no result from the FEP within %s: the command may or may not have been executed, check the live state", h.timeout), false)
		case <-sess.Done():
			final = ack(id, StatusTimeout, "the FEP disconnected before reporting a result: the command may or may not have been executed", false)
		}
		cancel()
	default:
		final = ack(id, StatusFailed, "the FEP command queue is full, nothing was sent", false)
	}

	h.mu.Lock()
	delete(h.pending, id)
	delete(h.busyTag, busyKey)
	h.remember(final)
	h.mu.Unlock()
	return proto.Clone(final).(*scadav1.CommandAck)
}

// Report delivers a FEP's result. It returns false for unknown command ids.
// A result arriving after Dispatch gave up (TIMEOUT) replaces the stored outcome
// so a later resend returns the truth.
func (h *Hub) Report(a *scadav1.CommandAck) bool {
	id := a.GetCommandId()
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.pending[id]; ok {
		select {
		case <-p.done:
			return false // already delivered
		default:
		}
		p.ack = proto.Clone(a).(*scadav1.CommandAck)
		if p.ack.Status == "" {
			p.ack.Status = map[bool]string{true: StatusExecuted, false: StatusFailed}[p.ack.Success]
		}
		close(p.done)
		return true
	}
	if e, ok := h.recent[id]; ok && e.ack.GetStatus() == StatusTimeout {
		late := proto.Clone(a).(*scadav1.CommandAck)
		if late.Status == "" {
			late.Status = map[bool]string{true: StatusExecuted, false: StatusFailed}[late.Success]
		}
		late.Message = "late result: " + late.Message
		e.ack = late
		return true
	}
	return false
}
