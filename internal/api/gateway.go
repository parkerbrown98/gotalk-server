package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/parkerbrown98/gotalk-server/internal/ratelimit"
	"github.com/parkerbrown98/gotalk-server/internal/realtime"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

// Gateway protocol close codes (see README).
const (
	closeUnknownError     = 4000
	closeDecodeError      = 4001
	closeNotAuthenticated = 4002
	closeAlreadyIdentify  = 4003
	closeAuthFailed       = 4004
	closeRateLimited      = 4008
	closeHeartbeatTimeout = 4009
)

const (
	defaultHeartbeat  = 30 * time.Second
	identifyTimeout   = 10 * time.Second
	writeTimeout      = 10 * time.Second
	maxClientFrame    = 4096
	clientQueue       = 256
	maxOpsPerMinute   = 120
	gatewayPathSuffix = "/gateway"
	voiceWebhookPath  = "/voice/webhook"
)

type gatewayFrame struct {
	Op   string          `json:"op"`
	Data json.RawMessage `json:"d,omitempty"`
}

type identifyData struct {
	Token  string `json:"token"`
	Status string `json:"status"`
}

type presenceData struct {
	Status string `json:"status"`
}

type speakingData struct {
	Speaking *bool `json:"speaking"`
}

func encodeFrame(op string, data any) []byte {
	f := map[string]any{"op": op}
	if data != nil {
		f["d"] = data
	}
	raw, _ := json.Marshal(f)
	return raw
}

func (s *Server) heartbeat() time.Duration {
	if s.HeartbeatInterval > 0 {
		return s.HeartbeatInterval
	}
	return defaultHeartbeat
}

// gatewayURL is the WebSocket URL clients connect to.
func gatewayURL(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + APIPrefix + gatewayPathSuffix
}

// originAllowed applies the CORS origin list to browser WebSocket handshakes. Same-origin
// and non-browser clients (no Origin header) are always allowed.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if u, err := url.Parse(origin); err == nil && strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, allowed := range s.Service.Providers().CORS.AllowedOrigins {
		if allowed == "*" || strings.EqualFold(allowed, origin) {
			return true
		}
		if prefix, suffix, ok := strings.Cut(allowed, "*"); ok &&
			len(origin) >= len(prefix)+len(suffix) &&
			strings.HasPrefix(strings.ToLower(origin), strings.ToLower(prefix)) &&
			strings.HasSuffix(strings.ToLower(origin), strings.ToLower(suffix)) {
			return true
		}
	}
	return false
}

// handleGateway upgrades to a WebSocket and streams real-time events to the client.
func (s *Server) handleGateway(w http.ResponseWriter, r *http.Request) {
	ip := clientFrom(r.Context()).IP
	if s.Config.RateLimit.Enabled {
		res, err := s.Limiter.Take(r.Context(), ratelimit.TierAuth, "gateway:"+ip)
		if err == nil && res.Reached {
			retry := max(int64(time.Until(res.Reset).Seconds()), 1)
			w.Header().Set("Retry-After", strconv.FormatInt(retry, 10))
			writeProblem(w, http.StatusTooManyRequests, "too many gateway connections; retry later")
			return
		}
	}
	if !s.originAllowed(r) {
		writeProblem(w, http.StatusForbidden, "origin not allowed")
		return
	}
	// The server's read/write timeouts would otherwise cut long-lived connections.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	// Origins were checked above against the operator's CORS list.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxClientFrame)
	ctx := r.Context()

	interval := s.heartbeat()
	if err := s.writeFrame(ctx, conn, encodeFrame("hello", map[string]any{"heartbeat_interval": interval.Milliseconds()})); err != nil {
		return
	}

	p, status, code, reason := s.identify(ctx, conn)
	if p == nil {
		if code != 0 {
			_ = conn.Close(websocket.StatusCode(code), reason)
		}
		return
	}
	places, err := s.Service.UserPlaceIDs(ctx, p.User.ID)
	if err != nil {
		s.Logger.Error("loading places for gateway", "error", err)
		_ = conn.Close(closeUnknownError, "internal error")
		return
	}
	voice, err := s.Service.MyVoiceState(ctx, p.User.ID)
	if err != nil {
		s.Logger.Error("loading voice state for gateway", "error", err)
		_ = conn.Close(closeUnknownError, "internal error")
		return
	}
	var voiceState *VoiceState
	if voice != nil {
		v := toVoiceState(*voice)
		voiceState = &v
	}

	client := realtime.NewClient(p.User.ID, p.SessionID, status, clientQueue)
	placeIDs := make([]string, len(places))
	for i, id := range places {
		placeIDs[i] = id.String()
	}
	// READY is queued before registering so it precedes every other event.
	client.Send(realtime.EncodeDispatch("READY", mustJSON(map[string]any{
		"user": toSelfUser(p.User), "session_id": p.SessionID.String(), "place_ids": placeIDs,
		"status": status, "heartbeat_interval": interval.Milliseconds(), "voice_state": voiceState,
	})))
	s.hub.Register(ctx, client, places)

	var lastBeat atomic.Int64
	lastBeat.Store(time.Now().UnixNano())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.gatewayWriter(ctx, conn, client, &lastBeat, interval)
	}()

	s.gatewayReader(ctx, conn, client, &lastBeat)

	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.hub.Unregister(cleanup, client)
	wg.Wait()
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func (s *Server) writeFrame(ctx context.Context, conn *websocket.Conn, frame []byte) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, frame)
}

// identify waits for the client's identify op and authenticates it. On failure it returns
// a nil principal and, when the client should be told why, a close code.
func (s *Server) identify(ctx context.Context, conn *websocket.Conn) (p *service.Principal, status string, code int, reason string) {
	idCtx, cancel := context.WithTimeout(ctx, identifyTimeout)
	defer cancel()
	_, raw, err := conn.Read(idCtx)
	if err != nil {
		return nil, "", 0, ""
	}
	var f gatewayFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, "", closeDecodeError, "invalid frame"
	}
	if f.Op != "identify" {
		return nil, "", closeNotAuthenticated, "send identify first"
	}
	var d identifyData
	if err := json.Unmarshal(f.Data, &d); err != nil || d.Token == "" {
		return nil, "", closeDecodeError, "identify needs a token"
	}
	if d.Status == "" {
		d.Status = realtime.StatusOnline
	}
	if !realtime.ValidStatus(d.Status) {
		return nil, "", closeDecodeError, "invalid status"
	}
	p, err = s.Service.Authenticate(ctx, d.Token)
	if err != nil {
		return nil, "", closeAuthFailed, "authentication failed"
	}
	if !p.HasScope(service.ScopeGateway) {
		return nil, "", closeAuthFailed, "this token lacks the gateway scope"
	}
	return p, d.Status, 0, ""
}

func (s *Server) gatewayWriter(ctx context.Context, conn *websocket.Conn, client *realtime.Client, lastBeat *atomic.Int64, interval time.Duration) {
	check := time.NewTicker(interval / 2)
	defer check.Stop()
	for {
		select {
		case frame := <-client.Out():
			if err := s.writeFrame(ctx, conn, frame); err != nil {
				client.Close(closeUnknownError, "write failed")
				_ = conn.CloseNow()
				return
			}
		case <-client.Done():
			code, reason := client.CloseInfo()
			_ = conn.Close(websocket.StatusCode(code), reason)
			return
		case <-check.C:
			if time.Since(time.Unix(0, lastBeat.Load())) > interval+interval/2 {
				client.Close(closeHeartbeatTimeout, "heartbeat timed out")
			}
		case <-ctx.Done():
			_ = conn.CloseNow()
			return
		}
	}
}

func (s *Server) gatewayReader(ctx context.Context, conn *websocket.Conn, client *realtime.Client, lastBeat *atomic.Int64) {
	windowStart, ops := time.Now(), 0
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if time.Since(windowStart) > time.Minute {
			windowStart, ops = time.Now(), 0
		}
		if ops++; ops > maxOpsPerMinute {
			client.Close(closeRateLimited, "too many messages")
			return
		}
		var f gatewayFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			client.Close(closeDecodeError, "invalid frame")
			return
		}
		switch f.Op {
		case "heartbeat":
			lastBeat.Store(time.Now().UnixNano())
			client.Send(encodeFrame("heartbeat_ack", nil))
		case "presence_update":
			var d presenceData
			if err := json.Unmarshal(f.Data, &d); err != nil || !realtime.ValidStatus(d.Status) {
				client.Close(closeDecodeError, "invalid status")
				return
			}
			s.hub.SetStatus(ctx, client, d.Status)
		case "voice_speaking":
			var d speakingData
			if err := json.Unmarshal(f.Data, &d); err != nil || d.Speaking == nil {
				client.Close(closeDecodeError, "voice_speaking needs speaking")
				return
			}
			if err := s.Service.SetSpeaking(ctx, client.UserID, *d.Speaking); err != nil {
				s.Logger.Warn("relaying speaking state", "error", err)
			}
		case "identify":
			client.Close(closeAlreadyIdentify, "already identified")
			return
		default:
			client.Close(closeDecodeError, "unknown op")
			return
		}
	}
}

// gatewayPublisher turns service events into envelopes for the hub.
type gatewayPublisher struct {
	hub *realtime.Hub
	log *slog.Logger
}

func (g gatewayPublisher) Publish(ctx context.Context, ev service.Event) {
	env := realtime.Envelope{Type: ev.Type, Users: ev.Users, Places: ev.Places, ChannelID: ev.ChannelID, Control: ev.Control}
	if ev.Type != "" {
		data, err := encodeEventData(ev.Data)
		if err != nil {
			g.log.Error("encoding gateway event", "type", ev.Type, "error", err)
			return
		}
		env.Data = data
	}
	if err := g.hub.Publish(ctx, env); err != nil {
		g.log.Warn("publishing gateway event", "type", ev.Type, "error", err)
	}
}

// encodeEventData converts service views to the same JSON shapes the REST API returns, for
// gateway events and webhook payloads. Unknown types are rejected so internal fields can
// never leak to clients.
func encodeEventData(v any) (json.RawMessage, error) {
	switch d := v.(type) {
	case service.ChannelView:
		return json.Marshal(toChannel(d))
	case service.MessageView:
		return json.Marshal(toMessage(d))
	case service.NotificationView:
		return json.Marshal(toNotification(d))
	case service.RecipientEvent:
		return json.Marshal(map[string]any{"channel_id": d.ChannelID, "user": toUser(d.User)})
	case service.VoiceStateView:
		return json.Marshal(toVoiceState(d))
	case service.VoiceConnection:
		return json.Marshal(toVoiceConnection(d))
	case service.InteractionView:
		return json.Marshal(toInteraction(d))
	case service.MemberEvent:
		return json.Marshal(map[string]any{"place_id": d.PlaceID, "user": toUser(d.User)})
	case service.TopicEvent:
		return json.Marshal(map[string]any{"topic": toTopic(d.Topic), "post": toPost(d.Post)})
	case service.AuditView:
		return json.Marshal(toAuditEntry(d))
	case service.ReportView:
		return json.Marshal(toReport(d))
	case map[string]any:
		return json.Marshal(d)
	case nil:
		return nil, errors.New("event has no data")
	}
	return nil, fmt.Errorf("unsupported event data %T", v)
}
