// Command historian subscribes to the core's telemetry and alarm feeds,
// applies deadband filtering and persists the result in TimescaleDB.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/grpcutil"
	"github.com/example/scada-gas/pkg/historian"
)

const defaultDSN = "postgres://scada:scada@127.0.0.1:5432/scada?sslmode=disable"

func main() {
	coreAddr := flag.String("core", "127.0.0.1:50051", "core gRPC address")
	dsn := flag.String("dsn", envOr("HISTORIAN_DSN", defaultDSN), "PostgreSQL/TimescaleDB connection string (env HISTORIAN_DSN)")
	logOnly := flag.Bool("log-only", false, "print stored rows instead of writing to the database")
	deadband := flag.Float64("deadband", 0.1, "analog deadband in engineering units")
	heartbeat := flag.Duration("heartbeat", 60*time.Second, "store a sample at least this often per tag (0 disables)")
	batch := flag.Int("batch-size", 1000, "flush when this many rows are pending")
	flushEvery := flag.Duration("flush-interval", time.Second, "flush at least this often")
	statsEvery := flag.Duration("stats-interval", 10*time.Second, "how often to log ingestion statistics (0 disables)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var store historian.Store
	if *logOnly {
		store = historian.LogStore{}
		log.Printf("[HIST] log-only mode: rows are printed, not stored")
	} else {
		var err error
		for attempt := 1; ; attempt++ {
			cctx, ccancel := context.WithTimeout(ctx, 15*time.Second)
			store, err = historian.NewRepository(cctx, *dsn)
			ccancel()
			if err == nil {
				break
			}
			log.Printf("[HIST] database not ready (attempt %d): %v", attempt, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
		log.Printf("[HIST] connected to database")
	}
	defer store.Close()

	ing := historian.NewIngester(historian.IngesterConfig{
		Filter:        historian.FilterConfig{AnalogDeadband: *deadband, Heartbeat: *heartbeat},
		BatchSize:     *batch,
		FlushInterval: *flushEvery,
		MaxPending:    200000,
	}, store)

	conn, err := grpc.NewClient(*coreAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	)
	if err != nil {
		log.Fatalf("[HIST] grpc client: %v", err)
	}
	defer conn.Close()
	data := scadav1.NewScadaDataServiceClient(conn)
	alarms := scadav1.NewScadaAlarmServiceClient(conn)
	self := &scadav1.SubscribeRequest{SubscriberId: "historian"}

	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	run(func() { ing.Run(ctx) })
	run(func() {
		grpcutil.SubscribeLoop(ctx, "[HIST]", "telemetry", func(c context.Context) (func() error, error) {
			s, err := data.SubscribeTelemetry(c, self)
			if err != nil {
				return nil, err
			}
			return func() error {
				m, err := s.Recv()
				if err != nil {
					return err
				}
				ing.HandleTelemetry(m)
				return nil
			}, nil
		})
	})
	run(func() {
		grpcutil.SubscribeLoop(ctx, "[HIST]", "alarm", func(c context.Context) (func() error, error) {
			s, err := alarms.SubscribeAlarmEvents(c, self)
			if err != nil {
				return nil, err
			}
			return func() error {
				ev, err := s.Recv()
				if err != nil {
					return err
				}
				ing.HandleAlarm(ev)
				return nil
			}, nil
		})
	})
	if *statsEvery > 0 {
		run(func() {
			t := time.NewTicker(*statsEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					log.Printf("[HIST] %s", ing.Stats())
				}
			}
		})
	}

	log.Printf("[HIST] historian running: core=%s deadband=±%.2f heartbeat=%s", *coreAddr, *deadband, *heartbeat)
	<-ctx.Done()
	log.Printf("[HIST] shutting down")
	wg.Wait()
	log.Printf("[HIST] final: %s", ing.Stats())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
