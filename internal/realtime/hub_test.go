package realtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeResolver struct {
	mu      sync.Mutex
	visible map[uuid.UUID]map[uuid.UUID]bool
	calls   int
}

func (f *fakeResolver) VisibleChannels(_ context.Context, _ uuid.UUID, users []uuid.UUID) (map[uuid.UUID]map[uuid.UUID]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	out := map[uuid.UUID]map[uuid.UUID]bool{}
	for _, u := range users {
		out[u] = map[uuid.UUID]bool{}
		for ch := range f.visible[u] {
			out[u][ch] = true
		}
	}
	return out, nil
}

func (f *fakeResolver) PresenceAudience(context.Context, uuid.UUID) ([]uuid.UUID, []uuid.UUID, error) {
	return nil, nil, nil
}

func (f *fakeResolver) set(user, channel uuid.UUID, visible bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.visible[user] == nil {
		f.visible[user] = map[uuid.UUID]bool{}
	}
	if visible {
		f.visible[user][channel] = true
	} else {
		delete(f.visible[user], channel)
	}
}

func (f *fakeResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestHub(t *testing.T) (*Hub, *fakeResolver) {
	t.Helper()
	res := &fakeResolver{visible: map[uuid.UUID]map[uuid.UUID]bool{}}
	h := NewHub(NewMemoryBroker(), NewMemoryPresence(), res, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, h.Start(context.Background()))
	t.Cleanup(h.Close)
	return h, res
}

// next returns the next dispatch type a client receives, skipping presence updates.
func next(t *testing.T, c *Client) string {
	t.Helper()
	for {
		select {
		case raw := <-c.Out():
			var f struct {
				Type string `json:"t"`
			}
			require.NoError(t, json.Unmarshal(raw, &f))
			if f.Type != "PRESENCE_UPDATE" {
				return f.Type
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no event")
		}
	}
}

// register adds a client and unregisters it when the test ends, as a connection would.
func register(t *testing.T, h *Hub, c *Client, places []uuid.UUID) {
	t.Helper()
	h.Register(context.Background(), c, places)
	t.Cleanup(func() { h.Unregister(context.Background(), c) })
}

func publish(t *testing.T, h *Hub, env Envelope) {
	t.Helper()
	require.NoError(t, h.Publish(context.Background(), env))
}

func TestHubRoutesByChannelVisibility(t *testing.T) {
	h, res := newTestHub(t)
	place, general, secret := uuid.New(), uuid.New(), uuid.New()
	alice, bob := NewClient(uuid.New(), uuid.New(), StatusOnline, 16), NewClient(uuid.New(), uuid.New(), StatusOnline, 16)
	res.set(alice.UserID, general, true)
	res.set(alice.UserID, secret, true)
	res.set(bob.UserID, general, true)
	register(t, h, alice, []uuid.UUID{place})
	register(t, h, bob, []uuid.UUID{place})

	publish(t, h, Envelope{Type: "SECRET", Places: []uuid.UUID{place}, ChannelID: &secret})
	publish(t, h, Envelope{Type: "GENERAL", Places: []uuid.UUID{place}, ChannelID: &general})
	require.Equal(t, "SECRET", next(t, alice))
	require.Equal(t, "GENERAL", next(t, alice))
	require.Equal(t, "GENERAL", next(t, bob), "bob cannot see the secret channel")
	require.Equal(t, 1, res.callCount(), "visibility is resolved once per place and cached")

	// Permission changes only apply after invalidation.
	res.set(bob.UserID, secret, true)
	publish(t, h, Envelope{Control: &Control{PlaceID: &place, Refresh: []uuid.UUID{bob.UserID}}})
	publish(t, h, Envelope{Type: "SECRET", Places: []uuid.UUID{place}, ChannelID: &secret})
	require.Equal(t, "SECRET", next(t, bob))
	require.Equal(t, "SECRET", next(t, alice))

	// Leaving a place stops its events; user-addressed events still arrive.
	publish(t, h, Envelope{Type: "PLACE_LEAVE", Users: []uuid.UUID{bob.UserID},
		Control: &Control{PlaceID: &place, Left: []uuid.UUID{bob.UserID}}})
	require.Equal(t, "PLACE_LEAVE", next(t, bob))
	publish(t, h, Envelope{Type: "PLACE_WIDE", Places: []uuid.UUID{place}})
	publish(t, h, Envelope{Type: "DIRECT", Users: []uuid.UUID{bob.UserID}})
	require.Equal(t, "DIRECT", next(t, bob))
	require.Equal(t, "PLACE_WIDE", next(t, alice))

	// Joining adds the place back.
	publish(t, h, Envelope{Control: &Control{PlaceID: &place, Joined: []uuid.UUID{bob.UserID}}})
	publish(t, h, Envelope{Type: "PLACE_WIDE", Places: []uuid.UUID{place}})
	require.Equal(t, "PLACE_WIDE", next(t, bob))
}

func TestHubClosesRevokedSessions(t *testing.T) {
	h, _ := newTestHub(t)
	user := uuid.New()
	keep, revoke := NewClient(user, uuid.New(), StatusOnline, 16), NewClient(user, uuid.New(), StatusOnline, 16)
	register(t, h, keep, nil)
	register(t, h, revoke, nil)

	publish(t, h, Envelope{Control: &Control{CloseUser: &user, KeepSession: &keep.SessionID}})
	select {
	case <-revoke.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session was not closed")
	}
	code, _ := revoke.CloseInfo()
	require.Equal(t, CloseSessionRevoked, code)
	select {
	case <-keep.Done():
		t.Fatal("kept session was closed")
	default:
	}
	h.Unregister(context.Background(), revoke)
	h.Unregister(context.Background(), revoke)
}

func TestHubResyncsAfterMissedEvents(t *testing.T) {
	h, res := newTestHub(t)
	place, ch := uuid.New(), uuid.New()
	c := NewClient(uuid.New(), uuid.New(), StatusOnline, 16)
	res.set(c.UserID, ch, true)
	register(t, h, c, []uuid.UUID{place})
	publish(t, h, Envelope{Type: "X", Places: []uuid.UUID{place}, ChannelID: &ch})
	require.Equal(t, "X", next(t, c))

	// A nil payload from the broker means the subscription was re-established.
	h.dispatch(context.Background(), nil)
	<-c.Done()
	code, _ := c.CloseInfo()
	require.Equal(t, CloseResync, code)
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Empty(t, h.vis)
}

func TestSlowClientsAreDisconnected(t *testing.T) {
	c := NewClient(uuid.New(), uuid.New(), StatusOnline, 1)
	c.Send([]byte("one"))
	c.Send([]byte("two"))
	<-c.Done()
	code, _ := c.CloseInfo()
	require.Equal(t, CloseTooSlow, code)
}

func TestPresenceAggregation(t *testing.T) {
	ctx := context.Background()
	p := NewMemoryPresence()
	u := uuid.New()
	get := func() string {
		s, err := p.Get(ctx, []uuid.UUID{u})
		require.NoError(t, err)
		return s[u]
	}
	require.Equal(t, StatusOffline, get())
	require.NoError(t, p.Set(ctx, PresenceEntry{UserID: u, ConnID: "a", Status: StatusInvisible}))
	require.Equal(t, StatusOffline, get(), "invisible counts as offline")
	require.NoError(t, p.Set(ctx, PresenceEntry{UserID: u, ConnID: "b", Status: StatusIdle}))
	require.Equal(t, StatusIdle, get())
	require.NoError(t, p.Set(ctx, PresenceEntry{UserID: u, ConnID: "c", Status: StatusDND}))
	require.Equal(t, StatusDND, get())
	require.NoError(t, p.Set(ctx, PresenceEntry{UserID: u, ConnID: "b", Status: StatusOnline}))
	require.Equal(t, StatusOnline, get(), "the most present connection wins")
	require.NoError(t, p.Remove(ctx, u, "b"))
	require.Equal(t, StatusDND, get())
	require.False(t, ValidStatus(StatusOffline), "clients cannot claim offline; they use invisible")
}

func TestHubAnnouncesPresenceChanges(t *testing.T) {
	h, _ := newTestHub(t)
	watcher := NewClient(uuid.New(), uuid.New(), StatusOnline, 16)
	register(t, h, watcher, nil)
	// The fake audience is empty, so only the user's own sessions hear about it.
	c := NewClient(watcher.UserID, uuid.New(), StatusOnline, 16)
	register(t, h, c, nil)
	h.SetStatus(context.Background(), watcher, StatusIdle)
	h.SetStatus(context.Background(), c, StatusIdle)
	require.Eventually(t, func() bool {
		select {
		case raw := <-watcher.Out():
			var f struct {
				Type string         `json:"t"`
				Data map[string]any `json:"d"`
			}
			_ = json.Unmarshal(raw, &f)
			return f.Type == "PRESENCE_UPDATE" && f.Data["status"] == StatusIdle
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)
}
