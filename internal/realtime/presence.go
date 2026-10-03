package realtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Statuses a connection may report. A user's presence is the most present status across
// their connections; invisible connections count as offline.
const (
	StatusOnline    = "online"
	StatusIdle      = "idle"
	StatusDND       = "dnd"
	StatusInvisible = "invisible"
	StatusOffline   = "offline"
)

var statusRank = map[string]int{StatusOnline: 3, StatusDND: 2, StatusIdle: 1}

// ValidStatus reports whether a client may set status.
func ValidStatus(status string) bool {
	switch status {
	case StatusOnline, StatusIdle, StatusDND, StatusInvisible:
		return true
	}
	return false
}

// aggregate picks the most present status, or offline.
func aggregate(statuses []string) string {
	best, rank := StatusOffline, 0
	for _, s := range statuses {
		if r := statusRank[s]; r > rank {
			best, rank = s, r
		}
	}
	return best
}

// PresenceEntry is one connection's reported status.
type PresenceEntry struct {
	UserID uuid.UUID
	ConnID string
	Status string
}

// Presence stores connection statuses so any replica can answer "is this user online?".
type Presence interface {
	Set(ctx context.Context, e PresenceEntry) error
	Remove(ctx context.Context, userID uuid.UUID, connID string) error
	// Get returns the aggregated status of each user, offline when not connected.
	Get(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error)
	// Refresh keeps live connections from expiring.
	Refresh(ctx context.Context, entries []PresenceEntry) error
}

// NewMemoryPresence tracks presence within this process.
func NewMemoryPresence() Presence {
	return &memoryPresence{users: map[uuid.UUID]map[string]string{}}
}

type memoryPresence struct {
	mu    sync.Mutex
	users map[uuid.UUID]map[string]string
}

func (m *memoryPresence) Set(_ context.Context, e PresenceEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.users[e.UserID] == nil {
		m.users[e.UserID] = map[string]string{}
	}
	m.users[e.UserID][e.ConnID] = e.Status
	return nil
}

func (m *memoryPresence) Remove(_ context.Context, userID uuid.UUID, connID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.users[userID], connID)
	if len(m.users[userID]) == 0 {
		delete(m.users, userID)
	}
	return nil
}

func (m *memoryPresence) Get(_ context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[uuid.UUID]string, len(userIDs))
	for _, id := range userIDs {
		statuses := make([]string, 0, len(m.users[id]))
		for _, s := range m.users[id] {
			statuses = append(statuses, s)
		}
		out[id] = aggregate(statuses)
	}
	return out, nil
}

func (m *memoryPresence) Refresh(context.Context, []PresenceEntry) error { return nil }

// presenceTTL bounds how long a crashed replica's connections still count as online.
const presenceTTL = 90 * time.Second

// NewRedisPresence shares presence across replicas. Each connection is a key that expires
// unless its replica refreshes it, so connections of a crashed replica go offline on their own.
func NewRedisPresence(rdb *redis.Client) Presence { return &redisPresence{rdb: rdb} }

type redisPresence struct{ rdb *redis.Client }

func connKey(connID string) string    { return "gotalk:presence:conn:" + connID }
func userKey(userID uuid.UUID) string { return "gotalk:presence:user:" + userID.String() }

func (r *redisPresence) Set(ctx context.Context, e PresenceEntry) error {
	return r.Refresh(ctx, []PresenceEntry{e})
}

func (r *redisPresence) Refresh(ctx context.Context, entries []PresenceEntry) error {
	if len(entries) == 0 {
		return nil
	}
	_, err := r.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, e := range entries {
			p.Set(ctx, connKey(e.ConnID), e.Status, presenceTTL)
			p.SAdd(ctx, userKey(e.UserID), e.ConnID)
			p.Expire(ctx, userKey(e.UserID), presenceTTL)
		}
		return nil
	})
	return err
}

func (r *redisPresence) Remove(ctx context.Context, userID uuid.UUID, connID string) error {
	_, err := r.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, connKey(connID))
		p.SRem(ctx, userKey(userID), connID)
		return nil
	})
	return err
}

func (r *redisPresence) Get(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}
	members := make([]*redis.StringSliceCmd, len(userIDs))
	if _, err := r.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range userIDs {
			members[i] = p.SMembers(ctx, userKey(id))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	statuses := make([][]*redis.StringCmd, len(userIDs))
	if _, err := r.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i := range userIDs {
			for _, conn := range members[i].Val() {
				statuses[i] = append(statuses[i], p.Get(ctx, connKey(conn)))
			}
		}
		return nil
	}); err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	var stale []PresenceEntry
	for i, id := range userIDs {
		var live []string
		for j, cmd := range statuses[i] {
			if s, err := cmd.Result(); err == nil {
				live = append(live, s)
			} else {
				stale = append(stale, PresenceEntry{UserID: id, ConnID: members[i].Val()[j]})
			}
		}
		out[id] = aggregate(live)
	}
	if len(stale) > 0 {
		_, _ = r.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
			for _, e := range stale {
				p.SRem(ctx, userKey(e.UserID), e.ConnID)
			}
			return nil
		})
	}
	return out, nil
}
