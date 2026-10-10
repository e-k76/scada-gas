// Command core is the central SCADA server: it accepts telemetry streams
// from FEPs over gRPC, stores last-known values in the RTDB and logs them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/rtdb"
)

type server struct {
	scadav1.UnimplementedScadaFepCoreServiceServer
	db      *rtdb.RTDB
	streams atomic.Int64
}

func formatTime(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func (s *server) StreamTelemetry(stream scadav1.ScadaFepCoreService_StreamTelemetryServer) error {
	remote := "unknown"
	if p, ok := peer.FromContext(stream.Context()); ok {
		remote = p.Addr.String()
	}
	n := s.streams.Add(1)
	defer s.streams.Add(-1)
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
		s.ingest(msg)
	}
}

func (s *server) ingest(msg *scadav1.TelemetryStreamUpdate) {
	fep := msg.GetSourceFepId()
	for _, a := range msg.GetAnalogUpdates() {
		st := s.db.UpdateAnalog(fep, a)
		fmt.Printf("[RTDB] Tag: %s | Value: %.1f %s | Quality: %s | Time: %s\n",
			st.TagID, st.Value, st.Unit, st.Quality, formatTime(st.TimestampMs))
	}
	for _, d := range msg.GetDigitalUpdates() {
		st := s.db.UpdateDigital(fep, d)
		soe := ""
		if st.IsSOE {
			soe = " | SOE"
		}
		fmt.Printf("[RTDB] Tag: %s | Value: %t | Quality: %s | Time: %s%s\n",
			st.TagID, st.State, st.Quality, formatTime(st.TimestampMs), soe)
	}
}

func main() {
	addr := flag.String("listen", "0.0.0.0:50051", "gRPC listen address")
	flag.Parse()

	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("[CORE] listen %s: %v", *addr, err)
	}

	db := rtdb.New()
	gs := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	scadav1.RegisterScadaFepCoreServiceServer(gs, &server{db: db})

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- gs.Serve(lis) }()
	log.Printf("[CORE] gRPC server listening on %s", *addr)

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("[CORE] serve: %v", err)
		}
	case <-ctx.Done():
		log.Printf("[CORE] shutting down (%d tags in RTDB)", db.Len())
		done := make(chan struct{})
		go func() { gs.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			gs.Stop()
		}
	}
	fmt.Fprintln(os.Stderr, "[CORE] stopped")
}
