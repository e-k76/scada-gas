package gateway

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

// HubConfig tunes the WebSocket dispatcher.
type HubConfig struct {
	MaxClients      int           // concurrent WebSocket clients accepted
	SendBuffer      int           // queued messages per client before it is dropped as too slow
	WriteTimeout    time.Duration // deadline for one write to a client
	PingInterval    time.Duration // keep-alive ping period
	PongTimeout     time.Duration // client must answer within this (or send any frame)
	SnapshotTimeout time.Duration // time allowed to fetch the initial snapshot from the core
	MaxMessageBytes int64         // largest frame accepted from a client
	AllowedOrigins  []string      // extra allowed Origin values ("*" allows all); same-host is always allowed
}

// DefaultHubConfig returns production-style defaults.
func DefaultHubConfig() HubConfig {
	return HubConfig{
		MaxClients:      200,
		SendBuffer:      256,
		WriteTimeout:    10 * time.Second,
		PingInterval:    25 * time.Second,
		PongTimeout:     60 * time.Second,
		SnapshotTimeout: 3 * time.Second,
		MaxMessageBytes: 4096,
	}
}

// HubStats are lifetime counters of the hub.
type HubStats struct {
	Clients  int
	Connects uint64
	Messages uint64 // messages queued to clients
	Slow     uint64 // clients disconnected because their send buffer filled up
}

// Hub fans telemetry and alarm events out to connected WebSocket clients.
//
// Each client has one reader and one writer goroutine; both are tied to the
// connection lifetime and are joined by Close, so connects and disconnects
// never leak goroutines. A client that cannot keep up is disconnected instead
// of slowing the broadcast down.
type Hub struct {
	cfg  HubConfig
	core Core
	up   websocket.Upgrader

	mu      sync.RWMutex
	clients map[*client]struct{}
	closed  bool
	wg      sync.WaitGroup

	connects, messages, slow atomic.Uint64
}

// NewHub creates a hub. core is used to build the snapshot sent to every new client.
func NewHub(cfg HubConfig, core Core) *Hub {
	d := DefaultHubConfig()
	if cfg.MaxClients <= 0 {
		cfg.MaxClients = d.MaxClients
	}
	if cfg.SendBuffer <= 0 {
		cfg.SendBuffer = d.SendBuffer
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = d.WriteTimeout
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = d.PingInterval
	}
	if cfg.PongTimeout <= 0 {
		cfg.PongTimeout = d.PongTimeout
	}
	if cfg.SnapshotTimeout <= 0 {
		cfg.SnapshotTimeout = d.SnapshotTimeout
	}
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = d.MaxMessageBytes
	}
	h := &Hub{cfg: cfg, core: core, clients: make(map[*client]struct{})}
	h.up = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin:     h.checkOrigin,
	}
	return h
}

// checkOrigin allows requests without an Origin header (non-browser clients),
// same-host origins, and explicitly allowed origins.
func (h *Hub) checkOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, a := range h.cfg.AllowedOrigins {
		if a == "*" || strings.EqualFold(a, o) {
			return true
		}
	}
	return false
}

type client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
	once sync.Once
}

// ClientCount returns the number of connected clients.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Stats returns a snapshot of the hub counters.
func (h *Hub) Stats() HubStats {
	return HubStats{
		Clients: h.ClientCount(), Connects: h.connects.Load(),
		Messages: h.messages.Load(), Slow: h.slow.Load(),
	}
}

// register adds c unless the hub is closed or full. On success it also takes
// a WaitGroup slot for the connection's lifetime.
func (h *Hub) register(c *client) (ok bool, reason int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closed:
		return false, websocket.CloseGoingAway
	case len(h.clients) >= h.cfg.MaxClients:
		return false, websocket.CloseTryAgainLater
	}
	h.clients[c] = struct{}{}
	h.wg.Add(1)
	h.connects.Add(1)
	return true, 0
}

// remove drops c from the hub and closes its connection. Idempotent.
func (h *Hub) remove(c *client) {
	c.once.Do(func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
		close(c.done)
		_ = c.conn.Close()
	})
}

// ServeWS upgrades the request to a WebSocket and serves the client until it
// disconnects. It blocks for the lifetime of the connection.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	if h.ClientCount() >= h.cfg.MaxClients {
		http.Error(w, "too many websocket clients", http.StatusServiceUnavailable)
		return
	}
	conn, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already written the HTTP error
	}
	c := &client{hub: h, conn: conn, send: make(chan []byte, h.cfg.SendBuffer), done: make(chan struct{})}
	if ok, code := h.register(c); !ok {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "unavailable"),
			time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	defer h.wg.Done()
	defer h.remove(c)

	// The client is registered first so events published while the snapshot
	// is being fetched are queued behind it; replaying them is harmless
	// because the browser applies every message as an idempotent upsert.
	if err := c.writeSnapshot(r.Context()); err != nil {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		c.writePump()
	}()
	c.readPump()
}

func (c *client) writeSnapshot(ctx context.Context) error {
	h := c.hub
	sctx, cancel := context.WithTimeout(ctx, h.cfg.SnapshotTimeout)
	defer cancel()
	msg := snapshotMsg{Type: "snapshot", Time: time.Now().UTC().UnixMilli(), Tags: []TagJSON{}, Alarms: []AlarmJSON{}}
	var errs []string
	if tags, err := h.core.CurrentTelemetry(sctx); err != nil {
		errs = append(errs, "telemetry: "+err.Error())
	} else {
		msg.Tags = tagsFromSnapshots(tags)
	}
	if alarms, err := h.core.ActiveAlarms(sctx, "", scadav1.AlarmSeverity_SEVERITY_INFO); err != nil {
		errs = append(errs, "alarms: "+err.Error())
	} else {
		msg.Alarms = alarmsFromEvents(alarms)
	}
	msg.Error = strings.Join(errs, "; ")
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout))
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// writePump is the only writer of c.conn after the snapshot.
func (c *client) writePump() {
	h := c.hub
	ping := time.NewTicker(h.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				h.remove(c)
				return
			}
		case <-ping.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(h.cfg.WriteTimeout))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				h.remove(c)
				return
			}
		}
	}
}

// readPump discards client frames; it exists to process control frames and to
// detect disconnects and dead peers.
func (c *client) readPump() {
	h := c.hub
	c.conn.SetReadLimit(h.cfg.MaxMessageBytes)
	_ = c.conn.SetReadDeadline(time.Now().Add(h.cfg.PongTimeout))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(h.cfg.PongTimeout))
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(h.cfg.PongTimeout))
	}
}

// broadcast queues data for every client; clients with a full buffer are
// disconnected after the lock is released.
func (h *Hub) broadcast(data []byte) {
	var slow []*client
	h.mu.RLock()
	for c := range h.clients {
		select {
		case c.send <- data:
			h.messages.Add(1)
		default:
			slow = append(slow, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range slow {
		h.slow.Add(1)
		log.Printf("[GW] websocket client %s too slow, disconnecting", c.conn.RemoteAddr())
		h.remove(c)
	}
}

// BroadcastTelemetry sends one telemetry batch to all clients.
func (h *Hub) BroadcastTelemetry(u *scadav1.TelemetryStreamUpdate) {
	if h.ClientCount() == 0 {
		return
	}
	fep := u.GetSourceFepId()
	tags := make([]TagJSON, 0, len(u.GetAnalogUpdates())+len(u.GetDigitalUpdates()))
	for _, a := range u.GetAnalogUpdates() {
		tags = append(tags, TagFromAnalog(a, fep))
	}
	for _, d := range u.GetDigitalUpdates() {
		tags = append(tags, TagFromDigital(d, fep))
	}
	data, err := json.Marshal(telemetryMsg{
		Type: "telemetry", Time: time.Now().UTC().UnixMilli(), FEP: fep, Seq: u.GetPacketSequence(), Tags: tags,
	})
	if err != nil {
		log.Printf("[GW] marshal telemetry: %v", err)
		return
	}
	h.broadcast(data)
}

// BroadcastAlarm sends one alarm state transition to all clients.
func (h *Hub) BroadcastAlarm(ev *scadav1.AlarmEvent) {
	if h.ClientCount() == 0 {
		return
	}
	data, err := json.Marshal(alarmMsg{Type: "alarm", Time: time.Now().UTC().UnixMilli(), Alarm: AlarmFromEvent(ev)})
	if err != nil {
		log.Printf("[GW] marshal alarm: %v", err)
		return
	}
	h.broadcast(data)
}

// Close disconnects every client and waits for all hub goroutines to finish.
// The hub rejects new connections afterwards.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	list := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		list = append(list, c)
	}
	h.mu.Unlock()
	for _, c := range list {
		_ = c.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"),
			time.Now().Add(time.Second))
		h.remove(c)
	}
	h.wg.Wait()
}
