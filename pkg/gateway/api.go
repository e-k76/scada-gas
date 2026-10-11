package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/historian"
)

// TrendSource provides downsampled history; *historian.Reader implements it.
type TrendSource interface {
	Trend(ctx context.Context, tags []string, start, end time.Time, bucket time.Duration) ([]historian.TrendSeries, error)
	Ping(ctx context.Context) error
}

// APIConfig configures the HTTP API.
type APIConfig struct {
	Core    Core
	Trend   TrendSource // nil disables the trend endpoint (HTTP 503)
	Hub     *Hub
	Static  fs.FS  // dashboard files served at "/"; nil disables
	Token   string // when set, /api/v1/* (except health) and /ws/* require it
	Timeout time.Duration
}

// API serves the northbound REST endpoints and the WebSocket upgrade.
type API struct {
	cfg APIConfig
}

// Limits for the trend endpoint.
const (
	maxTrendTags    = 10
	maxTrendRange   = 366 * 24 * time.Hour
	defaultRange    = time.Hour
	defaultBuckets  = 300
	minBuckets      = 10
	maxBuckets      = 2000
	maxBodyBytes    = 4096
	maxUserLen      = 64
	maxAlarmIDLen   = 128
	maxTagIDLen     = 128
	defaultCallTime = 5 * time.Second
)

var tagIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:\-]+$`)

// NewAPI creates the API.
func NewAPI(cfg APIConfig) *API {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultCallTime
	}
	return &API{cfg: cfg}
}

// Handler returns the complete HTTP handler (REST, WebSocket and dashboard).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", a.handleHealth)
	mux.Handle("GET /api/v1/telemetry/current", a.auth(http.HandlerFunc(a.handleTelemetryCurrent)))
	mux.Handle("GET /api/v1/alarms/active", a.auth(http.HandlerFunc(a.handleAlarmsActive)))
	mux.Handle("POST /api/v1/alarms/ack", a.auth(http.HandlerFunc(a.handleAlarmAck)))
	mux.Handle("GET /api/v1/historian/trend", a.auth(http.HandlerFunc(a.handleTrend)))
	if a.cfg.Hub != nil {
		mux.Handle("GET /ws/telemetry", a.auth(http.HandlerFunc(a.cfg.Hub.ServeWS)))
	}
	if a.cfg.Static != nil {
		mux.Handle("GET /", http.FileServerFS(a.cfg.Static))
	}
	return securityHeaders(mux)
}

// --- middleware ---------------------------------------------------------------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none'")
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// auth enforces the optional shared token. Browsers cannot set headers on a
// WebSocket handshake, so ?token= is accepted as well.
func (a *API) auth(next http.Handler) http.Handler {
	if a.cfg.Token == "" {
		return next
	}
	want := []byte(a.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="scada-gateway"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- helpers ------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"encoding failure"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeCoreError maps a gRPC failure to an HTTP status.
func writeCoreError(w http.ResponseWriter, err error) {
	switch status.Code(err) {
	case codes.InvalidArgument:
		writeError(w, http.StatusBadRequest, status.Convert(err).Message())
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		writeError(w, http.StatusServiceUnavailable, "core server unavailable")
	default:
		log.Printf("[GW] core error: %v", err)
		writeError(w, http.StatusBadGateway, "core server error")
	}
}

// --- handlers -----------------------------------------------------------------

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	res := map[string]any{"status": "ok", "time": time.Now().UTC().UnixMilli()}
	coreState := "ok"
	if _, err := a.cfg.Core.ActiveAlarms(ctx, "", scadav1.AlarmSeverity_SEVERITY_INFO); err != nil {
		coreState = "unreachable"
		res["status"] = "degraded"
	}
	res["core"] = coreState
	switch {
	case a.cfg.Trend == nil:
		res["historian"] = "disabled"
	case a.cfg.Trend.Ping(ctx) != nil:
		res["historian"] = "unreachable"
	default:
		res["historian"] = "ok"
	}
	if a.cfg.Hub != nil {
		st := a.cfg.Hub.Stats()
		res["websocket"] = map[string]any{"clients": st.Clients, "connects": st.Connects, "slow_disconnects": st.Slow}
	}
	code := http.StatusOK
	if coreState != "ok" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, res)
}

func (a *API) handleTelemetryCurrent(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout)
	defer cancel()
	tags, err := a.cfg.Core.CurrentTelemetry(ctx)
	if err != nil {
		writeCoreError(w, err)
		return
	}
	out := tagsFromSnapshots(tags)
	writeJSON(w, http.StatusOK, map[string]any{
		"time": time.Now().UTC().UnixMilli(), "count": len(out), "tags": out,
	})
}

func (a *API) handleAlarmsActive(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tag := q.Get("tag_id")
	if tag != "" && (len(tag) > maxTagIDLen || !tagIDPattern.MatchString(tag)) {
		writeError(w, http.StatusBadRequest, "invalid tag_id")
		return
	}
	minSev := 0
	if v := q.Get("min_severity"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > int(scadav1.AlarmSeverity_SEVERITY_CRITICAL) {
			writeError(w, http.StatusBadRequest, "min_severity must be 0 (INFO) .. 4 (CRITICAL)")
			return
		}
		minSev = n
	}
	onlyUnacked := false
	if v := q.Get("unacknowledged"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "unacknowledged must be true or false")
			return
		}
		onlyUnacked = b
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout)
	defer cancel()
	alarms, err := a.cfg.Core.ActiveAlarms(ctx, tag, scadav1.AlarmSeverity(minSev))
	if err != nil {
		writeCoreError(w, err)
		return
	}
	out := make([]AlarmJSON, 0, len(alarms))
	unacked := 0
	for _, e := range alarms {
		needsAck := e.GetState() == scadav1.AlarmState_ALARM_UNACK_ALARM || e.GetState() == scadav1.AlarmState_ALARM_UNACK_RTN
		if needsAck {
			unacked++
		}
		if onlyUnacked && !needsAck {
			continue
		}
		out = append(out, AlarmFromEvent(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"time": time.Now().UTC().UnixMilli(), "count": len(out), "unacknowledged": unacked, "alarms": out,
	})
}

type ackRequest struct {
	AlarmID string `json:"alarm_id"`
	User    string `json:"user"`
}

func validUser(u string) bool {
	if u == "" || len(u) > maxUserLen {
		return false
	}
	for _, r := range u {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (a *API) handleAlarmAck(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req ackRequest
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request body must contain a single JSON object")
		return
	}
	req.AlarmID = strings.TrimSpace(req.AlarmID)
	req.User = strings.TrimSpace(req.User)
	if req.AlarmID == "" || len(req.AlarmID) > maxAlarmIDLen || !tagIDPattern.MatchString(req.AlarmID) {
		writeError(w, http.StatusBadRequest, "alarm_id is required and must be a valid alarm id")
		return
	}
	if !validUser(req.User) {
		writeError(w, http.StatusBadRequest, "user is required (1-64 printable characters)")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout)
	defer cancel()
	resp, err := a.cfg.Core.Acknowledge(ctx, req.AlarmID, req.User)
	if err != nil {
		writeCoreError(w, err)
		return
	}
	log.Printf("[GW] ack alarm=%s user=%q remote=%s success=%t (%s)", req.AlarmID, req.User, r.RemoteAddr, resp.GetSuccess(), resp.GetMessage())
	if !resp.GetSuccess() {
		// The alarm is unknown, already acknowledged or already cleared.
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": resp.GetMessage()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "message": resp.GetMessage(), "alarm": AlarmFromEvent(resp.GetAlarm()),
	})
}

// --- trend --------------------------------------------------------------------

type trendPoint struct {
	T   int64   `json:"t"` // bucket start, epoch ms
	Avg float64 `json:"avg"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	N   int64   `json:"n"`
	Bad bool    `json:"bad,omitempty"`
}

type trendSeries struct {
	TagID  string       `json:"tag_id"`
	Points []trendPoint `json:"points"`
}

// parseTime accepts RFC 3339 or a Unix timestamp in seconds or milliseconds.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return time.Time{}, fmt.Errorf("%q is not RFC 3339 or a Unix timestamp", s)
	}
	if n >= 100_000_000_000 { // milliseconds
		return time.UnixMilli(n).UTC(), nil
	}
	return time.Unix(n, 0).UTC(), nil
}

func parseTrendTags(vals []string) ([]string, error) {
	var tags []string
	seen := map[string]bool{}
	for _, v := range vals {
		for _, t := range strings.Split(v, ",") {
			t = strings.TrimSpace(t)
			if t == "" || seen[t] {
				continue
			}
			if len(t) > maxTagIDLen || !tagIDPattern.MatchString(t) {
				return nil, fmt.Errorf("invalid tag_id %q", t)
			}
			seen[t] = true
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return nil, errors.New("tag_id is required (repeat the parameter or separate ids with commas)")
	}
	if len(tags) > maxTrendTags {
		return nil, fmt.Errorf("at most %d tags per request", maxTrendTags)
	}
	return tags, nil
}

func (a *API) handleTrend(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tags, err := parseTrendTags(q["tag_id"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	end := time.Now().UTC()
	if v := q.Get("end"); v != "" {
		if end, err = parseTime(v); err != nil {
			writeError(w, http.StatusBadRequest, "end: "+err.Error())
			return
		}
	}
	start := end.Add(-defaultRange)
	if v := q.Get("start"); v != "" {
		if start, err = parseTime(v); err != nil {
			writeError(w, http.StatusBadRequest, "start: "+err.Error())
			return
		}
	}
	if !end.After(start) {
		writeError(w, http.StatusBadRequest, "end must be after start")
		return
	}
	if end.Sub(start) > maxTrendRange {
		writeError(w, http.StatusBadRequest, "time range too large (max 366 days)")
		return
	}
	buckets := defaultBuckets
	if v := q.Get("buckets"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < minBuckets || n > maxBuckets {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("buckets must be %d..%d", minBuckets, maxBuckets))
			return
		}
		buckets = n
	}
	bucket := time.Duration(math.Ceil(end.Sub(start).Seconds()/float64(buckets))) * time.Second
	if bucket < time.Second {
		bucket = time.Second
	}

	if a.cfg.Trend == nil {
		writeError(w, http.StatusServiceUnavailable, "historian database not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	series, err := a.cfg.Trend.Trend(ctx, tags, start, end, bucket)
	if err != nil {
		log.Printf("[GW] trend query failed: %v", err)
		writeError(w, http.StatusServiceUnavailable, "historian database unavailable")
		return
	}
	out := make([]trendSeries, 0, len(series))
	for _, s := range series {
		ts := trendSeries{TagID: s.TagID, Points: make([]trendPoint, 0, len(s.Points))}
		for _, p := range s.Points {
			ts.Points = append(ts.Points, trendPoint{
				T: p.Time.UnixMilli(), Avg: p.Avg, Min: p.Min, Max: p.Max, N: p.Count, Bad: p.BadQual,
			})
		}
		out = append(out, ts)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"start_ms": start.UnixMilli(), "end_ms": end.UnixMilli(),
		"bucket_seconds": int64(bucket / time.Second), "series": out,
	})
}
