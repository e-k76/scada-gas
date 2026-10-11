// Package grpcutil holds small helpers shared by the gRPC client daemons.
package grpcutil

import (
	"context"
	"log"
	"time"
)

// SubscribeLoop keeps a server-streaming subscription alive. open must start
// the stream and return a recv function that blocks for one message; recv is
// called repeatedly until it fails, after which the loop waits (exponential
// backoff, 1 s .. 15 s) and opens a new stream. It returns when ctx ends.
func SubscribeLoop(ctx context.Context, prefix, name string, open func(context.Context) (func() error, error)) {
	backoff := time.Second
	for ctx.Err() == nil {
		recv, err := open(ctx)
		if err == nil {
			log.Printf("%s %s subscription established", prefix, name)
			backoff = time.Second
			for {
				if err = recv(); err != nil {
					break
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("%s %s subscription lost: %v (retry in %s)", prefix, name, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
}
