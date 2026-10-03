// Package realtime fans events out to connected gateway clients. Events travel through a
// Broker (in-process, or Redis pub/sub so every replica sees every event); each replica's
// Hub then routes them to its own WebSocket clients, checking channel visibility with a
// cache it keeps per place.
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Envelope is one event plus its routing, as it travels between replicas.
type Envelope struct {
	// Type is the dispatch event name sent to clients, e.g. MESSAGE_CREATE. Envelopes
	// without a type only carry Control.
	Type string          `json:"t,omitempty"`
	Data json.RawMessage `json:"d,omitempty"`
	// Users receive the event on every session.
	Users []uuid.UUID `json:"u,omitempty"`
	// Members of any of Places receive the event...
	Places []uuid.UUID `json:"p,omitempty"`
	// ...or, when ChannelID is set, only members of Places[0] who can view that channel
	// (a category or text channel; thread events use their parent channel).
	ChannelID *uuid.UUID `json:"c,omitempty"`
	Control   *Control   `json:"x,omitempty"`
}

// Control updates hub state before the envelope's event (if any) is routed.
type Control struct {
	PlaceID *uuid.UUID `json:"place_id,omitempty"`
	// Invalidate drops every cached visibility for PlaceID: channels, overwrites, roles or
	// ownership changed.
	Invalidate bool `json:"invalidate,omitempty"`
	// Refresh drops cached visibility for these users in PlaceID (their roles changed).
	Refresh []uuid.UUID `json:"refresh,omitempty"`
	// Joined and Left add or remove PlaceID from these users' connections.
	Joined []uuid.UUID `json:"joined,omitempty"`
	Left   []uuid.UUID `json:"left,omitempty"`
	// Removed detaches PlaceID from every connection (the place was deleted).
	Removed bool `json:"removed,omitempty"`
	// CloseUser disconnects the user's sessions listed in CloseSessions, or every session
	// except KeepSession when CloseSessions is empty.
	CloseUser     *uuid.UUID  `json:"close_user,omitempty"`
	CloseSessions []uuid.UUID `json:"close_sessions,omitempty"`
	KeepSession   *uuid.UUID  `json:"keep_session,omitempty"`
}

// Broker carries encoded envelopes to every replica's hub.
type Broker interface {
	Publish(ctx context.Context, payload []byte) error
	// Subscribe returns once the subscription is active. The channel closes when ctx ends.
	// A nil payload means the subscription was re-established and events may have been
	// missed in between.
	Subscribe(ctx context.Context) (<-chan []byte, error)
	Name() string
}

// NewMemoryBroker delivers envelopes within this process only, which is correct for a
// single replica.
func NewMemoryBroker() Broker {
	return &memoryBroker{ch: make(chan []byte, 4096), done: make(chan struct{})}
}

type memoryBroker struct {
	ch       chan []byte
	done     chan struct{}
	stopOnce sync.Once
}

func (b *memoryBroker) Name() string { return "memory" }

func (b *memoryBroker) Publish(ctx context.Context, payload []byte) error {
	select {
	case b.ch <- payload:
		return nil
	case <-b.done:
		return errors.New("realtime: broker stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *memoryBroker) Subscribe(ctx context.Context) (<-chan []byte, error) {
	out := make(chan []byte)
	go func() {
		defer close(out)
		defer b.stopOnce.Do(func() { close(b.done) })
		for {
			select {
			case p := <-b.ch:
				select {
				case out <- p:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

const redisChannel = "gotalk:events"

// NewRedisBroker fans envelopes out to every replica through Redis pub/sub.
func NewRedisBroker(rdb *redis.Client) Broker { return &redisBroker{rdb: rdb} }

type redisBroker struct{ rdb *redis.Client }

func (b *redisBroker) Name() string { return "redis" }

func (b *redisBroker) Publish(ctx context.Context, payload []byte) error {
	return b.rdb.Publish(ctx, redisChannel, payload).Err()
}

func (b *redisBroker) Subscribe(ctx context.Context) (<-chan []byte, error) {
	ps := b.rdb.Subscribe(ctx, redisChannel)
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, err
	}
	out := make(chan []byte)
	go func() {
		defer close(out)
		defer func() { _ = ps.Close() }()
		// go-redis resubscribes automatically after reconnecting and reports it as a new
		// subscription; anything published while disconnected is lost.
		msgs := ps.ChannelWithSubscriptions(redis.WithChannelSize(4096))
		for {
			select {
			case m, ok := <-msgs:
				if !ok {
					return
				}
				var payload []byte
				switch v := m.(type) {
				case *redis.Message:
					payload = []byte(v.Payload)
				case *redis.Subscription:
					if v.Kind != "subscribe" {
						continue
					}
				default:
					continue
				}
				select {
				case out <- payload:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
