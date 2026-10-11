// Command gateway is the northbound API server. It serves
//   - REST/JSON endpoints under /api/v1 (current values, alarms, acknowledge, trends),
//   - a WebSocket feed at /ws/telemetry with live tag and alarm updates, and
//   - the embedded web dashboard at /.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/gateway"
	"github.com/example/scada-gas/pkg/grpcutil"
	"github.com/example/scada-gas/pkg/historian"
	"github.com/example/scada-gas/web"
)

const defaultDSN = "postgres://scada:scada@127.0.0.1:5432/scada?sslmode=disable"

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", "0.0.0.0:8080", "HTTP listen address")
	coreAddr := flag.String("core", "127.0.0.1:50051", "core gRPC address")
	dsn := flag.String("dsn", envOr("HISTORIAN_DSN", defaultDSN), "TimescaleDB connection string for trends (env HISTORIAN_DSN); empty disables trends")
	token := flag.String("token", os.Getenv("GATEWAY_TOKEN"), "shared token required for /api/v1 and /ws (env GATEWAY_TOKEN); empty disables authentication")
	origins := flag.String("allowed-origins", "", "comma-separated extra WebSocket origins (e.g. http://localhost:3000); * allows all")
	maxWS := flag.Int("max-ws-clients", 200, "maximum concurrent WebSocket clients")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	conn, err := grpc.NewClient(*coreAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		log.Fatalf("[GW] grpc client: %v", err)
	}
	defer conn.Close()
	core := gateway.NewGRPCCore(conn)

	var trend gateway.TrendSource
	if *dsn != "" {
		rd, err := historian.NewReader(ctx, *dsn)
		if err != nil {
			log.Fatalf("[GW] historian reader: %v", err)
		}
		defer rd.Close()
		trend = rd
	} else {
		log.Printf("[GW] no DSN: trend endpoint disabled")
	}

	hcfg := gateway.DefaultHubConfig()
	hcfg.MaxClients = *maxWS
	for _, o := range strings.Split(*origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			hcfg.AllowedOrigins = append(hcfg.AllowedOrigins, o)
		}
	}
	hub := gateway.NewHub(hcfg, core)

	api := gateway.NewAPI(gateway.APIConfig{Core: core, Trend: trend, Hub: hub, Static: web.FS, Token: *token})
	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: it would also cut long-lived WebSocket writes;
		// the hub applies its own per-write deadlines.
	}

	// Feed the hub from the core's live streams (auto-reconnecting).
	data := scadav1.NewScadaDataServiceClient(conn)
	alarms := scadav1.NewScadaAlarmServiceClient(conn)
	self := &scadav1.SubscribeRequest{SubscriberId: "gateway"}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		grpcutil.SubscribeLoop(ctx, "[GW]", "telemetry", func(c context.Context) (func() error, error) {
			s, err := data.SubscribeTelemetry(c, self)
			if err != nil {
				return nil, err
			}
			return func() error {
				m, err := s.Recv()
				if err != nil {
					return err
				}
				hub.BroadcastTelemetry(m)
				return nil
			}, nil
		})
	}()
	go func() {
		defer wg.Done()
		grpcutil.SubscribeLoop(ctx, "[GW]", "alarm", func(c context.Context) (func() error, error) {
			s, err := alarms.SubscribeAlarmEvents(c, self)
			if err != nil {
				return nil, err
			}
			return func() error {
				ev, err := s.Recv()
				if err != nil {
					return err
				}
				hub.BroadcastAlarm(ev)
				return nil
			}, nil
		})
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	auth := "disabled"
	if *token != "" {
		auth = "token required"
	}
	log.Printf("[GW] listening on http://%s (core=%s, auth=%s, trends=%t)", *listen, *coreAddr, auth, trend != nil)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[GW] http server: %v", err)
		}
	case <-ctx.Done():
	}

	log.Printf("[GW] shutting down")
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = srv.Shutdown(sctx) // stops accepting, finishes plain HTTP requests
	hub.Close()            // WebSocket connections are hijacked, so close them explicitly
	wg.Wait()
	st := hub.Stats()
	log.Printf("[GW] stopped (ws connects=%d messages=%d slow disconnects=%d)", st.Connects, st.Messages, st.Slow)
}
