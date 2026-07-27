// Package heartbeat is the shared WS dead-peer-detection loop both sides of
// the relay/1 persistent connection run: a protocol-level ping every
// interval, each round-trip bounded by timeout. A peer that stops servicing
// its connection — a half-open NAT-dropped socket, a power-cut relay, a
// process wedged off its read loop — stops answering pings, the round-trip
// times out, and the caller tears the connection down instead of leaking it
// (and its goroutines) forever. TCP keepalives do not cover this: they
// probe the kernel's socket, not whether the peer application still reads.
package heartbeat

import (
	"context"
	"time"
)

// Pinger is the subset of a WS connection a heartbeat loop drives —
// coder/websocket's Conn.Ping sends a ping and waits for the matching pong.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Run pings p every interval, bounding each ping+pong round-trip by
// timeout, until ctx is done or a round-trip fails. It returns nil when ctx
// ended the loop (an orderly shutdown) and the failing ping's error
// otherwise — the caller's signal to close the connection.
func Run(ctx context.Context, p Pinger, interval, timeout time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, timeout)
			err := p.Ping(pctx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil // shutdown raced the ping; not a peer failure
				}
				return err
			}
		}
	}
}
