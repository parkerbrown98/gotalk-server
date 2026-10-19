package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/indexsig"
)

// Ping timing. A ping goes out IndexPingSettle after the first change, then at most once
// per IndexPingInterval across every replica (the claim is recorded in the database).
// Tests shorten them.
var (
	IndexPingInterval = 30 * time.Second
	IndexPingSettle   = 2 * time.Second
	indexPingRetries  = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}
	indexPingTimeout  = 10 * time.Second
)

// IndexPingPath is the index's ping endpoint, relative to directory.platform_url.
const IndexPingPath = "/api/index/v1/ping"

// wakeIndexPings tells the ping sender that the change feed has new events.
func (s *Service) wakeIndexPings() {
	select {
	case s.indexWake <- struct{}{}:
	default:
	}
}

// RunIndexPings sends a signed ping to the index after change feed events, so it reads the
// feed early. Pings are hints: failures are logged and retried a few times, and the index
// reads the feed on its own schedule anyway.
func (s *Service) RunIndexPings(ctx context.Context) {
	var (
		timer   *time.Timer
		fire    <-chan time.Time
		attempt int
	)
	schedule := func(d time.Duration) {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(d)
		fire = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.indexWake:
			if fire == nil {
				attempt = 0
				schedule(IndexPingSettle)
			}
		case <-fire:
			fire = nil
			retry, err := s.pingIndex(ctx)
			switch {
			case err == nil && retry > 0:
				schedule(retry)
			case err != nil && ctx.Err() == nil:
				if attempt < len(indexPingRetries) {
					s.log.Warn("pinging the public index; will retry", "error", err, "attempt", attempt+1)
					schedule(max(retry, indexPingRetries[attempt]))
					attempt++
				} else {
					s.log.Warn("pinging the public index failed; it will pick up changes on its next scheduled read", "error", err)
				}
			}
		}
	}
}

// pingIndex sends one ping when listing and pings are on. A positive duration without an
// error means another ping was sent too recently and this one should wait that long.
func (s *Service) pingIndex(ctx context.Context) (time.Duration, error) {
	dir := s.Providers().Directory
	if !dir.Listed || dir.PingsDisabled {
		return 0, nil
	}
	origin := s.cfg.Server.PublicURL
	if origin == "" {
		s.log.Debug("not pinging the public index: server.public_url is not set")
		return 0, nil
	}
	keys, err := s.directoryKeys(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := s.q.ClaimIndexPing(ctx, IndexPingInterval.Seconds()); errors.Is(err, pgx.ErrNoRows) {
		last, err := s.q.GetLastIndexPing(ctx)
		if err != nil || last == nil {
			return IndexPingInterval, err
		}
		return max(time.Until(last.Add(IndexPingInterval)), 100*time.Millisecond), nil
	} else if err != nil {
		return 0, err
	}

	body, err := json.Marshal(map[string]string{"origin": origin})
	if err != nil {
		return 0, err
	}
	cctx, cancel := context.WithTimeout(ctx, indexPingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, dir.platformURL()+IndexPingPath, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gotalk-server")
	indexsig.SignHTTP(req, body, keys.signing, time.Now())
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	detail, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		s.log.Debug("pinged the public index", "status", res.StatusCode)
		return 0, nil
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		wait := time.Duration(0)
		if secs, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
		return wait, fmt.Errorf("index answered %d: %s", res.StatusCode, bytes.TrimSpace(detail))
	default:
		// 401 (key not published yet), 404 (instance not registered) and the like.
		s.log.Warn("the public index did not accept the ping", "status", res.StatusCode, "detail", string(bytes.TrimSpace(detail)))
		return 0, nil
	}
}
