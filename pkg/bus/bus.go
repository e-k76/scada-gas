// Package bus provides a small in-process publish/subscribe broker used by
// the core server to fan telemetry and alarm events out to gRPC subscribers.
package bus

import (
	"sync"
	"sync/atomic"
)

// Broker fans published values out to all current subscribers. Publishing
// never blocks: a subscriber whose buffer is full loses the value and its
// Dropped counter is incremented.
type Broker[T any] struct {
	mu   sync.RWMutex
	subs map[uint64]*Subscription[T]
	next uint64
}

// Subscription is a single consumer of a Broker.
type Subscription[T any] struct {
	C       <-chan T
	ch      chan T
	id      uint64
	broker  *Broker[T]
	dropped atomic.Uint64
}

// New creates an empty broker.
func New[T any]() *Broker[T] {
	return &Broker[T]{subs: make(map[uint64]*Subscription[T])}
}

// Subscribe registers a consumer with the given channel buffer size.
func (b *Broker[T]) Subscribe(buffer int) *Subscription[T] {
	if buffer < 1 {
		buffer = 1
	}
	ch := make(chan T, buffer)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	s := &Subscription[T]{C: ch, ch: ch, id: b.next, broker: b}
	b.subs[s.id] = s
	return s
}

// Publish delivers v to every subscriber without blocking.
func (b *Broker[T]) Publish(v T) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		select {
		case s.ch <- v:
		default:
			s.dropped.Add(1)
		}
	}
}

// Subscribers returns the current number of subscribers.
func (b *Broker[T]) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Dropped returns how many values this subscriber missed because it was slow.
func (s *Subscription[T]) Dropped() uint64 { return s.dropped.Load() }

// Cancel unregisters the subscription. It is safe to call more than once.
func (s *Subscription[T]) Cancel() {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	if _, ok := s.broker.subs[s.id]; ok {
		delete(s.broker.subs, s.id)
		close(s.ch)
	}
}
