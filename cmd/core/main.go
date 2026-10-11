// Command core is the central SCADA server. It
//   - ingests telemetry streams from FEPs and keeps the RTDB current,
//   - evaluates alarms (ISA-18.2) on every update,
//   - serves the alarm API (acknowledge / list / subscribe), and
//   - republishes telemetry and alarm events to subscribers such as the historian.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/alarm"
	"github.com/example/scada-gas/pkg/bus"
	"github.com/example/scada-gas/pkg/rtdb"
)

func formatTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

// core bundles the shared state of all gRPC services.
type core struct {
	db        *rtdb.RTDB
	engine    *alarm.Engine
	telemetry *bus.Broker[*scadav1.TelemetryStreamUpdate]
	alarms    *bus.Broker[*scadav1.AlarmEvent]
	logRTDB   bool
	streams   atomic.Int64

	cfgMu   sync.RWMutex
	configs map[string]*scadav1.RuntimeConfig // deployed topology config per station
	cfgBus  *bus.Broker[*scadav1.RuntimeConfig]
}

// --- FEP ingest service ----------------------------------------------------

type fepService struct {
	scadav1.UnimplementedScadaFepCoreServiceServer
	c *core
}

func (s *fepService) StreamTelemetry(stream scadav1.ScadaFepCoreService_StreamTelemetryServer) error {
	remote := "unknown"
	if p, ok := peer.FromContext(stream.Context()); ok {
		remote = p.Addr.String()
	}
	n := s.c.streams.Add(1)
	defer s.c.streams.Add(-1)
	log.Printf("[CORE] telemetry stream opened from %s (active streams: %d)", remote, n)

	var lastSeq int64
	var fep string
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			log.Printf("[CORE] stream from %s (%s) closed by client, last seq %d", remote, fep, lastSeq)
			return stream.SendAndClose(&scadav1.StreamAck{LastProcessedSequence: lastSeq, IsHealthy: true})
		}
		if err != nil {
			if status.Code(err) == codes.Canceled {
				log.Printf("[CORE] stream from %s (%s) cancelled, last seq %d", remote, fep, lastSeq)
			} else {
				log.Printf("[CORE] stream from %s (%s) error: %v", remote, fep, err)
			}
			return err
		}
		fep = msg.GetSourceFepId()
		if seq := msg.GetPacketSequence(); seq > lastSeq {
			lastSeq = seq
		}
		s.c.ingest(msg)
	}
}

// ingest updates the RTDB, evaluates alarms and republishes the batch.
func (c *core) ingest(msg *scadav1.TelemetryStreamUpdate) {
	fep := msg.GetSourceFepId()
	for _, a := range msg.GetAnalogUpdates() {
		st := c.db.UpdateAnalog(fep, a)
		if c.logRTDB {
			fmt.Printf("[RTDB] Tag: %s | Value: %.1f %s | Quality: %s | Time: %s\n",
				st.TagID, st.Value, st.Unit, st.Quality, formatTime(st.TimestampMs))
		}
		c.engine.EvaluateAnalog(st.TagID, st.Value, st.Unit, st.Quality, st.TimestampMs)
	}
	for _, d := range msg.GetDigitalUpdates() {
		st := c.db.UpdateDigital(fep, d)
		if c.logRTDB {
			soe := ""
			if st.IsSOE {
				soe = " | SOE"
			}
			fmt.Printf("[RTDB] Tag: %s | Value: %t | Quality: %s | Time: %s%s\n",
				st.TagID, st.State, st.Quality, formatTime(st.TimestampMs), soe)
		}
		c.engine.EvaluateDigital(st.TagID, st.State, st.Quality, st.TimestampMs)
	}
	c.telemetry.Publish(msg)
}

// onAlarmEvent is the engine sink: it logs every transition and fans it out.
// It runs under the engine lock, so it must stay fast and non-blocking.
func (c *core) onAlarmEvent(ev *scadav1.AlarmEvent) {
	c.alarms.Publish(ev)

	var kind string
	switch ev.GetState() {
	case scadav1.AlarmState_ALARM_UNACK_ALARM:
		kind = "RAISED"
	case scadav1.AlarmState_ALARM_ACK_ALARM:
		kind = "ACKNOWLEDGED"
	case scadav1.AlarmState_ALARM_UNACK_RTN:
		kind = "RETURNED TO NORMAL (awaiting ack)"
	case scadav1.AlarmState_ALARM_ACK_RTN:
		kind = "CLEARED"
	}
	sev := strings.TrimPrefix(ev.GetSeverity().String(), "SEVERITY_")
	extra := ""
	if ev.GetAckByUser() != "" {
		extra = " | By: " + ev.GetAckByUser()
	}
	fmt.Printf("[ALARM] %s | ID: %s | Severity: %s | State: %s | Value: %.1f%s | %s | Time: %s\n",
		kind, ev.GetAlarmId(), sev, ev.GetState(), ev.GetTriggerValue(), extra,
		ev.GetMessage(), formatTime(alarm.EventTimeMs(ev)))
}

// --- alarm service ---------------------------------------------------------

type alarmService struct {
	scadav1.UnimplementedScadaAlarmServiceServer
	c *core
}

func (s *alarmService) AcknowledgeAlarm(_ context.Context, req *scadav1.AckAlarmRequest) (*scadav1.AckAlarmResponse, error) {
	if req.GetAlarmId() == "" {
		return nil, status.Error(codes.InvalidArgument, "alarm_id is required")
	}
	user := strings.TrimSpace(req.GetUser())
	if user == "" {
		return nil, status.Error(codes.InvalidArgument, "user is required")
	}
	ev, err := s.c.engine.Acknowledge(req.GetAlarmId(), user)
	switch {
	case errors.Is(err, alarm.ErrNotFound), errors.Is(err, alarm.ErrAlreadyAcknowledged):
		return &scadav1.AckAlarmResponse{Success: false, Message: err.Error()}, nil
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &scadav1.AckAlarmResponse{Success: true, Message: "acknowledged", Alarm: ev}, nil
}

func (s *alarmService) GetActiveAlarms(_ context.Context, req *scadav1.GetActiveAlarmsRequest) (*scadav1.GetActiveAlarmsResponse, error) {
	return &scadav1.GetActiveAlarmsResponse{
		Alarms: s.c.engine.ActiveAlarms(req.GetTagId(), req.GetMinSeverity()),
	}, nil
}

func (s *alarmService) SubscribeAlarmEvents(req *scadav1.SubscribeRequest, stream scadav1.ScadaAlarmService_SubscribeAlarmEventsServer) error {
	sub := s.c.alarms.Subscribe(1024)
	defer sub.Cancel()
	log.Printf("[CORE] alarm subscriber %q connected", req.GetSubscriberId())
	defer log.Printf("[CORE] alarm subscriber %q disconnected (dropped %d)", req.GetSubscriberId(), sub.Dropped())
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev, ok := <-sub.C:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

// --- data distribution service ---------------------------------------------

type dataService struct {
	scadav1.UnimplementedScadaDataServiceServer
	c *core
}

func (s *dataService) GetCurrentTelemetry(_ context.Context, _ *scadav1.GetCurrentTelemetryRequest) (*scadav1.GetCurrentTelemetryResponse, error) {
	snap := s.c.db.Snapshot()
	out := make([]*scadav1.TagSnapshot, 0, len(snap))
	for _, t := range snap {
		out = append(out, &scadav1.TagSnapshot{
			TagId: t.TagID, IsDigital: t.IsDigital, Value: t.Value, RawValue: t.RawValue,
			State: t.State, EngineeringUnit: t.Unit, Quality: t.Quality,
			TimestampUtcMs: t.TimestampMs, UpdateCount: t.UpdateCount,
			IsSoe: t.IsSOE, SourceFepId: t.SourceFEP,
		})
	}
	return &scadav1.GetCurrentTelemetryResponse{Tags: out, ServerTimeUtcMs: time.Now().UTC().UnixMilli()}, nil
}

func (s *dataService) SubscribeTelemetry(req *scadav1.SubscribeRequest, stream scadav1.ScadaDataService_SubscribeTelemetryServer) error {
	sub := s.c.telemetry.Subscribe(4096)
	defer sub.Cancel()
	log.Printf("[CORE] telemetry subscriber %q connected", req.GetSubscriberId())
	defer func() {
		log.Printf("[CORE] telemetry subscriber %q disconnected (dropped %d)", req.GetSubscriberId(), sub.Dropped())
	}()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case msg, ok := <-sub.C:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

// --- runtime configuration service ------------------------------------------

var (
	stationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,64}$`)
	tagPattern       = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,128}$`)
)

const maxBoundTags = 5000

type configService struct {
	scadav1.UnimplementedScadaConfigServiceServer
	c *core
}

func (s *configService) DeployTopology(_ context.Context, req *scadav1.DeployTopologyRequest) (*scadav1.DeployTopologyResponse, error) {
	if !stationIDPattern.MatchString(req.GetStationId()) {
		return nil, status.Error(codes.InvalidArgument, "invalid station_id")
	}
	if req.GetVersion() < 1 {
		return nil, status.Error(codes.InvalidArgument, "version must be >= 1")
	}
	if strings.TrimSpace(req.GetDeployedBy()) == "" {
		return nil, status.Error(codes.InvalidArgument, "deployed_by is required")
	}
	if len(req.GetBoundTags()) > maxBoundTags {
		return nil, status.Errorf(codes.InvalidArgument, "too many bound tags (max %d)", maxBoundTags)
	}
	seen := make(map[string]struct{}, len(req.GetBoundTags()))
	tags := make([]string, 0, len(req.GetBoundTags()))
	for _, t := range req.GetBoundTags() {
		if !tagPattern.MatchString(t) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid tag id %q", t)
		}
		if _, dup := seen[t]; !dup {
			seen[t] = struct{}{}
			tags = append(tags, t)
		}
	}
	sort.Strings(tags)

	c := s.c
	c.cfgMu.Lock()
	if cur := c.configs[req.GetStationId()]; cur != nil && req.GetVersion() < cur.GetVersion() {
		c.cfgMu.Unlock()
		return &scadav1.DeployTopologyResponse{
			Accepted: false, Version: cur.GetVersion(),
			Message: fmt.Sprintf("stale version %d: version %d is already deployed", req.GetVersion(), cur.GetVersion()),
		}, nil
	}
	cfg := &scadav1.RuntimeConfig{
		StationId: req.GetStationId(), Version: req.GetVersion(), DeployedBy: strings.TrimSpace(req.GetDeployedBy()),
		DeployedAtUtcMs: time.Now().UTC().UnixMilli(), BoundTags: tags,
	}
	c.configs[cfg.StationId] = cfg
	c.cfgMu.Unlock()

	var live, unresolved []string
	for _, t := range tags {
		if _, ok := c.db.Get(t); ok {
			live = append(live, t)
		} else {
			unresolved = append(unresolved, t)
		}
	}
	notified := c.cfgBus.Subscribers()
	c.cfgBus.Publish(cfg)
	log.Printf("[CORE] topology %s v%d deployed by %s: %d components, %d pipelines, %d bound tags (%d live, %d unresolved), %d subscriber(s) notified",
		cfg.StationId, cfg.Version, cfg.DeployedBy, req.GetComponentCount(), req.GetPipelineCount(), len(tags), len(live), len(unresolved), notified)
	return &scadav1.DeployTopologyResponse{
		Accepted: true, Message: "configuration applied", Version: cfg.Version,
		LiveTags: live, UnresolvedTags: unresolved, SubscribersNotified: int32(notified),
	}, nil
}

func (c *core) configSnapshot() []*scadav1.RuntimeConfig {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	out := make([]*scadav1.RuntimeConfig, 0, len(c.configs))
	for _, cfg := range c.configs {
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StationId < out[j].StationId })
	return out
}

func (s *configService) GetRuntimeConfigs(_ context.Context, _ *scadav1.GetRuntimeConfigsRequest) (*scadav1.GetRuntimeConfigsResponse, error) {
	return &scadav1.GetRuntimeConfigsResponse{Configs: s.c.configSnapshot()}, nil
}

func (s *configService) SubscribeConfig(req *scadav1.SubscribeRequest, stream scadav1.ScadaConfigService_SubscribeConfigServer) error {
	// Subscribe before reading the snapshot so a deployment in between is not lost;
	// receiving one config twice is harmless because configs are versioned.
	sub := s.c.cfgBus.Subscribe(64)
	defer sub.Cancel()
	log.Printf("[CORE] config subscriber %q connected", req.GetSubscriberId())
	defer log.Printf("[CORE] config subscriber %q disconnected", req.GetSubscriberId())
	for _, cfg := range s.c.configSnapshot() {
		if err := stream.Send(cfg); err != nil {
			return err
		}
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case cfg, ok := <-sub.C:
			if !ok {
				return nil
			}
			if err := stream.Send(cfg); err != nil {
				return err
			}
		}
	}
}

func main() {
	addr := flag.String("listen", "0.0.0.0:50051", "gRPC listen address")
	station := flag.String("station", "TEH_CGS_01", "tag prefix used for the built-in alarm rules")
	logRTDB := flag.Bool("log-rtdb", true, "print one [RTDB] line per tag update")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("[CORE] listen %s: %v", *addr, err)
	}

	c := &core{
		db:        rtdb.New(),
		engine:    alarm.NewEngine(),
		telemetry: bus.New[*scadav1.TelemetryStreamUpdate](),
		alarms:    bus.New[*scadav1.AlarmEvent](),
		logRTDB:   *logRTDB,
		configs:   make(map[string]*scadav1.RuntimeConfig),
		cfgBus:    bus.New[*scadav1.RuntimeConfig](),
	}
	if err := alarm.DefaultRules(c.engine, *station); err != nil {
		log.Fatalf("[CORE] alarm rules: %v", err)
	}
	c.engine.SetSink(c.onAlarmEvent)

	gs := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	scadav1.RegisterScadaFepCoreServiceServer(gs, &fepService{c: c})
	scadav1.RegisterScadaAlarmServiceServer(gs, &alarmService{c: c})
	scadav1.RegisterScadaDataServiceServer(gs, &dataService{c: c})
	scadav1.RegisterScadaConfigServiceServer(gs, &configService{c: c})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- gs.Serve(lis) }()
	log.Printf("[CORE] gRPC server listening on %s (alarm rules loaded for %s)", *addr, *station)

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("[CORE] serve: %v", err)
		}
	case <-ctx.Done():
		raised, acked, cleared, active := c.engine.Stats()
		log.Printf("[CORE] shutting down (%d tags in RTDB; alarms raised=%d acked=%d cleared=%d active=%d)",
			c.db.Len(), raised, acked, cleared, active)
		done := make(chan struct{})
		go func() { gs.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			gs.Stop()
		}
	}
	log.Printf("[CORE] stopped")
}
