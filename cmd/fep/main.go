// Command fep is the Front-End Processor: it polls a Modbus TCP device and
// streams the converted telemetry to the core server over a long-lived gRPC
// client stream, reconnecting with backoff and buffering while offline.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
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
	mbdrv "github.com/example/scada-gas/pkg/driver/modbus"
)

// buffer is a bounded FIFO that drops the oldest entry when full.
type buffer struct {
	mu      sync.Mutex
	items   []*scadav1.TelemetryStreamUpdate
	max     int
	dropped uint64
	notify  chan struct{}
}

func newBuffer(max int) *buffer {
	return &buffer{max: max, notify: make(chan struct{}, 1)}
}

func (b *buffer) push(u *scadav1.TelemetryStreamUpdate) {
	b.mu.Lock()
	if len(b.items) >= b.max {
		b.items = b.items[1:]
		b.dropped++
	}
	b.items = append(b.items, u)
	b.mu.Unlock()
	select {
	case b.notify <- struct{}{}:
	default:
	}
}

func (b *buffer) peek() *scadav1.TelemetryStreamUpdate {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) == 0 {
		return nil
	}
	return b.items[0]
}

func (b *buffer) pop() {
	b.mu.Lock()
	if len(b.items) > 0 {
		b.items = b.items[1:]
	}
	b.mu.Unlock()
}

func (b *buffer) stats() (int, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items), b.dropped
}

// pollLoop polls the device every interval and enqueues one update per cycle.
func pollLoop(ctx context.Context, drv *mbdrv.Driver, fepID string, interval time.Duration, buf *buffer) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var seq int64
	var lastErr string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		analogs, digitals, err := drv.Poll()
		if err != nil {
			if err.Error() != lastErr {
				log.Printf("[FEP] poll failed: %v (publishing QUALITY_BAD_COMM_FAILURE)", err)
				lastErr = err.Error()
			}
		} else if lastErr != "" {
			log.Printf("[FEP] device communication restored")
			lastErr = ""
		}
		seq++
		buf.push(&scadav1.TelemetryStreamUpdate{
			SourceFepId:    fepID,
			PacketSequence: seq,
			AnalogUpdates:  analogs,
			DigitalUpdates: digitals,
		})
	}
}

// streamLoop keeps a client stream to the core open and drains the buffer
// into it. An update is only removed from the buffer after Send succeeds.
func streamLoop(ctx context.Context, conn *grpc.ClientConn, buf *buffer) {
	client := scadav1.NewScadaFepCoreServiceClient(conn)
	backoff := time.Second
	const maxBackoff = 15 * time.Second

	for ctx.Err() == nil {
		// The stream context is detached from the shutdown signal so the final
		// ack can still be read after ctx is cancelled.
		streamCtx, cancelStream := context.WithCancel(context.WithoutCancel(ctx))
		stream, err := client.StreamTelemetry(streamCtx)
		if err != nil {
			cancelStream()
			log.Printf("[FEP] open stream failed: %v (retry in %s)", err, backoff)
			if !sleep(ctx, backoff) {
				break
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}
		log.Printf("[FEP] telemetry stream established")
		sent := 0

		for {
			u := buf.peek()
			if u == nil {
				select {
				case <-ctx.Done():
				case <-buf.notify:
					continue
				}
				break
			}
			if err := stream.Send(u); err != nil {
				if errors.Is(err, io.EOF) {
					_, err = stream.CloseAndRecv() // surfaces the real status
				}
				log.Printf("[FEP] stream broken after %d updates: %v", sent, err)
				break
			}
			buf.pop()
			sent++
			backoff = time.Second
		}

		if ctx.Err() != nil {
			defer cancelStream()
			// Graceful shutdown: close the send side and read the final ack.
			ack, err := stream.CloseAndRecv()
			if err == nil {
				log.Printf("[FEP] core acknowledged up to sequence %d (healthy=%t)", ack.GetLastProcessedSequence(), ack.GetIsHealthy())
			}
			return
		}
		cancelStream()
		if !sleep(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	if cur *= 2; cur > max {
		return max
	}
	return cur
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func main() {
	fepID := flag.String("id", "FEP_TEH_01", "FEP identifier")
	station := flag.String("station", "TEH_CGS_01", "tag prefix of the station")
	modbusAddr := flag.String("modbus", "127.0.0.1:1502", "Modbus TCP device address")
	unit := flag.Uint("unit", 1, "Modbus unit id")
	coreAddr := flag.String("core", "127.0.0.1:50051", "core gRPC address")
	interval := flag.Duration("interval", 500*time.Millisecond, "polling interval")
	bufSize := flag.Int("buffer", 7200, "max buffered updates while core is unreachable")
	flag.Parse()

	analogs, digitals := mbdrv.DefaultStationPoints(*station)
	drv, err := mbdrv.New(mbdrv.Config{
		Address:  *modbusAddr,
		UnitID:   uint8(*unit),
		Timeout:  2 * time.Second,
		Analogs:  analogs,
		Digitals: digitals,
	})
	if err != nil {
		log.Fatalf("[FEP] driver: %v", err)
	}
	defer drv.Close()

	conn, err := grpc.NewClient(*coreAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		log.Fatalf("[FEP] grpc client: %v", err)
	}
	defer conn.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	buf := newBuffer(*bufSize)
	log.Printf("[FEP] %s polling %s every %s, streaming to %s", *fepID, *modbusAddr, *interval, *coreAddr)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pollLoop(ctx, drv, *fepID, *interval, buf) }()
	go func() { defer wg.Done(); streamLoop(ctx, conn, buf) }()

	<-ctx.Done()
	log.Printf("[FEP] shutting down")
	wg.Wait()
	pending, dropped := buf.stats()
	log.Printf("[FEP] stopped (unsent=%d dropped=%d)", pending, dropped)
	os.Exit(0)
}
