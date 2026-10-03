package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Gateway close codes sent by the hub. The API layer defines the protocol-level ones.
const (
	CloseGoingAway      = 1001
	CloseResync         = 4007
	CloseTooSlow        = 4008
	CloseSessionRevoked = 4010
)

// Resolver answers the permission questions the hub needs while routing.
type Resolver interface {
	// VisibleChannels returns, for each user, the categories and text channels of the
	// place they can view. Users who are not members map to an empty set.
	VisibleChannels(ctx context.Context, placeID uuid.UUID, userIDs []uuid.UUID) (map[uuid.UUID]map[uuid.UUID]bool, error)
	// PresenceAudience lists who should hear about a user's presence: their places and
	// the people they share direct messages with.
	PresenceAudience(ctx context.Context, userID uuid.UUID) (places, users []uuid.UUID, err error)
}

// Client is one identified gateway connection.
type Client struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	ID        string

	out       chan []byte
	done      chan struct{}
	closeOnce sync.Once
	code      int
	reason    string

	// Guarded by Hub.mu.
	places     map[uuid.UUID]struct{}
	status     string
	registered bool
}

// NewClient prepares a client that can buffer up to queue frames.
func NewClient(userID, sessionID uuid.UUID, status string, queue int) *Client {
	return &Client{
		UserID: userID, SessionID: sessionID, ID: uuid.NewString(),
		out: make(chan []byte, queue), done: make(chan struct{}),
		places: map[uuid.UUID]struct{}{}, status: status,
	}
}

// Out yields encoded frames for the connection to write.
func (c *Client) Out() <-chan []byte { return c.out }

// Done closes when the hub disconnects the client; CloseInfo then explains why.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) CloseInfo() (code int, reason string) { return c.code, c.reason }

// Close asks the connection to close with the given code. Only the first call counts.
func (c *Client) Close(code int, reason string) {
	c.closeOnce.Do(func() {
		c.code, c.reason = code, reason
		close(c.done)
	})
}

// Send queues a frame without blocking. A client that falls too far behind is
// disconnected rather than slowing everyone else down.
func (c *Client) Send(frame []byte) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.out <- frame:
	default:
		c.Close(CloseTooSlow, "client is not reading events fast enough")
	}
}

// Hub routes envelopes from the broker to this replica's clients.
type Hub struct {
	broker   Broker
	presence Presence
	resolver Resolver
	log      *slog.Logger

	mu      sync.Mutex
	clients map[*Client]struct{}
	users   map[uuid.UUID]map[*Client]struct{}
	places  map[uuid.UUID]map[*Client]struct{}
	// vis caches place -> user -> visible channel IDs.
	vis    map[uuid.UUID]map[uuid.UUID]map[uuid.UUID]bool
	closed bool

	cancel context.CancelFunc
	wg     sync.WaitGroup
	// active counts registered clients that have not unregistered yet.
	active sync.WaitGroup
}

func NewHub(broker Broker, presence Presence, resolver Resolver, log *slog.Logger) *Hub {
	return &Hub{
		broker: broker, presence: presence, resolver: resolver, log: log,
		clients: map[*Client]struct{}{},
		users:   map[uuid.UUID]map[*Client]struct{}{},
		places:  map[uuid.UUID]map[*Client]struct{}{},
		vis:     map[uuid.UUID]map[uuid.UUID]map[uuid.UUID]bool{},
	}
}

// Start subscribes to the broker and begins routing. It returns once the subscription is
// active so no event published afterwards is missed.
func (h *Hub) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	msgs, err := h.broker.Subscribe(ctx)
	if err != nil {
		cancel()
		return err
	}
	h.cancel = cancel
	h.wg.Add(2)
	go func() {
		defer h.wg.Done()
		for raw := range msgs {
			h.dispatch(ctx, raw)
		}
	}()
	go func() {
		defer h.wg.Done()
		t := time.NewTicker(presenceTTL / 3)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := h.presence.Refresh(ctx, h.entries()); err != nil {
					h.log.Warn("refreshing presence", "error", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

// Close disconnects every client, waits briefly for their connections to wind down (so
// their presence is cleared and announced), then stops routing.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	for c := range h.clients {
		c.Close(CloseGoingAway, "server is shutting down")
	}
	h.mu.Unlock()
	drained := make(chan struct{})
	go func() {
		h.active.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		h.log.Warn("gateway connections did not close in time")
	}
	if h.cancel != nil {
		h.cancel()
	}
	h.wg.Wait()
}

// Publish sends an envelope to every replica.
func (h *Hub) Publish(ctx context.Context, env Envelope) error {
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return h.broker.Publish(ctx, raw)
}

func (h *Hub) entries() []PresenceEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]PresenceEntry, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, PresenceEntry{UserID: c.UserID, ConnID: c.ID, Status: c.status})
	}
	return out
}

// Register adds an identified client that belongs to places.
func (h *Hub) Register(ctx context.Context, c *Client, places []uuid.UUID) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.Close(CloseGoingAway, "server is shutting down")
		return
	}
	h.active.Add(1)
	c.registered = true
	h.clients[c] = struct{}{}
	addTo(h.users, c.UserID, c)
	for _, p := range places {
		c.places[p] = struct{}{}
		addTo(h.places, p, c)
	}
	status := c.status
	h.mu.Unlock()
	h.updatePresence(ctx, c.UserID, func() error {
		return h.presence.Set(ctx, PresenceEntry{UserID: c.UserID, ConnID: c.ID, Status: status})
	})
}

// Unregister removes a client after its connection ends. Calling it for a client that is
// not registered does nothing.
func (h *Hub) Unregister(ctx context.Context, c *Client) {
	h.mu.Lock()
	if !c.registered {
		h.mu.Unlock()
		return
	}
	c.registered = false
	delete(h.clients, c)
	removeFrom(h.users, c.UserID, c)
	for p := range c.places {
		removeFrom(h.places, p, c)
	}
	h.mu.Unlock()
	defer h.active.Done()
	c.Close(CloseGoingAway, "")
	h.updatePresence(ctx, c.UserID, func() error { return h.presence.Remove(ctx, c.UserID, c.ID) })
}

// SetStatus changes a client's reported status.
func (h *Hub) SetStatus(ctx context.Context, c *Client, status string) {
	h.mu.Lock()
	c.status = status
	h.mu.Unlock()
	h.updatePresence(ctx, c.UserID, func() error {
		return h.presence.Set(ctx, PresenceEntry{UserID: c.UserID, ConnID: c.ID, Status: status})
	})
}

// Presences returns aggregated statuses.
func (h *Hub) Presences(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	return h.presence.Get(ctx, userIDs)
}

// updatePresence applies change and announces the user's new presence if it changed.
func (h *Hub) updatePresence(ctx context.Context, userID uuid.UUID, change func() error) {
	before, err := h.presence.Get(ctx, []uuid.UUID{userID})
	if err != nil {
		h.log.Warn("reading presence", "error", err)
		return
	}
	if err := change(); err != nil {
		h.log.Warn("updating presence", "error", err)
		return
	}
	after, err := h.presence.Get(ctx, []uuid.UUID{userID})
	if err != nil || after[userID] == before[userID] {
		return
	}
	places, users, err := h.resolver.PresenceAudience(ctx, userID)
	if err != nil {
		h.log.Warn("resolving presence audience", "error", err)
		return
	}
	data, _ := json.Marshal(map[string]any{"user_id": userID, "status": after[userID]})
	env := Envelope{Type: "PRESENCE_UPDATE", Data: data, Users: append(users, userID), Places: places}
	if err := h.Publish(ctx, env); err != nil {
		h.log.Warn("publishing presence", "error", err)
	}
}

func addTo(m map[uuid.UUID]map[*Client]struct{}, key uuid.UUID, c *Client) {
	if m[key] == nil {
		m[key] = map[*Client]struct{}{}
	}
	m[key][c] = struct{}{}
}

func removeFrom(m map[uuid.UUID]map[*Client]struct{}, key uuid.UUID, c *Client) {
	delete(m[key], c)
	if len(m[key]) == 0 {
		delete(m, key)
	}
}

type frame struct {
	Op   string          `json:"op"`
	Type string          `json:"t"`
	Data json.RawMessage `json:"d"`
}

// EncodeDispatch builds the frame clients receive for an event.
func EncodeDispatch(eventType string, data json.RawMessage) []byte {
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	raw, _ := json.Marshal(frame{Op: "dispatch", Type: eventType, Data: data})
	return raw
}

func (h *Hub) dispatch(ctx context.Context, raw []byte) {
	if raw == nil {
		h.resync()
		return
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		h.log.Warn("dropping malformed event", "error", err)
		return
	}
	if env.Control != nil {
		h.apply(*env.Control)
	}
	if env.Type == "" {
		return
	}
	out := EncodeDispatch(env.Type, env.Data)
	for c := range h.targets(ctx, env) {
		c.Send(out)
	}
}

// resync recovers from possibly missed events (e.g. a Redis reconnect). Membership and
// permission changes may have been lost, so every cache is dropped and clients are asked
// to reconnect, which reloads their state from the database.
func (h *Hub) resync() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log.Warn("event subscription was re-established; reconnecting gateway clients", "clients", len(h.clients))
	h.vis = map[uuid.UUID]map[uuid.UUID]map[uuid.UUID]bool{}
	for c := range h.clients {
		c.Close(CloseResync, "events may have been missed; reconnect")
	}
}

func (h *Hub) apply(ctl Control) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ctl.PlaceID != nil {
		pid := *ctl.PlaceID
		if ctl.Invalidate {
			delete(h.vis, pid)
		}
		for _, u := range ctl.Refresh {
			delete(h.vis[pid], u)
		}
		for _, u := range ctl.Joined {
			for c := range h.users[u] {
				c.places[pid] = struct{}{}
				addTo(h.places, pid, c)
			}
			delete(h.vis[pid], u)
		}
		for _, u := range ctl.Left {
			for c := range h.users[u] {
				delete(c.places, pid)
				removeFrom(h.places, pid, c)
			}
			delete(h.vis[pid], u)
		}
		if ctl.Removed {
			for c := range h.places[pid] {
				delete(c.places, pid)
			}
			delete(h.places, pid)
			delete(h.vis, pid)
		}
	}
	if ctl.CloseUser != nil {
		revoked := map[uuid.UUID]bool{}
		for _, s := range ctl.CloseSessions {
			revoked[s] = true
		}
		for c := range h.users[*ctl.CloseUser] {
			keep := ctl.KeepSession != nil && c.SessionID == *ctl.KeepSession
			if (len(revoked) == 0 && !keep) || revoked[c.SessionID] {
				c.Close(CloseSessionRevoked, "session has ended")
			}
		}
	}
}

// targets resolves an envelope's recipients on this replica.
func (h *Hub) targets(ctx context.Context, env Envelope) map[*Client]struct{} {
	out := map[*Client]struct{}{}
	h.mu.Lock()
	for _, u := range env.Users {
		for c := range h.users[u] {
			out[c] = struct{}{}
		}
	}
	if env.ChannelID == nil || len(env.Places) == 0 {
		for _, p := range env.Places {
			for c := range h.places[p] {
				out[c] = struct{}{}
			}
		}
		h.mu.Unlock()
		return out
	}

	pid, cid := env.Places[0], *env.ChannelID
	candidates := make([]*Client, 0, len(h.places[pid]))
	present := map[uuid.UUID]bool{}
	var missing []uuid.UUID
	for c := range h.places[pid] {
		candidates = append(candidates, c)
		if _, ok := h.vis[pid][c.UserID]; !ok && !present[c.UserID] {
			missing = append(missing, c.UserID)
		}
		present[c.UserID] = true
	}
	h.mu.Unlock()

	if len(missing) > 0 {
		resolved, err := h.resolver.VisibleChannels(ctx, pid, missing)
		if err != nil {
			h.log.Error("resolving channel visibility", "place", pid, "error", err)
			return out
		}
		h.mu.Lock()
		if h.vis[pid] == nil {
			h.vis[pid] = map[uuid.UUID]map[uuid.UUID]bool{}
		}
		for _, u := range missing {
			h.vis[pid][u] = resolved[u]
			if h.vis[pid][u] == nil {
				h.vis[pid][u] = map[uuid.UUID]bool{}
			}
		}
		// Forget users with no connection here any more.
		for u := range h.vis[pid] {
			if !present[u] {
				delete(h.vis[pid], u)
			}
		}
		h.mu.Unlock()
	}

	h.mu.Lock()
	for _, c := range candidates {
		if h.vis[pid][c.UserID][cid] {
			out[c] = struct{}{}
		}
	}
	h.mu.Unlock()
	return out
}
