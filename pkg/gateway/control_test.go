package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/audit"
	"github.com/example/scada-gas/pkg/command"
	"github.com/example/scada-gas/pkg/model"
	"github.com/example/scada-gas/pkg/topology"
)

type fakeDisp struct {
	mu      sync.Mutex
	cmds    []*scadav1.ControlCommand
	ack     *scadav1.CommandAck
	err     error
	block   chan struct{} // when set, SendControlCommand waits for it
	started chan struct{}
	caps    []*scadav1.StationCapabilities
}

func (f *fakeDisp) SendControlCommand(ctx context.Context, c *scadav1.ControlCommand) (*scadav1.CommandAck, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, c)
	ack, err, block, started := f.ack, f.err, f.block, f.started
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	a := proto.Clone(ack).(*scadav1.CommandAck)
	a.CommandId = c.CommandId
	return a, nil
}

func (f *fakeDisp) CommandCapabilities(context.Context, string) ([]*scadav1.StationCapabilities, error) {
	return f.caps, nil
}

func (f *fakeDisp) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.cmds) }

func okAck() *scadav1.CommandAck {
	return &scadav1.CommandAck{Success: true, Status: command.StatusExecuted, Message: "FC05 write coil 10 = false confirmed by read-back", ExecutedTimeUtcMs: time.Now().UnixMilli()}
}

type ctlEnv struct {
	srv   *httptest.Server
	disp  *fakeDisp
	audit *audit.MemoryStore
	ctl   *ControlAPI
}

const (
	tokAlice = "alice-token"
	tokBob   = "bob-token"
	tokVera  = "vera-token"
)

func hdr(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func newCtlEnv(t *testing.T, authn *Authenticator) *ctlEnv {
	t.Helper()
	e := &ctlEnv{disp: &fakeDisp{ack: okAck()}, audit: audit.NewMemoryStore()}
	e.ctl = NewControlAPI(e.disp, e.audit, authn)
	api := NewAPI(APIConfig{Core: newFakeCore(), Control: e.ctl, Auth: authn,
		Topology: NewTopologyAPI(topology.NewMemoryStore(), &fakeDeployer{resp: &scadav1.DeployTopologyResponse{Accepted: true}})})
	e.srv = httptest.NewServer(api.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func users(t *testing.T) *Authenticator {
	t.Helper()
	a, err := NewAuthenticator("shared-secret", []UserSpec{
		{User: "alice", Role: "operator", TokenSHA256: HashToken(tokAlice)},
		{User: "bob", Role: "engineer", TokenSHA256: HashToken(tokBob)},
		{User: "vera", Role: "viewer", TokenSHA256: HashToken(tokVera)},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *ctlEnv) post(t *testing.T, tok, body string) (*http.Response, map[string]any) {
	t.Helper()
	var h map[string]string
	if tok != "" {
		h = hdr(tok)
	}
	return do(t, "POST", e.srv.URL+"/api/v1/control/command", body, h)
}

const closeBody = `{"station_id":"ST","tag_id":"ST_INLET_VLV","action":"CMD_CLOSE_VALVE","user":"alice","confirmed":true}`

func TestControlHappyPathAndAudit(t *testing.T) {
	e := newCtlEnv(t, users(t))
	resp, m := e.post(t, tokAlice, closeBody)
	if resp.StatusCode != 200 || m["success"] != true || m["status"] != "EXECUTED" || m["operator"] != "alice" || m["executed_time_ms"] == nil {
		t.Fatalf("response: %d %v", resp.StatusCode, m)
	}
	if e.disp.count() != 1 {
		t.Fatal("the command must reach the core exactly once")
	}
	c := e.disp.cmds[0]
	if c.TargetStationId != "ST" || c.TargetTagId != "ST_INLET_VLV" || c.Action != "CMD_CLOSE_VALVE" || c.InitiatedByUser != "alice" || c.TimestampUtcMs == 0 || !strings.HasPrefix(c.CommandId, "cmd-") {
		t.Fatalf("dispatched command: %+v", c)
	}
	entries := e.audit.All()
	if len(entries) != 1 {
		t.Fatalf("audit entries: %d", len(entries))
	}
	a := entries[0]
	if a.Status != audit.Executed || !a.Success || a.Operator != "alice" || a.Role != "operator" || a.CommandID != c.CommandId ||
		a.TagID != "ST_INLET_VLV" || a.Action != "CMD_CLOSE_VALVE" || a.LatencyMs == nil || a.ExecutedAt == nil || a.RemoteAddr == "" {
		t.Fatalf("audit record: %+v", a)
	}
	// the user field may be omitted: the credentials decide
	body := `{"station_id":"ST","tag_id":"ST_INLET_VLV","action":"CMD_OPEN_VALVE","confirmed":true}`
	if resp, m := e.post(t, tokAlice, body); resp.StatusCode != 200 || m["operator"] != "alice" {
		t.Fatalf("without user: %d %v", resp.StatusCode, m)
	}
}

func TestControlAuthenticationAndPrivileges(t *testing.T) {
	e := newCtlEnv(t, users(t))
	if resp, _ := e.post(t, "", closeBody); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := e.post(t, "wrong", closeBody); resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	resp, m := e.post(t, tokVera, closeBody)
	if resp.StatusCode != 403 || !strings.Contains(m["error"].(string), "viewer") || m["status"] != "DENIED" {
		t.Fatalf("viewer: %d %v", resp.StatusCode, m)
	}
	resp, m = e.post(t, "shared-secret", closeBody)
	if resp.StatusCode != 403 || !strings.Contains(m["error"].(string), "personal operator account") {
		t.Fatalf("shared token: %d %v", resp.StatusCode, m)
	}
	// the body cannot claim another identity
	resp, m = e.post(t, tokAlice, strings.Replace(closeBody, `"user":"alice"`, `"user":"bob"`, 1))
	if resp.StatusCode != 403 || !strings.Contains(m["error"].(string), "does not match") {
		t.Fatalf("identity spoofing: %d %v", resp.StatusCode, m)
	}
	// setpoints need the engineer role
	sp := `{"station_id":"ST","tag_id":"ST_P_OUT_SP","action":"CMD_SET_SETPOINT","setpoint_value":62.5,"confirmed":true}`
	if resp, m := e.post(t, tokAlice, sp); resp.StatusCode != 403 || !strings.Contains(m["error"].(string), "engineer") {
		t.Fatalf("operator setpoint: %d %v", resp.StatusCode, m)
	}
	if e.disp.count() != 0 {
		t.Fatal("refused requests must never reach the core")
	}
	if resp, m := e.post(t, tokBob, sp); resp.StatusCode != 200 || e.disp.cmds[0].SetpointValue != 62.5 {
		t.Fatalf("engineer setpoint: %d %v", resp.StatusCode, m)
	}
	// every refusal was audited as DENIED; unauthenticated ones have no identity to record
	denied := 0
	for _, a := range e.audit.All() {
		if a.Status == audit.Denied {
			denied++
			if a.Message == "" {
				t.Fatalf("a denial needs a reason: %+v", a)
			}
		}
	}
	if denied != 4 {
		t.Fatalf("denied audit entries: %d", denied)
	}
}

func TestControlRequestValidation(t *testing.T) {
	e := newCtlEnv(t, users(t))
	bad := map[string]string{
		"not confirmed":    `{"station_id":"ST","tag_id":"ST_INLET_VLV","action":"CMD_CLOSE_VALVE"}`,
		"bad action":       `{"station_id":"ST","tag_id":"ST_INLET_VLV","action":"CMD_EXPLODE","confirmed":true}`,
		"bad station":      `{"station_id":"S T","tag_id":"ST_INLET_VLV","action":"CMD_CLOSE_VALVE","confirmed":true}`,
		"bad tag":          `{"station_id":"ST","tag_id":"a;b","action":"CMD_CLOSE_VALVE","confirmed":true}`,
		"setpoint missing": `{"station_id":"ST","tag_id":"ST_P_OUT_SP","action":"CMD_SET_SETPOINT","confirmed":true}`,
		"stray setpoint":   `{"station_id":"ST","tag_id":"ST_INLET_VLV","action":"CMD_CLOSE_VALVE","setpoint_value":5,"confirmed":true}`,
	}
	for name, body := range bad {
		tok := tokAlice
		if name == "setpoint missing" {
			tok = tokBob
		}
		if resp, _ := e.post(t, tok, body); resp.StatusCode != 400 {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
	for name, body := range map[string]string{"unknown field": `{"station_id":"ST","evil":1}`, "garbage": `nope`, "trailing": closeBody + closeBody} {
		if resp, _ := e.post(t, tokAlice, body); resp.StatusCode != 400 {
			t.Errorf("%s: status %d", name, resp.StatusCode)
		}
	}
	if resp, _ := e.post(t, tokAlice, `{"user":"`+strings.Repeat("x", 9000)+`"}`); resp.StatusCode != 413 {
		t.Errorf("oversize: %d", resp.StatusCode)
	}
	if e.disp.count() != 0 {
		t.Fatal("invalid requests must never reach the core")
	}
}

func TestControlRateLimitAndInflight(t *testing.T) {
	e := newCtlEnv(t, users(t))
	e.ctl.rateMax = 3
	for i := 0; i < 3; i++ {
		if resp, _ := e.post(t, tokAlice, closeBody); resp.StatusCode != 200 {
			t.Fatalf("command %d: %d", i, resp.StatusCode)
		}
	}
	if resp, m := e.post(t, tokAlice, closeBody); resp.StatusCode != 429 || !strings.Contains(m["error"].(string), "rate limit") {
		t.Fatalf("4th command: %d %v", resp.StatusCode, m)
	}
	if resp, _ := e.post(t, tokBob, closeBody[:0]+`{"station_id":"ST","tag_id":"ST_INLET_VLV","action":"CMD_OPEN_VALVE","confirmed":true}`); resp.StatusCode != 200 {
		t.Fatalf("the limit is per operator: %d", resp.StatusCode)
	}

	// one command per tag in flight
	e2 := newCtlEnv(t, users(t))
	e2.disp.block = make(chan struct{})
	e2.disp.started = make(chan struct{}, 2)
	first := make(chan int, 1)
	go func() { r, _ := e2.post(t, tokAlice, closeBody); first <- r.StatusCode }()
	<-e2.disp.started
	resp, m := e2.post(t, tokBob, `{"station_id":"ST","tag_id":"ST_INLET_VLV","action":"CMD_OPEN_VALVE","confirmed":true}`)
	if resp.StatusCode != 409 || !strings.Contains(m["error"].(string), "in progress") {
		t.Fatalf("concurrent command: %d %v", resp.StatusCode, m)
	}
	close(e2.disp.block)
	if code := <-first; code != 200 {
		t.Fatalf("first command: %d", code)
	}
	if resp, _ := e2.post(t, tokBob, `{"station_id":"ST","tag_id":"ST_INLET_VLV","action":"CMD_OPEN_VALVE","confirmed":true}`); resp.StatusCode != 200 {
		t.Fatalf("the tag must be free again: %d", resp.StatusCode)
	}
}

func TestControlFailsClosedWithoutAudit(t *testing.T) {
	e := newCtlEnv(t, users(t))
	e.audit.FailBegin = true
	resp, m := e.post(t, tokAlice, closeBody)
	if resp.StatusCode != 503 || m["status"] != "NOT_SENT" || !strings.Contains(m["error"].(string), "NOT sent") {
		t.Fatalf("audit down: %d %v", resp.StatusCode, m)
	}
	if e.disp.count() != 0 {
		t.Fatal("no audit record, no command")
	}
}

func TestControlOutcomeMapping(t *testing.T) {
	cases := []struct {
		ack    *scadav1.CommandAck
		err    error
		http   int
		status audit.Status
	}{
		{&scadav1.CommandAck{Success: false, Status: command.StatusRejected, Message: "outside range"}, nil, 422, audit.Rejected},
		{&scadav1.CommandAck{Success: false, Status: command.StatusNoFEP, Message: "offline"}, nil, 503, audit.NoFEP},
		{&scadav1.CommandAck{Success: false, Status: command.StatusFailed, Message: "device refused"}, nil, 502, audit.Failed},
		{&scadav1.CommandAck{Success: false, Status: command.StatusTimeout, Message: "unknown"}, nil, 504, audit.Timeout},
		{nil, status.Error(codes.Unavailable, "down"), 502, audit.Failed},
		{nil, status.Error(codes.DeadlineExceeded, "slow"), 504, audit.Timeout},
	}
	for i, c := range cases {
		e := newCtlEnv(t, users(t))
		e.disp.ack, e.disp.err = c.ack, c.err
		resp, m := e.post(t, tokAlice, closeBody)
		if resp.StatusCode != c.http || m["success"] != false || m["status"] != string(c.status) {
			t.Errorf("case %d: %d %v", i, resp.StatusCode, m)
		}
		if a := e.audit.All(); len(a) != 1 || a[0].Status != c.status || a[0].Success {
			t.Errorf("case %d audit: %+v", i, a)
		}
	}
}

func TestControlDisabledAndLabMode(t *testing.T) {
	// no accounts, no lab flag: control is off, everything else stays open
	open, _ := NewAuthenticator("", nil, false)
	e := newCtlEnv(t, open)
	resp, m := e.post(t, "", closeBody)
	if resp.StatusCode != 403 || !strings.Contains(m["error"].(string), "disabled") || e.disp.count() != 0 {
		t.Fatalf("disabled: %d %v", resp.StatusCode, m)
	}
	if resp, _ := do(t, "GET", e.srv.URL+"/api/v1/telemetry/current", "", nil); resp.StatusCode != 200 {
		t.Fatalf("read access must stay open: %d", resp.StatusCode)
	}
	// lab mode: control without accounts, user name unverified
	lab, _ := NewAuthenticator("", nil, true)
	l := newCtlEnv(t, lab)
	if resp, m := l.post(t, "", closeBody); resp.StatusCode != 200 || m["operator"] != "alice" {
		t.Fatalf("lab mode: %d %v", resp.StatusCode, m)
	}
	if resp, _ := l.post(t, "", strings.Replace(closeBody, `"user":"alice",`, "", 1)); resp.StatusCode != 400 {
		t.Fatalf("lab mode needs a user name: %d", resp.StatusCode)
	}
	sp := `{"station_id":"ST","tag_id":"ST_P_OUT_SP","action":"CMD_SET_SETPOINT","setpoint_value":60,"user":"x","confirmed":true}`
	if resp, _ := l.post(t, "", sp); resp.StatusCode != 403 {
		t.Fatalf("lab mode must not allow setpoints: %d", resp.StatusCode)
	}
	if a := l.audit.All(); len(a) < 2 || a[0].Operator != "alice" {
		t.Fatalf("lab mode audit: %+v", a)
	}
}

func TestWhoamiCapabilitiesAndAuditEndpoints(t *testing.T) {
	e := newCtlEnv(t, users(t))
	e.disp.caps = []*scadav1.StationCapabilities{{StationId: "ST", FepId: "FEP1", Online: true, Capabilities: []*scadav1.CommandCapability{
		{TagId: "ST_INLET_VLV", Actions: []string{"CMD_CLOSE_VALVE", "CMD_OPEN_VALVE"}},
		{TagId: "ST_P_OUT_SP", Actions: []string{"CMD_SET_SETPOINT"}, MinValue: 50, MaxValue: 70, Unit: "PSI"}}}}
	get := func(tok, path string) (*http.Response, map[string]any) {
		return do(t, "GET", e.srv.URL+path, "", hdr(tok))
	}

	_, m := get(tokAlice, "/api/v1/control/whoami")
	if m["user"] != "alice" || m["role"] != "operator" || m["control_allowed"] != true || m["can_set_setpoints"] != false || m["auth_required"] != true {
		t.Fatalf("whoami alice: %v", m)
	}
	_, m = get(tokBob, "/api/v1/control/whoami")
	if m["can_set_setpoints"] != true {
		t.Fatalf("whoami bob: %v", m)
	}
	_, m = get(tokVera, "/api/v1/control/whoami")
	if m["control_allowed"] != false || m["control_denied_reason"] == nil {
		t.Fatalf("whoami vera: %v", m)
	}
	if resp, _ := do(t, "GET", e.srv.URL+"/api/v1/control/whoami", "", nil); resp.StatusCode != 401 {
		t.Fatalf("whoami without token: %d", resp.StatusCode)
	}

	_, m = get(tokVera, "/api/v1/control/capabilities?station_id=ST")
	st := m["stations"].([]any)[0].(map[string]any)
	caps := st["capabilities"].([]any)
	if st["online"] != true || len(caps) != 2 || caps[1].(map[string]any)["max_value"].(float64) != 70 {
		t.Fatalf("capabilities: %v", m)
	}
	if resp, _ := get(tokAlice, "/api/v1/control/capabilities?station_id=a%20b"); resp.StatusCode != 400 {
		t.Fatalf("bad station: %d", resp.StatusCode)
	}

	e.post(t, tokAlice, closeBody)
	resp, m := get(tokAlice, "/api/v1/control/audit?tag_id=ST_INLET_VLV&limit=5")
	ents := m["entries"].([]any)
	if resp.StatusCode != 200 || len(ents) != 1 || ents[0].(map[string]any)["operator"] != "alice" || ents[0].(map[string]any)["status"] != "EXECUTED" {
		t.Fatalf("audit endpoint: %d %v", resp.StatusCode, m)
	}
	if resp, _ := get(tokVera, "/api/v1/control/audit"); resp.StatusCode != 403 {
		t.Fatalf("viewers must not read the audit log: %d", resp.StatusCode)
	}
	if resp, _ := get(tokAlice, "/api/v1/control/audit?limit=9999"); resp.StatusCode != 400 {
		t.Fatalf("limit bounds: %d", resp.StatusCode)
	}
}

func TestTopologyWritesNeedEngineerRole(t *testing.T) {
	e := newCtlEnv(t, users(t))
	body := exampleBody(t, "ST1", 0, nil)
	url := e.srv.URL + "/api/v1/topology/ST1"
	if resp, _ := do(t, "POST", url, body, hdr(tokAlice)); resp.StatusCode != 403 {
		t.Fatalf("operator save: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", url, body, hdr(tokVera)); resp.StatusCode != 403 {
		t.Fatalf("viewer save: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", url, body, hdr(tokBob)); resp.StatusCode != 200 {
		t.Fatalf("engineer save: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", url+"/deploy", `{"deployed_by":"alice"}`, hdr(tokAlice)); resp.StatusCode != 403 {
		t.Fatalf("operator deploy: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", url, "", hdr(tokVera)); resp.StatusCode != 200 {
		t.Fatalf("viewers can read topologies: %d", resp.StatusCode)
	}
	// the shared token keeps its Phase 3/4 rights (read + topology), but cannot operate
	if resp, _ := do(t, "POST", url, exampleBody(t, "ST1", 1, nil), hdr("shared-secret")); resp.StatusCode != 200 {
		t.Fatalf("shared token save: %d", resp.StatusCode)
	}
}

func TestDeployedVersionQuery(t *testing.T) {
	store := topology.NewMemoryStore()
	dep := &fakeDeployer{resp: &scadav1.DeployTopologyResponse{Accepted: true}}
	api := NewAPI(APIConfig{Core: newFakeCore(), Topology: NewTopologyAPI(store, dep)})
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	base := srv.URL + "/api/v1/topology/ST1"
	if resp, _ := do(t, "GET", base+"?version=deployed", "", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown station: %d", resp.StatusCode)
	}
	do(t, "POST", base, exampleBody(t, "ST1", 0, nil), nil)
	if resp, m := do(t, "GET", base+"?version=deployed", "", nil); resp.StatusCode != 404 || !strings.Contains(m["error"].(string), "never been deployed") {
		t.Fatalf("not deployed yet: %d %v", resp.StatusCode, m)
	}
	do(t, "POST", base+"/deploy", `{"deployed_by":"bob"}`, nil) // deploys v1
	do(t, "POST", base, exampleBody(t, "ST1", 1, func(s *model.StationTopology) { s.Name = "second draft" }), nil)
	_, m := do(t, "GET", base+"?version=deployed", "", nil)
	if m["topology"].(map[string]any)["version"].(float64) != 1 || m["topology"].(map[string]any)["name"] == "second draft" {
		t.Fatalf("the deployed drawing must be v1, not the newer draft: %v", m["topology"])
	}
	_, m = do(t, "GET", base, "", nil)
	if m["topology"].(map[string]any)["version"].(float64) != 2 {
		t.Fatal("the plain GET still returns the latest saved drawing")
	}
	if resp, _ := do(t, "GET", base+"?version=latest", "", nil); resp.StatusCode != 400 {
		t.Fatalf("bad version value: %d", resp.StatusCode)
	}
}

func TestAuthenticatorConfiguration(t *testing.T) {
	good := UserSpec{User: "a", Role: "operator", TokenSHA256: HashToken("t")}
	bad := map[string][]UserSpec{
		"no name":      {{User: "", Role: "operator", TokenSHA256: HashToken("t")}},
		"duplicate":    {good, good},
		"unknown role": {{User: "a", Role: "root", TokenSHA256: HashToken("t")}},
		"bad hash":     {{User: "a", Role: "operator", TokenSHA256: "abc"}},
	}
	for name, specs := range bad {
		if _, err := NewAuthenticator("", specs, false); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if len(HashToken("x")) != 64 || HashToken("x") == HashToken("y") {
		t.Fatal("HashToken")
	}
	// ?token= works for WebSocket handshakes
	a, _ := NewAuthenticator("", []UserSpec{good}, false)
	r := httptest.NewRequest("GET", "/ws/telemetry?token=t", nil)
	if p, ok := a.Authenticate(r); !ok || p.User != "a" {
		t.Fatalf("query token: %+v %v", p, ok)
	}
}

func TestAlarmAckUsesTheAuthenticatedOperator(t *testing.T) {
	core := newFakeCore()
	core.ackResp = &scadav1.AckAlarmResponse{Success: true, Message: "acknowledged", Alarm: &scadav1.AlarmEvent{AlarmId: "S_P_IN_H", State: scadav1.AlarmState_ALARM_ACK_ALARM}}
	authn := users(t)
	api := NewAPI(APIConfig{Core: core, Auth: authn})
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	url := srv.URL + "/api/v1/alarms/ack"

	if resp, _ := do(t, "POST", url, `{"alarm_id":"S_P_IN_H","user":"vera"}`, hdr(tokVera)); resp.StatusCode != 403 {
		t.Fatalf("viewers must not acknowledge: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", url, `{"alarm_id":"S_P_IN_H","user":"bob"}`, hdr(tokAlice)); resp.StatusCode != 403 {
		t.Fatalf("acknowledging in somebody else's name: %d", resp.StatusCode)
	}
	if len(core.acked) != 0 {
		t.Fatal("refused acknowledgements must not reach the core")
	}
	if resp, _ := do(t, "POST", url, `{"alarm_id":"S_P_IN_H"}`, hdr(tokAlice)); resp.StatusCode != 200 {
		t.Fatalf("operator without a user field: %d", resp.StatusCode)
	}
	if len(core.acked) != 1 || core.acked[0].User != "alice" {
		t.Fatalf("the core must see the authenticated operator: %+v", core.acked)
	}
}

func TestTopologyAuthorIsTheAuthenticatedUser(t *testing.T) {
	store := topology.NewMemoryStore()
	dep := &fakeDeployer{resp: &scadav1.DeployTopologyResponse{Accepted: true}}
	api := NewAPI(APIConfig{Core: newFakeCore(), Auth: users(t), Topology: NewTopologyAPI(store, dep)})
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	url := srv.URL + "/api/v1/topology/ST1"
	// the body claims to be "mallory", the credentials say bob
	body := exampleBody(t, "ST1", 0, func(s *model.StationTopology) { s.UpdatedBy = "mallory" })
	if resp, m := do(t, "POST", url, body, hdr(tokBob)); resp.StatusCode != 200 || m["topology"].(map[string]any)["updated_by"] != "bob" {
		t.Fatalf("save: %d %v", resp.StatusCode, m["topology"])
	}
	if resp, _ := do(t, "POST", url+"/deploy", `{"deployed_by":"mallory"}`, hdr(tokBob)); resp.StatusCode != 200 {
		t.Fatalf("deploy: %d", resp.StatusCode)
	}
	if dep.reqs[0].DeployedBy != "bob" {
		t.Fatalf("the deployer must see the authenticated user, got %q", dep.reqs[0].DeployedBy)
	}
	rec, _ := store.Get(context.Background(), "ST1")
	if rec.DeployedBy != "bob" || rec.UpdatedBy != "bob" {
		t.Fatalf("stored authors: %+v", rec)
	}
	// without a name in the body, the credentials are enough
	if resp, _ := do(t, "POST", url+"/deploy", `{}`, hdr(tokBob)); resp.StatusCode != 200 {
		t.Fatalf("deploy without body user: %d", resp.StatusCode)
	}
}
