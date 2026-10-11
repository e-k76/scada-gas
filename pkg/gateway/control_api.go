package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/audit"
	"github.com/example/scada-gas/pkg/command"
)

// CommandDispatcher is the core as seen by the control API.
type CommandDispatcher interface {
	SendControlCommand(ctx context.Context, cmd *scadav1.ControlCommand) (*scadav1.CommandAck, error)
	CommandCapabilities(ctx context.Context, station string) ([]*scadav1.StationCapabilities, error)
}

// ControlAPI serves the supervisory control endpoints:
//
//	POST /api/v1/control/command        issue a command (see commandRequest)
//	GET  /api/v1/control/capabilities   what each connected station can do
//	GET  /api/v1/control/audit          recent audited commands
//	GET  /api/v1/control/whoami         identity and control privileges of the caller
//
// A command is only dispatched after the caller was authenticated and
// authorised, the request validated, rate limits checked and an audit record
// written. If the audit log cannot be written nothing is sent (fail closed).
type ControlAPI struct {
	disp    CommandDispatcher
	audit   audit.Store
	authn   *Authenticator
	timeout time.Duration

	// rate limit: at most rateMax commands per rateWindow and operator
	rateMax    int
	rateWindow time.Duration

	mu       sync.Mutex
	recent   map[string][]time.Time // operator -> command times
	inflight map[string]string      // station/tag -> command id

	now   func() time.Time
	newID func() string
}

// NewControlAPI creates the control endpoints.
func NewControlAPI(d CommandDispatcher, a audit.Store, authn *Authenticator) *ControlAPI {
	return &ControlAPI{
		disp: d, audit: a, authn: authn, timeout: 12 * time.Second,
		rateMax: 10, rateWindow: 30 * time.Second,
		recent: map[string][]time.Time{}, inflight: map[string]string{},
		now: time.Now, newID: newCommandID,
	}
}

func newCommandID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "cmd-" + strconv.FormatInt(time.Now().UnixMilli(), 36) + "-" + hex.EncodeToString(b[:])
}

// Register adds the routes; wrap authenticates (it must store the Principal).
func (c *ControlAPI) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/control/command", wrap(http.HandlerFunc(c.handleCommand)))
	mux.Handle("GET /api/v1/control/capabilities", wrap(http.HandlerFunc(c.handleCapabilities)))
	mux.Handle("GET /api/v1/control/audit", wrap(http.HandlerFunc(c.handleAudit)))
	mux.Handle("GET /api/v1/control/whoami", wrap(http.HandlerFunc(c.handleWhoami)))
}

// minRole is the privilege an action needs.
func minRole(action string) Role {
	if action == command.ActionSetSetpoint {
		return RoleEngineer
	}
	return RoleOperator
}

type commandRequest struct {
	StationID     string   `json:"station_id"`
	TagID         string   `json:"tag_id"`
	Action        string   `json:"action"`
	SetpointValue *float64 `json:"setpoint_value,omitempty"` // CMD_SET_SETPOINT only
	User          string   `json:"user,omitempty"`           // must match the authenticated operator
	// Confirmed must be true: the caller asserts that a human confirmed this
	// specific command (the HMI shows a two-step confirmation).
	Confirmed bool `json:"confirmed"`
}

// rateOK records an attempt and reports whether the operator is within the limit.
func (c *ControlAPI) rateOK(user string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	kept := c.recent[user][:0]
	for _, t := range c.recent[user] {
		if now.Sub(t) < c.rateWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= c.rateMax {
		c.recent[user] = kept
		return false
	}
	c.recent[user] = append(kept, now)
	return true
}

func (c *ControlAPI) claim(key, id string) (other string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if o, busy := c.inflight[key]; busy {
		return o, false
	}
	c.inflight[key] = id
	return "", true
}

func (c *ControlAPI) release(key string) {
	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()
}

func httpStatusFor(st string) int {
	switch st {
	case command.StatusExecuted:
		return http.StatusOK
	case command.StatusRejected:
		return http.StatusUnprocessableEntity
	case command.StatusNoFEP:
		return http.StatusServiceUnavailable
	case command.StatusTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

func (c *ControlAPI) handleCommand(w http.ResponseWriter, r *http.Request) {
	start := c.now()
	p := PrincipalFrom(r.Context())
	var req commandRequest
	if !decodeSmall(w, r, &req) {
		return
	}
	req.StationID, req.TagID, req.Action, req.User = strings.TrimSpace(req.StationID), strings.TrimSpace(req.TagID), strings.TrimSpace(req.Action), strings.TrimSpace(req.User)

	entry := audit.Entry{
		Time: start.UTC(), Operator: p.User, Role: p.Role.String(), RemoteAddr: r.RemoteAddr,
		StationID: req.StationID, TagID: req.TagID, Action: req.Action, Setpoint: req.SetpointValue,
	}
	// deny refuses the request, and records the attempt (best effort: a denial needs no audit guarantee).
	deny := func(code int, msg string) {
		entry.CommandID = "-"
		entry.Status, entry.Message = audit.Denied, msg
		if _, err := c.audit.Begin(r.Context(), entry); err != nil {
			log.Printf("[GW] control: could not audit a denied request: %v", err)
		}
		log.Printf("[GW] control DENIED user=%q role=%s %s %s/%s: %s", entry.Operator, entry.Role, entry.Action, entry.StationID, entry.TagID, msg)
		writeJSON(w, code, map[string]any{"success": false, "status": string(audit.Denied), "error": msg})
	}

	// 1. who may issue commands at all
	if ok, why := c.authn.ControlAllowed(p); !ok {
		deny(http.StatusForbidden, why)
		return
	}
	// 2. identity: the credentials decide who the operator is
	user := p.User
	if p.Named() {
		if req.User != "" && req.User != p.User {
			deny(http.StatusForbidden, fmt.Sprintf("user %q does not match the authenticated operator %q", req.User, p.User))
			return
		}
	} else { // lab mode: the name is taken from the request and not verified
		user = req.User
		entry.Operator = user
		entry.Role = RoleOperator.String()
	}
	if !validUser(user) {
		deny(http.StatusBadRequest, "user is required (1-64 printable characters)")
		return
	}
	// 3. request validity
	switch {
	case !command.ValidAction(req.Action):
		deny(http.StatusBadRequest, fmt.Sprintf("action must be one of %s, %s, %s, %s", command.ActionOpenValve, command.ActionCloseValve, command.ActionResetTrip, command.ActionSetSetpoint))
		return
	case !validStationID(req.StationID):
		deny(http.StatusBadRequest, "invalid station_id")
		return
	case !tagIDPattern.MatchString(req.TagID) || len(req.TagID) > maxTagIDLen:
		deny(http.StatusBadRequest, "invalid tag_id")
		return
	case req.Action == command.ActionSetSetpoint && (req.SetpointValue == nil || math.IsNaN(*req.SetpointValue) || math.IsInf(*req.SetpointValue, 0)):
		deny(http.StatusBadRequest, "setpoint_value (a finite number) is required for "+command.ActionSetSetpoint)
		return
	case req.Action != command.ActionSetSetpoint && req.SetpointValue != nil:
		deny(http.StatusBadRequest, "setpoint_value is only allowed for "+command.ActionSetSetpoint)
		return
	case !req.Confirmed:
		deny(http.StatusBadRequest, `the command was not confirmed: send "confirmed": true after the operator confirmed it`)
		return
	}
	// 4. privileges for this action (lab mode counts as operator)
	role := p.Role
	if !p.Named() {
		role = RoleOperator
	}
	if need := minRole(req.Action); role < need {
		deny(http.StatusForbidden, fmt.Sprintf("%s needs the %s role, you are %s", req.Action, need, role))
		return
	}
	// 5. limits
	if !c.rateOK(user) {
		deny(http.StatusTooManyRequests, fmt.Sprintf("rate limit: at most %d commands per %s", c.rateMax, c.rateWindow))
		return
	}
	id := c.newID()
	key := req.StationID + "/" + req.TagID
	if other, ok := c.claim(key, id); !ok {
		deny(http.StatusConflict, fmt.Sprintf("another command (%s) for %s is still in progress", other, req.TagID))
		return
	}
	defer c.release(key)

	// 6. audit BEFORE dispatch; no audit, no command
	entry.CommandID, entry.Operator, entry.Status = id, user, audit.Pending
	auditID, err := c.audit.Begin(r.Context(), entry)
	if err != nil {
		log.Printf("[GW] control: audit log unavailable, command refused: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "status": "NOT_SENT",
			"error": "the audit log is unavailable, so the command was NOT sent"})
		return
	}

	// 7. dispatch to the core
	cmd := &scadav1.ControlCommand{
		CommandId: id, TargetStationId: req.StationID, TargetTagId: req.TagID, Action: req.Action,
		InitiatedByUser: user, TimestampUtcMs: start.UTC().UnixMilli(),
	}
	if req.SetpointValue != nil {
		cmd.SetpointValue = *req.SetpointValue
	}
	ctx, cancel := context.WithTimeout(r.Context(), c.timeout)
	defer cancel()
	ack, derr := c.disp.SendControlCommand(ctx, cmd)
	if derr != nil {
		st, msg := command.StatusFailed, "the core could not be reached, the command was not delivered"
		if code := status.Code(derr); code == codes.DeadlineExceeded || code == codes.Canceled {
			st, msg = command.StatusTimeout, "no answer from the core in time: the command may or may not have been executed, check the live state"
		}
		ack = &scadav1.CommandAck{CommandId: id, Success: false, Status: st, Message: msg}
		log.Printf("[GW] control: dispatch of %s failed: %v", id, derr)
	}

	// 8. complete the audit record
	res := audit.Result{Status: audit.Status(ack.GetStatus()), Success: ack.GetSuccess(), Message: ack.GetMessage(), LatencyMs: c.now().Sub(start).Milliseconds()}
	if res.Status == "" {
		res.Status = audit.Failed
	}
	var executedAt *time.Time
	if ack.GetSuccess() && ack.GetExecutedTimeUtcMs() > 0 {
		t := time.UnixMilli(ack.GetExecutedTimeUtcMs()).UTC()
		executedAt = &t
		res.ExecutedAt = executedAt
	}
	if err := c.audit.Complete(context.WithoutCancel(r.Context()), auditID, res); err != nil {
		log.Printf("[GW] control: AUDIT RECORD %d of command %s stays PENDING: %v", auditID, id, err)
	}
	log.Printf("[GW] control user=%q role=%s %s %s -> %s (%s) in %dms", user, entry.Role, req.Action, key, res.Status, ack.GetMessage(), res.LatencyMs)

	out := map[string]any{
		"command_id": id, "success": ack.GetSuccess(), "status": string(res.Status), "message": ack.GetMessage(),
		"operator": user, "latency_ms": res.LatencyMs,
	}
	if executedAt != nil {
		out["executed_time_ms"] = executedAt.UnixMilli()
	}
	writeJSON(w, httpStatusFor(string(res.Status)), out)
}

func validStationID(s string) bool { return stationPathPattern.MatchString(s) }

func (c *ControlAPI) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	station := r.URL.Query().Get("station_id")
	if station != "" && !validStationID(station) {
		writeError(w, http.StatusBadRequest, "invalid station_id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	list, err := c.disp.CommandCapabilities(ctx, station)
	if err != nil {
		writeCoreError(w, err)
		return
	}
	type capJSON struct {
		TagID   string   `json:"tag_id"`
		Actions []string `json:"actions"`
		Min     float64  `json:"min_value,omitempty"`
		Max     float64  `json:"max_value,omitempty"`
		Unit    string   `json:"unit,omitempty"`
	}
	type stationJSON struct {
		StationID string    `json:"station_id"`
		FEPID     string    `json:"fep_id"`
		Online    bool      `json:"online"`
		Caps      []capJSON `json:"capabilities"`
	}
	out := make([]stationJSON, 0, len(list))
	for _, s := range list {
		sj := stationJSON{StationID: s.GetStationId(), FEPID: s.GetFepId(), Online: s.GetOnline(), Caps: []capJSON{}}
		for _, cp := range s.GetCapabilities() {
			sj.Caps = append(sj.Caps, capJSON{TagID: cp.GetTagId(), Actions: cp.GetActions(), Min: cp.GetMinValue(), Max: cp.GetMaxValue(), Unit: cp.GetUnit()})
		}
		out = append(out, sj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"stations": out})
}

func (c *ControlAPI) handleWhoami(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	allowed, why := c.authn.ControlAllowed(p)
	out := map[string]any{
		"user": p.User, "role": p.Role.String(), "authenticated": p.Authenticated,
		"auth_required": c.authn.Required(), "control_allowed": allowed, "can_set_setpoints": allowed && p.Named() && p.Role >= RoleEngineer,
		"lab_mode": c.authn.ControlOpen(),
	}
	if !allowed {
		out["control_denied_reason"] = why
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *ControlAPI) handleAudit(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Role < RoleOperator {
		writeError(w, http.StatusForbidden, "the audit log needs the operator role")
		return
	}
	tag := r.URL.Query().Get("tag_id")
	if tag != "" && !tagIDPattern.MatchString(tag) {
		writeError(w, http.StatusBadRequest, "invalid tag_id")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be 1..200")
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	list, err := c.audit.Recent(ctx, tag, limit)
	if err != nil {
		log.Printf("[GW] control: audit query failed: %v", err)
		writeError(w, http.StatusServiceUnavailable, "the audit log is unavailable")
		return
	}
	type entryJSON struct {
		ID         int64    `json:"id"`
		TimeMs     int64    `json:"time_ms"`
		CommandID  string   `json:"command_id"`
		Operator   string   `json:"operator"`
		Role       string   `json:"role"`
		StationID  string   `json:"station_id"`
		TagID      string   `json:"tag_id"`
		Action     string   `json:"action"`
		Setpoint   *float64 `json:"setpoint_value,omitempty"`
		Status     string   `json:"status"`
		Success    bool     `json:"success"`
		Message    string   `json:"message"`
		ExecutedMs int64    `json:"executed_time_ms,omitempty"`
		LatencyMs  *int64   `json:"latency_ms,omitempty"`
	}
	out := make([]entryJSON, 0, len(list))
	for _, e := range list {
		j := entryJSON{ID: e.ID, TimeMs: e.Time.UnixMilli(), CommandID: e.CommandID, Operator: e.Operator, Role: e.Role,
			StationID: e.StationID, TagID: e.TagID, Action: e.Action, Setpoint: e.Setpoint, Status: string(e.Status),
			Success: e.Success, Message: e.Message, LatencyMs: e.LatencyMs}
		if e.ExecutedAt != nil {
			j.ExecutedMs = e.ExecutedAt.UnixMilli()
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// decodeSmall decodes a small strict JSON body into dst.
func decodeSmall(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := decodeJSONStrict(r.Body, dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		}
		return false
	}
	return true
}
