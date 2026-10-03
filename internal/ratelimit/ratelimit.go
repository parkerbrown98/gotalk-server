// Package ratelimit wraps ulule/limiter with named tiers backed by Redis or memory.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ulule/limiter/v3"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	redisstore "github.com/ulule/limiter/v3/drivers/store/redis"
)

const (
	TierDefault = "default"
	TierAuth    = "auth"
)

type Result struct {
	Limit     int64
	Remaining int64
	Reset     time.Time
	Reached   bool
}

type TierInfo struct {
	Name   string `json:"name"`
	Limit  int64  `json:"limit"`
	Period string `json:"period"`
}

type Limiter struct {
	tiers   map[string]*limiter.Limiter
	info    []TierInfo
	backend string
}

// New builds a limiter. When rdb is nil an in-process memory store is used, which is
// correct for single-replica deployments only.
func New(rdb *redis.Client, rates map[string]string) (*Limiter, error) {
	var store limiter.Store
	backend := "memory"
	if rdb != nil {
		s, err := redisstore.NewStoreWithOptions(rdb, limiter.StoreOptions{Prefix: "gotalk:ratelimit"})
		if err != nil {
			return nil, fmt.Errorf("creating redis rate limit store: %w", err)
		}
		store = s
		backend = "redis"
	} else {
		store = memory.NewStoreWithOptions(limiter.StoreOptions{
			Prefix:          "gotalk:ratelimit",
			CleanUpInterval: time.Minute,
		})
	}

	l := &Limiter{tiers: map[string]*limiter.Limiter{}, backend: backend}
	for _, name := range []string{TierDefault, TierAuth} {
		formatted, ok := rates[name]
		if !ok {
			continue
		}
		rate, err := limiter.NewRateFromFormatted(formatted)
		if err != nil {
			return nil, fmt.Errorf("invalid rate %q for tier %s: %w", formatted, name, err)
		}
		l.tiers[name] = limiter.New(store, rate)
		l.info = append(l.info, TierInfo{Name: name, Limit: rate.Limit, Period: rate.Period.String()})
	}
	return l, nil
}

func (l *Limiter) Backend() string { return l.backend }

func (l *Limiter) Tiers() []TierInfo { return l.info }

// Take consumes one request from the tier for key. Unknown tiers are never limited.
func (l *Limiter) Take(ctx context.Context, tier, key string) (Result, error) {
	lim, ok := l.tiers[tier]
	if !ok {
		return Result{Limit: -1}, nil
	}
	c, err := lim.Get(ctx, tier+":"+key)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Limit:     c.Limit,
		Remaining: c.Remaining,
		Reset:     time.Unix(c.Reset, 0),
		Reached:   c.Reached,
	}, nil
}
