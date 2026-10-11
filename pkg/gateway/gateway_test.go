package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/historian"
)

type fakeCore struct {
	mu      sync.Mutex
	tags    []*scadav1.TagSnapshot
	alarms  []*scadav1.AlarmEvent
	failAll error
	ackResp *scadav1.AckAlarmResponse
	acked   []ackRequest
}

func (f *fakeCore) CurrentTelemetry(context.Context) ([]*scadav1.TagSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tags, f.failAll
}
func (f *fakeCore) ActiveAlarms(_ context.Context, tag string, min scadav1.AlarmSeverity) ([]*scadav1.AlarmEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return nil, f.failAll
	}
	var out []*scadav1.AlarmEvent
	for _, a := range f.alarms {
		if (tag == "" || a.TagId == tag) && a.Severity >= min {
			out = append(out, a)
		}
	}
	return out, nil
}
func (f *fakeCore) Acknowledge(_ context.Context, id, user string) (*scadav1.AckAlarmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, ackRequest{id, user})
	return f.ackResp, f.failAll
}

type fakeTrend struct {
	series []historian.TrendSeries
	err    error
	gotTag []string
	gotB   time.Duration
}

func (f *fakeTrend) Trend(_ context.Context, tags []string, _, _ time.Time, b time.Duration) ([]historian.TrendSeries, error) {
	f.gotTag, f.gotB = tags, b
	return f.series, f.err
}
func (f *fakeTrend) Ping(context.Context) error { return f.err }

func newFakeCore() *fakeCore {
	return &fakeCore{
		tags: []*scadav1.TagSnapshot{
			{TagId: "S_P_IN", Value: 250.1, RawValue: 2501, EngineeringUnit: "PSI", Quality: scadav1.DataQuality_QUALITY_GOOD, TimestampUtcMs: 1000, UpdateCount: 3},
			{TagId: "S_LEAK", IsDigital: true, State: true, Quality: scadav1.DataQuality_QUALITY_GOOD, TimestampUtcMs: 1000},
		},
		alarms: []*scadav1.AlarmEvent{
			{AlarmId: "S_P_IN_H", TagId: "S_P_IN", Severity: scadav1.AlarmSeverity_SEVERITY_HIGH, State: scadav1.AlarmState_ALARM_UNACK_ALARM, Message: "m", TriggerValue: 250.6, TriggerTimeUtcMs: 900},
			{AlarmId: "S_LEAK_ALM", TagId: "S_LEAK", Severity: scadav1.AlarmSeverity_SEVERITY_CRITICAL, State: scadav1.AlarmState_ALARM_ACK_ALARM, Message: "leak"},
		},
	}
}

func testServer(t *testing.T, core Core, trend TrendSource, token string) (*httptest.Server, *Hub) {
	t.Helper()
	hub := NewHub(HubConfig{PingInterval: time.Hour, SendBuffer: 8}, core)
	api := NewAPI(APIConfig{Core: core, Trend: trend, Hub: hub, Token: token})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() { hub.Close(); srv.Close() })
	return srv, hub
}

func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

func readMsg(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestWebSocketSnapshotAndBroadcast(t *testing.T) {
	srv, hub := testServer(t, newFakeCore(), nil, "")
	c, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/telemetry"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	snap := readMsg(t, c)
	if snap["type"] != "snapshot" || len(snap["tags"].([]any)) != 2 || len(snap["alarms"].([]any)) != 2 {
		t.Fatalf("bad snapshot: %v", snap)
	}
	waitFor(t, "client registered", func() bool { return hub.ClientCount() == 1 })

	hub.BroadcastTelemetry(&scadav1.TelemetryStreamUpdate{
		SourceFepId: "F1", PacketSequence: 7,
		AnalogUpdates:  []*scadav1.AnalogTagValue{{TagId: "S_P_IN", Value: 251.2, EngineeringUnit: "PSI", Quality: scadav1.DataQuality_QUALITY_GOOD, TimestampUtcMs: 2000}},
		DigitalUpdates: []*scadav1.DigitalTagValue{{TagId: "S_LEAK", State: false, TimestampUtcMs: 2000, IsSoe: true}},
	})
	m := readMsg(t, c)
	tags := m["tags"].([]any)
	if m["type"] != "telemetry" || m["fep"] != "F1" || m["seq"].(float64) != 7 || len(tags) != 2 {
		t.Fatalf("bad telemetry msg: %v", m)
	}
	if first := tags[0].(map[string]any); first["value"].(float64) != 251.2 || first["quality"] != "GOOD" || first["type"] != "analog" {
		t.Fatalf("bad analog tag: %v", first)
	}
	if d := tags[1].(map[string]any); d["state"] != false || d["soe"] != true {
		t.Fatalf("bad digital tag: %v", d)
	}

	hub.BroadcastAlarm(&scadav1.AlarmEvent{AlarmId: "A", TagId: "S_P_IN", Severity: scadav1.AlarmSeverity_SEVERITY_CRITICAL, State: scadav1.AlarmState_ALARM_ACK_RTN})
	a := readMsg(t, c)["alarm"].(map[string]any)
	if a["severity"] != "CRITICAL" || a["state"] != "ACK_RTN" || a["active"] != false {
		t.Fatalf("bad alarm msg: %v", a)
	}
}

func TestHubNoGoroutineLeakOnConnectDisconnect(t *testing.T) {
	srv, hub := testServer(t, newFakeCore(), nil, "")
	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		c, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/telemetry"), nil)
		if err != nil {
			t.Fatal(err)
		}
		readMsg(t, c)
		if i%2 == 0 {
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		}
		c.Close() // half abrupt, half graceful
	}
	waitFor(t, "all clients removed", func() bool { return hub.ClientCount() == 0 })
	waitFor(t, "goroutines to settle", func() bool { return runtime.NumGoroutine() <= before+3 })
	if hub.Stats().Connects != 25 {
		t.Fatalf("connects = %d", hub.Stats().Connects)
	}
}

func TestSlowClientIsDisconnected(t *testing.T) {
	srv, hub := testServer(t, newFakeCore(), nil, "")
	c, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/telemetry"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	readMsg(t, c)
	waitFor(t, "registered", func() bool { return hub.ClientCount() == 1 })
	// The client stops reading; flood far more than SendBuffer + socket buffers.
	upd := &scadav1.TelemetryStreamUpdate{AnalogUpdates: []*scadav1.AnalogTagValue{{TagId: strings.Repeat("X", 2000), Value: 1}}}
	for i := 0; i < 20000 && hub.ClientCount() > 0; i++ {
		hub.BroadcastTelemetry(upd)
	}
	waitFor(t, "slow client dropped", func() bool { return hub.ClientCount() == 0 })
	if hub.Stats().Slow == 0 {
		t.Fatal("slow counter not incremented")
	}
}

func TestOriginCheckAndMaxClients(t *testing.T) {
	core := newFakeCore()
	hub := NewHub(HubConfig{MaxClients: 1, AllowedOrigins: []string{"http://good.example"}}, core)
	srv := httptest.NewServer(http.HandlerFunc(hub.ServeWS))
	defer func() { hub.Close(); srv.Close() }()

	dial := func(origin string) (*websocket.Conn, *http.Response, error) {
		h := http.Header{}
		if origin != "" {
			h.Set("Origin", origin)
		}
		return websocket.DefaultDialer.Dial(wsURL(srv, ""), h)
	}
	if _, resp, err := dial("http://evil.example"); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin must be rejected, err=%v", err)
	}
	c, _, err := dial("http://good.example")
	if err != nil {
		t.Fatalf("allowed origin rejected: %v", err)
	}
	defer c.Close()
	readMsg(t, c)
	waitFor(t, "registered", func() bool { return hub.ClientCount() == 1 })
	if _, resp, err := dial(""); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second client must be refused with 503, err=%v", err)
	}
}

func TestSnapshotDegradedWhenCoreDown(t *testing.T) {
	core := newFakeCore()
	core.failAll = status.Error(codes.Unavailable, "down")
	srv, _ := testServer(t, core, nil, "")
	c, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/telemetry"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	m := readMsg(t, c)
	if m["type"] != "snapshot" || m["error"] == nil || len(m["tags"].([]any)) != 0 {
		t.Fatalf("expected degraded snapshot, got %v", m)
	}
}

func do(t *testing.T, method, url, body string, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp, m
}

func TestRESTCurrentAndActiveAlarms(t *testing.T) {
	srv, _ := testServer(t, newFakeCore(), nil, "")
	resp, m := do(t, "GET", srv.URL+"/api/v1/telemetry/current", "", nil)
	if resp.StatusCode != 200 || m["count"].(float64) != 2 {
		t.Fatalf("current: %d %v", resp.StatusCode, m)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type %q", got)
	}
	_, m = do(t, "GET", srv.URL+"/api/v1/alarms/active", "", nil)
	if m["count"].(float64) != 2 || m["unacknowledged"].(float64) != 1 {
		t.Fatalf("alarms: %v", m)
	}
	_, m = do(t, "GET", srv.URL+"/api/v1/alarms/active?unacknowledged=true", "", nil)
	if m["count"].(float64) != 1 {
		t.Fatalf("unacked filter: %v", m)
	}
	_, m = do(t, "GET", srv.URL+"/api/v1/alarms/active?min_severity=4", "", nil)
	if m["count"].(float64) != 1 {
		t.Fatalf("severity filter: %v", m)
	}
	for _, bad := range []string{"min_severity=9", "min_severity=x", "unacknowledged=maybe", "tag_id=a%20b"} {
		if resp, _ := do(t, "GET", srv.URL+"/api/v1/alarms/active?"+bad, "", nil); resp.StatusCode != 400 {
			t.Fatalf("%s: status %d", bad, resp.StatusCode)
		}
	}
}

func TestRESTAck(t *testing.T) {
	core := newFakeCore()
	core.ackResp = &scadav1.AckAlarmResponse{Success: true, Message: "acknowledged",
		Alarm: &scadav1.AlarmEvent{AlarmId: "S_P_IN_H", State: scadav1.AlarmState_ALARM_ACK_ALARM, AckByUser: "bob"}}
	srv, _ := testServer(t, core, nil, "")
	u := srv.URL + "/api/v1/alarms/ack"

	resp, m := do(t, "POST", u, `{"alarm_id":"S_P_IN_H","user":"bob"}`, nil)
	if resp.StatusCode != 200 || m["success"] != true || m["alarm"].(map[string]any)["state"] != "ACK_ALARM" {
		t.Fatalf("ack: %d %v", resp.StatusCode, m)
	}
	if len(core.acked) != 1 || core.acked[0].User != "bob" {
		t.Fatalf("core call: %+v", core.acked)
	}

	core.ackResp = &scadav1.AckAlarmResponse{Success: false, Message: "alarm already acknowledged"}
	if resp, m := do(t, "POST", u, `{"alarm_id":"S_P_IN_H","user":"bob"}`, nil); resp.StatusCode != 409 || m["success"] != false {
		t.Fatalf("conflict: %d %v", resp.StatusCode, m)
	}

	bad := map[string]string{
		`{"alarm_id":"","user":"bob"}`:                              "empty id",
		`{"alarm_id":"x","user":""}`:                                "empty user",
		`{"alarm_id":"x y","user":"bob"}`:                           "bad id chars",
		`{"alarm_id":"x","user":"bob","extra":1}`:                   "unknown field",
		`{"alarm_id":"x","user":"bob"} {"a":1}`:                     "trailing data",
		`not json`:                                                  "garbage",
		`{"alarm_id":"x","user":"` + strings.Repeat("u", 65) + `"}`: "long user",
	}
	for body, why := range bad {
		if resp, _ := do(t, "POST", u, body, nil); resp.StatusCode != 400 {
			t.Fatalf("%s: status %d", why, resp.StatusCode)
		}
	}
	if resp, _ := do(t, "POST", u, `{"alarm_id":"x","user":"`+strings.Repeat("u", 8000)+`"}`, nil); resp.StatusCode != 413 {
		t.Fatalf("oversize body: status %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", u, "", nil); resp.StatusCode != 405 {
		t.Fatalf("GET on ack: %d", resp.StatusCode)
	}
	core.failAll = status.Error(codes.Unavailable, "down")
	if resp, _ := do(t, "POST", u, `{"alarm_id":"x","user":"bob"}`, nil); resp.StatusCode != 503 {
		t.Fatalf("core down: %d", resp.StatusCode)
	}
}

func TestRESTTrend(t *testing.T) {
	ft := &fakeTrend{series: []historian.TrendSeries{
		{TagID: "A", Points: []historian.TrendPoint{{Time: time.UnixMilli(5000), Avg: 1.5, Min: 1, Max: 2, Count: 4}}},
		{TagID: "B"},
	}}
	srv, _ := testServer(t, newFakeCore(), ft, "")
	base := srv.URL + "/api/v1/historian/trend"

	resp, m := do(t, "GET", base+"?tag_id=A&tag_id=B&start=2026-01-01T00:00:00Z&end=2026-01-01T01:00:00Z&buckets=100", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("trend: %d %v", resp.StatusCode, m)
	}
	if m["bucket_seconds"].(float64) != 36 || len(ft.gotTag) != 2 {
		t.Fatalf("bucket/tags: %v %v", m["bucket_seconds"], ft.gotTag)
	}
	series := m["series"].([]any)
	pts := series[0].(map[string]any)["points"].([]any)
	if len(series) != 2 || pts[0].(map[string]any)["avg"].(float64) != 1.5 || len(series[1].(map[string]any)["points"].([]any)) != 0 {
		t.Fatalf("series: %v", series)
	}
	// comma separated, epoch seconds and ms
	if resp, _ := do(t, "GET", base+"?tag_id=A,B&start=1700000000&end=1700003600000", "", nil); resp.StatusCode != 200 {
		t.Fatalf("epoch/comma: %d", resp.StatusCode)
	}
	for _, q := range []string{
		"", "tag_id=a%20b", "tag_id=A&start=nonsense", "tag_id=A&end=nonsense",
		"tag_id=A&start=2026-01-02T00:00:00Z&end=2026-01-01T00:00:00Z",
		"tag_id=A&start=2020-01-01T00:00:00Z&end=2026-01-01T00:00:00Z", "tag_id=A&buckets=3", "tag_id=A&buckets=99999",
		"tag_id=1,2,3,4,5,6,7,8,9,10,11",
	} {
		if resp, _ := do(t, "GET", base+"?"+q, "", nil); resp.StatusCode != 400 {
			t.Fatalf("%q: status %d", q, resp.StatusCode)
		}
	}
	ft.err = errors.New("db down")
	if resp, _ := do(t, "GET", base+"?tag_id=A", "", nil); resp.StatusCode != 503 {
		t.Fatalf("db down: %d", resp.StatusCode)
	}
	srv2, _ := testServer(t, newFakeCore(), nil, "")
	if resp, _ := do(t, "GET", srv2.URL+"/api/v1/historian/trend?tag_id=A", "", nil); resp.StatusCode != 503 {
		t.Fatalf("no historian: %d", resp.StatusCode)
	}
}

func TestTokenAuth(t *testing.T) {
	srv, _ := testServer(t, newFakeCore(), nil, "s3cret")
	u := srv.URL + "/api/v1/telemetry/current"
	if resp, _ := do(t, "GET", u, "", nil); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", u, "", map[string]string{"Authorization": "Bearer wrong"}); resp.StatusCode != 401 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", u, "", map[string]string{"Authorization": "Bearer s3cret"}); resp.StatusCode != 200 {
		t.Fatalf("header token: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", u+"?token=s3cret", "", nil); resp.StatusCode != 200 {
		t.Fatalf("query token: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", srv.URL+"/api/v1/health", "", nil); resp.StatusCode != 200 {
		t.Fatalf("health must be public: %d", resp.StatusCode)
	}
	if _, resp, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/telemetry"), nil); err == nil || resp.StatusCode != 401 {
		t.Fatalf("ws without token must be 401, err=%v", err)
	}
	c, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/ws/telemetry?token=s3cret"), nil)
	if err != nil {
		t.Fatalf("ws with token: %v", err)
	}
	c.Close()
}

func TestHealth(t *testing.T) {
	core := newFakeCore()
	srv, _ := testServer(t, core, &fakeTrend{}, "")
	resp, m := do(t, "GET", srv.URL+"/api/v1/health", "", nil)
	if resp.StatusCode != 200 || m["status"] != "ok" || m["historian"] != "ok" {
		t.Fatalf("health: %d %v", resp.StatusCode, m)
	}
	core.failAll = status.Error(codes.Unavailable, "x")
	resp, m = do(t, "GET", srv.URL+"/api/v1/health", "", nil)
	if resp.StatusCode != 503 || m["core"] != "unreachable" {
		t.Fatalf("health degraded: %d %v", resp.StatusCode, m)
	}
}

func TestNonFiniteValuesDoNotBreakJSON(t *testing.T) {
	nan := TagFromAnalog(&scadav1.AnalogTagValue{TagId: "T", Value: nanValue()}, "F")
	if nan.Value != nil {
		t.Fatal("NaN must be omitted")
	}
	if _, err := json.Marshal(nan); err != nil {
		t.Fatal(err)
	}
}
