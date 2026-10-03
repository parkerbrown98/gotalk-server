package service

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/livekit"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const (
	kindVoice         = "voice"
	maxVoiceUserLimit = 99

	voiceSweepInterval = 30 * time.Second
	voiceCallTimeout   = 10 * time.Second
)

// Reasons a voice session ended, recorded for diagnostics.
const (
	voiceEndLeft         = "left"
	voiceEndSwitched     = "switched"
	voiceEndReplaced     = "replaced"
	voiceEndMoved        = "moved"
	voiceEndRemoved      = "removed"
	voiceEndDisconnected = "disconnected"
	voiceEndTimeout      = "timeout"
	voiceEndPermissions  = "permissions"
	voiceEndSession      = "session_ended"
	voiceEndDisabled     = "voice_disabled"
)

// VoiceEnabled reports whether a LiveKit server is configured.
func (s *Service) VoiceEnabled() bool { return s.voice != nil }

// VoiceBackend is the LiveKit client, or nil when voice is disabled.
func (s *Service) VoiceBackend() *livekit.Client { return s.voice }

func (s *Service) requireVoice() error {
	if s.voice == nil {
		return apperr.Unavailable("voice is not enabled on this instance")
	}
	return nil
}

// roomName is the LiveKit room of a voice channel.
func roomName(channelID uuid.UUID) string { return channelID.String() }

// parseUUID parses LiveKit room names and identities; rooms and participants Gotalk did
// not create do not parse.
func parseUUID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	return id, err == nil
}

// voiceGrant derives what the SFU allows from channel permissions and server mute/deafen.
// SPEAK covers the microphone; SHARE_SCREEN covers camera and screen sharing.
func voiceGrant(perms permissions.Permission, mute bool) (canSpeak, canStream bool) {
	return perms&permissions.Speak != 0 && !mute, perms&permissions.ShareScreen != 0
}

func livekitPermission(st store.VoiceState) livekit.Permission {
	p := livekit.Permission{CanSubscribe: !st.ServerDeaf}
	if st.CanSpeak {
		p.Sources = append(p.Sources, livekit.SourceMicrophone)
	}
	if st.CanStream {
		p.Sources = append(p.Sources, livekit.SourceCamera, livekit.SourceScreenShare, livekit.SourceScreenShareAudio)
	}
	return p
}

// VoiceStateView is a user's presence in a voice channel. When Left is set, the user has
// left State.ChannelID.
type VoiceStateView struct {
	State store.VoiceState
	User  *store.User
	Left  bool
}

// VoiceConnection is what a client needs to connect to a voice channel's media server.
type VoiceConnection struct {
	URL       string
	Token     string
	Room      string
	ExpiresAt time.Time
	State     VoiceStateView
}

// VoiceSessionView is one past or current stay in a voice channel.
type VoiceSessionView struct {
	Session store.VoiceSession
	User    *store.User
}

// MemberVoiceView is a member's server mute/deafen and current voice state in a place.
type MemberVoiceView struct {
	Mute  bool
	Deaf  bool
	State *VoiceStateView
}

func (s *Service) voiceViews(ctx context.Context, q *store.Queries, states []store.VoiceState, left bool) ([]VoiceStateView, error) {
	ids := make([]uuid.UUID, len(states))
	for i, st := range states {
		ids[i] = st.UserID
	}
	users, err := s.usersByID(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	out := make([]VoiceStateView, len(states))
	for i, st := range states {
		out[i] = VoiceStateView{State: st, User: ptrUser(users, &st.UserID), Left: left}
	}
	return out, nil
}

func (s *Service) voiceView(ctx context.Context, q *store.Queries, st store.VoiceState, left bool) (VoiceStateView, error) {
	views, err := s.voiceViews(ctx, q, []store.VoiceState{st}, left)
	if err != nil {
		return VoiceStateView{}, err
	}
	return views[0], nil
}

// emitVoiceState tells the user's sessions and everyone who can see the channel about a
// voice state change.
func (s *Service) emitVoiceState(ctx context.Context, q *store.Queries, st store.VoiceState, left bool) error {
	view, err := s.voiceView(ctx, q, st, left)
	if err != nil {
		return err
	}
	ch := st.ChannelID
	s.emit(ctx, q, Event{
		Type: EventVoiceStateUpdate, Data: view,
		Users: []uuid.UUID{st.UserID}, Places: []uuid.UUID{st.PlaceID}, ChannelID: &ch,
	})
	return nil
}

// kickFromRoom disconnects a participant from LiveKit after the transaction commits.
func (s *Service) kickFromRoom(ctx context.Context, q *store.Queries, channelID, userID uuid.UUID) {
	if s.voice == nil {
		return
	}
	s.afterCommit(ctx, q, func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceCallTimeout)
		defer cancel()
		if err := s.voice.RemoveParticipant(ctx, roomName(channelID), userID.String()); err != nil {
			s.log.Warn("removing voice participant", "channel", channelID, "user", userID, "error", err)
		}
	})
}

// removeVoice ends one stay in a voice channel. It does nothing if the user has since
// joined again (a newer voice session). kick also disconnects them from LiveKit.
func (s *Service) removeVoice(ctx context.Context, q *store.Queries, st store.VoiceState, reason string, kick bool) (bool, error) {
	removed, err := q.DeleteVoiceState(ctx, store.DeleteVoiceStateParams{UserID: st.UserID, VoiceSessionID: st.VoiceSessionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := q.EndVoiceSession(ctx, store.EndVoiceSessionParams{ID: removed.VoiceSessionID, EndReason: &reason}); err != nil {
		return false, err
	}
	if err := s.emitVoiceState(ctx, q, removed, true); err != nil {
		return false, err
	}
	if kick {
		s.kickFromRoom(ctx, q, removed.ChannelID, removed.UserID)
	}
	return true, nil
}

// applyVoiceGrant updates what a participant may do after their permissions, server mute
// or server deafen changed, and pushes the change to LiveKit.
func (s *Service) applyVoiceGrant(ctx context.Context, q *store.Queries, st store.VoiceState, perms permissions.Permission, mute, deaf bool) (store.VoiceState, error) {
	canSpeak, canStream := voiceGrant(perms, mute)
	if st.CanSpeak == canSpeak && st.CanStream == canStream && st.ServerMute == mute && st.ServerDeaf == deaf {
		return st, nil
	}
	updated, err := q.UpdateVoiceGrant(ctx, store.UpdateVoiceGrantParams{
		UserID: st.UserID, VoiceSessionID: st.VoiceSessionID,
		ServerMute: mute, ServerDeaf: deaf, CanSpeak: canSpeak, CanStream: canStream,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := s.emitVoiceState(ctx, q, updated, false); err != nil {
		return st, err
	}
	if s.voice != nil {
		perm := livekitPermission(updated)
		s.afterCommit(ctx, q, func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceCallTimeout)
			defer cancel()
			if err := s.voice.UpdatePermission(ctx, roomName(updated.ChannelID), updated.UserID.String(), perm); err != nil {
				s.log.Warn("updating voice permissions", "channel", updated.ChannelID, "user", updated.UserID, "error", err)
			}
		})
	}
	return updated, nil
}

// connection issues a LiveKit token for a voice state.
func (s *Service) connection(view VoiceStateView) (VoiceConnection, error) {
	if err := s.requireVoice(); err != nil {
		return VoiceConnection{}, err
	}
	name := ""
	if view.User != nil {
		name = view.User.DisplayName
		if name == "" {
			name = view.User.Username
		}
	}
	room := roomName(view.State.ChannelID)
	token, exp, err := s.voice.ParticipantToken(livekit.TokenOptions{
		Identity: view.State.UserID.String(), Name: name, Room: room,
		Permission: livekitPermission(view.State), TTL: s.cfg.Voice.TokenTTL,
	})
	if err != nil {
		return VoiceConnection{}, err
	}
	return VoiceConnection{URL: s.voice.URL(), Token: token, Room: room, ExpiresAt: exp, State: view}, nil
}

func getVoiceState(ctx context.Context, q *store.Queries, userID uuid.UUID) (store.VoiceState, bool, error) {
	st, err := q.GetVoiceState(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, false, nil
	}
	return st, err == nil, err
}

type JoinVoiceInput struct {
	SelfMute bool
	SelfDeaf bool
}

// JoinVoice puts the caller in a voice channel (CONNECT_VOICE) and returns a token for
// LiveKit. A user is in at most one voice channel at a time, so joining another channel
// (or the same one from another session) replaces their previous stay. Joining the same
// channel again from the same session only issues a fresh token.
func (s *Service) JoinVoice(ctx context.Context, p *Principal, channelID uuid.UUID, in JoinVoiceInput) (VoiceConnection, error) {
	if p.ViaToken() {
		return VoiceConnection{}, apperr.Forbidden("voice needs a login session; API tokens and bots cannot join voice channels")
	}
	if err := s.requireVoice(); err != nil {
		return VoiceConnection{}, err
	}
	var view VoiceStateView
	err := s.tx(ctx, func(q *store.Queries) error {
		if err := q.LockUserVoice(ctx, p.User.ID.String()); err != nil {
			return err
		}
		ch, err := q.GetChannelForUpdate(ctx, channelID)
		if err != nil {
			return notFound(err, "channel not found")
		}
		cc, err := s.channelCtxOf(ctx, q, p, ch)
		if err != nil {
			return err
		}
		if ch.Kind != kindVoice {
			return apperr.Invalid("only voice channels can be joined")
		}
		if err := cc.require(permissions.ConnectVoice); err != nil {
			return err
		}
		prev, hasPrev, err := getVoiceState(ctx, q, p.User.ID)
		if err != nil {
			return err
		}
		if hasPrev && prev.ChannelID == ch.ID && prev.AuthSessionID == p.SessionID {
			view, err = s.voiceView(ctx, q, prev, false)
			return err
		}
		if ch.UserLimit > 0 && !cc.has(permissions.MoveMembers) && (!hasPrev || prev.ChannelID != ch.ID) {
			n, err := q.CountOtherVoiceStates(ctx, store.CountOtherVoiceStatesParams{ChannelID: ch.ID, UserID: p.User.ID})
			if err != nil {
				return err
			}
			if n >= ch.UserLimit {
				return apperr.Conflict("this voice channel is full")
			}
		}
		if hasPrev {
			reason := voiceEndSwitched
			if prev.ChannelID == ch.ID {
				// LiveKit replaces the older connection of the same identity by itself.
				reason = voiceEndReplaced
			}
			if err := q.EndVoiceSession(ctx, store.EndVoiceSessionParams{ID: prev.VoiceSessionID, EndReason: &reason}); err != nil {
				return err
			}
			if prev.ChannelID != ch.ID {
				if err := s.emitVoiceState(ctx, q, prev, true); err != nil {
					return err
				}
				s.kickFromRoom(ctx, q, prev.ChannelID, prev.UserID)
			}
		}
		flags, err := q.GetMemberVoiceFlags(ctx, store.GetMemberVoiceFlagsParams{PlaceID: *ch.PlaceID, UserID: p.User.ID})
		if err != nil {
			return err
		}
		st, err := s.startVoiceSession(ctx, q, ch, p.User.ID, p.SessionID, cc.perms, flags.VoiceMuted, flags.VoiceDeafened, in.SelfMute, in.SelfDeaf)
		if err != nil {
			return err
		}
		view, err = s.voiceView(ctx, q, st, false)
		return err
	})
	if err != nil {
		return VoiceConnection{}, err
	}
	return s.connection(view)
}

// startVoiceSession records a new stay of userID in ch and announces it.
func (s *Service) startVoiceSession(ctx context.Context, q *store.Queries, ch store.Channel, userID, authSession uuid.UUID,
	perms permissions.Permission, mute, deaf, selfMute, selfDeaf bool) (store.VoiceState, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return store.VoiceState{}, err
	}
	if _, err := q.CreateVoiceSession(ctx, store.CreateVoiceSessionParams{ID: id, ChannelID: ch.ID, PlaceID: *ch.PlaceID, UserID: userID}); err != nil {
		return store.VoiceState{}, err
	}
	canSpeak, canStream := voiceGrant(perms, mute)
	st, err := q.UpsertVoiceState(ctx, store.UpsertVoiceStateParams{
		UserID: userID, ChannelID: ch.ID, PlaceID: *ch.PlaceID, VoiceSessionID: id, AuthSessionID: authSession,
		SelfMute: selfMute || selfDeaf, SelfDeaf: selfDeaf, ServerMute: mute, ServerDeaf: deaf,
		CanSpeak: canSpeak, CanStream: canStream,
	})
	if err != nil {
		return st, err
	}
	return st, s.emitVoiceState(ctx, q, st, false)
}

// LeaveVoice disconnects the caller from voice. Leaving when not connected is a no-op.
func (s *Service) LeaveVoice(ctx context.Context, p *Principal) error {
	return s.tx(ctx, func(q *store.Queries) error {
		if err := q.LockUserVoice(ctx, p.User.ID.String()); err != nil {
			return err
		}
		st, ok, err := getVoiceState(ctx, q, p.User.ID)
		if err != nil || !ok {
			return err
		}
		_, err = s.removeVoice(ctx, q, st, voiceEndLeft, true)
		return err
	})
}

// MyVoiceState returns the user's voice state, or nil when they are not in voice.
func (s *Service) MyVoiceState(ctx context.Context, userID uuid.UUID) (*VoiceStateView, error) {
	st, ok, err := getVoiceState(ctx, s.q, userID)
	if err != nil || !ok {
		return nil, err
	}
	view, err := s.voiceView(ctx, s.q, st, false)
	return &view, err
}

type VoiceSelfUpdate struct {
	SelfMute   *bool
	SelfDeaf   *bool
	SelfVideo  *bool
	SelfStream *bool
}

// UpdateVoiceSelf records the caller's own mute, deafen, camera and screen share state.
// Clients control their media themselves; the server relays the state to others.
// Deafening also mutes.
func (s *Service) UpdateVoiceSelf(ctx context.Context, p *Principal, in VoiceSelfUpdate) (VoiceStateView, error) {
	if in.SelfDeaf != nil && *in.SelfDeaf && in.SelfMute == nil {
		t := true
		in.SelfMute = &t
	}
	var view VoiceStateView
	err := s.tx(ctx, func(q *store.Queries) error {
		if err := q.LockUserVoice(ctx, p.User.ID.String()); err != nil {
			return err
		}
		st, ok, err := getVoiceState(ctx, q, p.User.ID)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.NotFound("you are not in a voice channel")
		}
		if ((in.SelfVideo != nil && *in.SelfVideo) || (in.SelfStream != nil && *in.SelfStream)) && !st.CanStream {
			return missing(permissions.ShareScreen)
		}
		updated, err := q.UpdateVoiceSelf(ctx, store.UpdateVoiceSelfParams{
			UserID: p.User.ID, SelfMute: in.SelfMute, SelfDeaf: in.SelfDeaf, SelfVideo: in.SelfVideo, SelfStream: in.SelfStream,
		})
		if err != nil {
			return err
		}
		if err := s.emitVoiceState(ctx, q, updated, false); err != nil {
			return err
		}
		view, err = s.voiceView(ctx, q, updated, false)
		return err
	})
	return view, err
}

// SetSpeaking relays a speaking indicator from the user's client to everyone who can see
// their voice channel. Participants of the room also get this from LiveKit directly.
func (s *Service) SetSpeaking(ctx context.Context, userID uuid.UUID, speaking bool) error {
	st, ok, err := getVoiceState(ctx, s.q, userID)
	if err != nil || !ok {
		return err
	}
	if speaking && (!st.CanSpeak || st.SelfMute) {
		return nil
	}
	ch := st.ChannelID
	s.emit(ctx, s.q, Event{
		Type: EventVoiceSpeaking, Places: []uuid.UUID{st.PlaceID}, ChannelID: &ch,
		Data: map[string]any{"user_id": userID, "channel_id": ch, "place_id": st.PlaceID, "speaking": speaking},
	})
	return nil
}

// ListVoiceStates returns who is in the voice channels of a place that the caller can see.
func (s *Service) ListVoiceStates(ctx context.Context, p *Principal, ref string) ([]VoiceStateView, error) {
	c, err := s.chat(ctx, s.q, p, ref)
	if err != nil {
		return nil, err
	}
	states, err := s.q.ListPlaceVoiceStates(ctx, c.place.ID)
	if err != nil {
		return nil, err
	}
	visible := slicesFilter(states, func(st store.VoiceState) bool { return c.canView(st.ChannelID) })
	return s.voiceViews(ctx, s.q, visible, false)
}

type VoiceTelemetry struct {
	// PacketLoss is the fraction of packets lost, 0 to 1.
	PacketLoss  float64
	JitterMs    float64
	RttMs       float64
	BitrateKbps float64
}

func (t VoiceTelemetry) validate() error {
	for _, f := range []struct {
		name     string
		v, limit float64
	}{
		{"packet_loss", t.PacketLoss, 1}, {"jitter_ms", t.JitterMs, 10_000},
		{"rtt_ms", t.RttMs, 60_000}, {"bitrate_kbps", t.BitrateKbps, 100_000},
	} {
		if math.IsNaN(f.v) || f.v < 0 || f.v > f.limit {
			return apperr.Invalid("%s must be between 0 and %g", f.name, f.limit)
		}
	}
	return nil
}

// RecordVoiceTelemetry folds a call quality report into the caller's current voice session.
func (s *Service) RecordVoiceTelemetry(ctx context.Context, p *Principal, t VoiceTelemetry) error {
	if err := t.validate(); err != nil {
		return err
	}
	st, ok, err := getVoiceState(ctx, s.q, p.User.ID)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.NotFound("you are not in a voice channel")
	}
	return s.q.RecordVoiceTelemetry(ctx, store.RecordVoiceTelemetryParams{
		ID: st.VoiceSessionID, PacketLoss: t.PacketLoss, JitterMs: t.JitterMs, RttMs: t.RttMs, BitrateKbps: t.BitrateKbps,
	})
}

// ListVoiceSessions returns a voice channel's recent sessions with their call quality,
// newest first (MANAGE_CHANNELS).
func (s *Service) ListVoiceSessions(ctx context.Context, p *Principal, channelID uuid.UUID, page Pagination) ([]VoiceSessionView, error) {
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return nil, err
	}
	if cc.ch.Kind != kindVoice {
		return nil, apperr.Invalid("only voice channels have voice sessions")
	}
	if !cc.has(permissions.ManageChannels) {
		return nil, missing(permissions.ManageChannels)
	}
	page = page.normalized()
	rows, err := s.q.ListChannelVoiceSessions(ctx, store.ListChannelVoiceSessionsParams{ChannelID: cc.ch.ID, Lim: page.Limit, Off: page.Offset})
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.UserID
	}
	users, err := s.usersByID(ctx, s.q, ids)
	if err != nil {
		return nil, err
	}
	out := make([]VoiceSessionView, len(rows))
	for i, r := range rows {
		out[i] = VoiceSessionView{Session: r, User: ptrUser(users, &r.UserID)}
	}
	return out, nil
}

type MemberVoiceUpdate struct {
	Mute *bool
	Deaf *bool
	// ChannelID moves the member to another voice channel of the place.
	ChannelID *uuid.UUID
}

// voiceTarget loads a member for voice moderation. Members may always act on themselves;
// otherwise they must outrank the target.
func (s *Service) voiceTarget(ctx context.Context, q *store.Queries, p *Principal, ref string, userID uuid.UUID) (*chatScope, permissions.Member, error) {
	c, err := s.chat(ctx, q, p, ref)
	if err != nil {
		return nil, permissions.Member{}, err
	}
	target, err := s.targetStanding(ctx, q, c.place, userID)
	if err != nil {
		return nil, target, err
	}
	if userID != p.User.ID && !c.acc.Member.Outranks(target) {
		return nil, target, apperr.Forbidden("you can only manage the voice of members ranked below you")
	}
	if err := q.LockUserVoice(ctx, userID.String()); err != nil {
		return nil, target, err
	}
	return c, target, nil
}

// UpdateMemberVoice server-mutes or deafens a member (MUTE_MEMBERS), or moves them to
// another voice channel (MOVE_MEMBERS in both channels). Mute and deafen persist across
// the place's voice channels until lifted. A moved member's sessions receive a
// VOICE_SERVER_UPDATE event with a token for the new channel.
func (s *Service) UpdateMemberVoice(ctx context.Context, p *Principal, ref string, userID uuid.UUID, in MemberVoiceUpdate) (MemberVoiceView, error) {
	if in.Mute == nil && in.Deaf == nil && in.ChannelID == nil {
		return MemberVoiceView{}, apperr.Invalid("set at least one of mute, deaf or channel_id")
	}
	if in.ChannelID != nil {
		if err := s.requireVoice(); err != nil {
			return MemberVoiceView{}, err
		}
	}
	var out MemberVoiceView
	err := s.tx(ctx, func(q *store.Queries) error {
		c, target, err := s.voiceTarget(ctx, q, p, ref, userID)
		if err != nil {
			return err
		}
		st, inVoice, err := getVoiceState(ctx, q, userID)
		if err != nil {
			return err
		}
		inVoice = inVoice && st.PlaceID == c.place.ID
		flags, err := q.GetMemberVoiceFlags(ctx, store.GetMemberVoiceFlagsParams{PlaceID: c.place.ID, UserID: userID})
		if err != nil {
			return err
		}

		if in.Mute != nil || in.Deaf != nil {
			perms := c.acc.Member.Effective()
			if inVoice {
				perms = c.perms(st.ChannelID)
			}
			if perms&permissions.MuteMembers == 0 {
				return missing(permissions.MuteMembers)
			}
			updated, err := q.SetMemberVoiceFlags(ctx, store.SetMemberVoiceFlagsParams{
				PlaceID: c.place.ID, UserID: userID, Muted: in.Mute, Deafened: in.Deaf,
			})
			if err != nil {
				return err
			}
			for _, change := range []struct {
				before, after bool
				on, off       string
			}{
				{flags.VoiceMuted, updated.VoiceMuted, "member.voice_mute", "member.voice_unmute"},
				{flags.VoiceDeafened, updated.VoiceDeafened, "member.voice_deafen", "member.voice_undeafen"},
			} {
				if change.before == change.after {
					continue
				}
				action := map[bool]string{true: change.on, false: change.off}[change.after]
				if err := s.audit(ctx, q, c.place.ID, p, action, "user", &userID, "", nil); err != nil {
					return err
				}
			}
			flags = store.GetMemberVoiceFlagsRow(updated)
		}

		moved := false
		if in.ChannelID != nil {
			if !inVoice {
				return apperr.Invalid("the member is not in a voice channel of this place")
			}
			dest, ok := c.channels[*in.ChannelID]
			if !ok || dest.Kind != kindVoice || !c.canView(dest.ID) {
				return apperr.Invalid("voice channel not found")
			}
			if !c.has(st.ChannelID, permissions.MoveMembers) || !c.has(dest.ID, permissions.MoveMembers) {
				return missing(permissions.MoveMembers)
			}
			destPerms := c.permsFor(target, dest.ID)
			if !c.visibleTo(target, dest.ID) || destPerms&permissions.ConnectVoice == 0 {
				return apperr.Forbidden("the member cannot connect to that channel")
			}
			if dest.ID != st.ChannelID {
				reason := voiceEndMoved
				if err := q.EndVoiceSession(ctx, store.EndVoiceSessionParams{ID: st.VoiceSessionID, EndReason: &reason}); err != nil {
					return err
				}
				if err := s.emitVoiceState(ctx, q, st, true); err != nil {
					return err
				}
				s.kickFromRoom(ctx, q, st.ChannelID, userID)
				from := st.ChannelID
				st, err = s.startVoiceSession(ctx, q, dest, userID, st.AuthSessionID, destPerms,
					flags.VoiceMuted, flags.VoiceDeafened, st.SelfMute, st.SelfDeaf)
				if err != nil {
					return err
				}
				view, err := s.voiceView(ctx, q, st, false)
				if err != nil {
					return err
				}
				conn, err := s.connection(view)
				if err != nil {
					return err
				}
				s.emit(ctx, q, Event{Type: EventVoiceServerUpdate, Data: conn, Users: []uuid.UUID{userID}})
				if err := s.audit(ctx, q, c.place.ID, p, "member.voice_move", "user", &userID, "",
					map[string]any{"from_channel_id": from, "to_channel_id": dest.ID}); err != nil {
					return err
				}
				moved = true
			}
		}

		if inVoice && !moved {
			if st, err = s.applyVoiceGrant(ctx, q, st, c.permsFor(target, st.ChannelID), flags.VoiceMuted, flags.VoiceDeafened); err != nil {
				return err
			}
		}
		out = MemberVoiceView{Mute: flags.VoiceMuted, Deaf: flags.VoiceDeafened}
		if inVoice {
			view, err := s.voiceView(ctx, q, st, false)
			if err != nil {
				return err
			}
			out.State = &view
		}
		return nil
	})
	return out, err
}

// GetMemberVoice returns a member's server mute/deafen and voice state in a place.
func (s *Service) GetMemberVoice(ctx context.Context, p *Principal, ref string, userID uuid.UUID) (MemberVoiceView, error) {
	c, err := s.chat(ctx, s.q, p, ref)
	if err != nil {
		return MemberVoiceView{}, err
	}
	flags, err := s.q.GetMemberVoiceFlags(ctx, store.GetMemberVoiceFlagsParams{PlaceID: c.place.ID, UserID: userID})
	if err != nil {
		return MemberVoiceView{}, notFound(err, "member not found")
	}
	out := MemberVoiceView{Mute: flags.VoiceMuted, Deaf: flags.VoiceDeafened}
	st, ok, err := getVoiceState(ctx, s.q, userID)
	if err != nil {
		return out, err
	}
	if ok && st.PlaceID == c.place.ID && c.canView(st.ChannelID) {
		view, err := s.voiceView(ctx, s.q, st, false)
		if err != nil {
			return out, err
		}
		out.State = &view
	}
	return out, nil
}

// DisconnectMember removes a member from their voice channel (MOVE_MEMBERS in it).
func (s *Service) DisconnectMember(ctx context.Context, p *Principal, ref string, userID uuid.UUID, reason string) error {
	if len([]rune(reason)) > maxReasonLen {
		return apperr.Invalid("reason must be at most %d characters", maxReasonLen)
	}
	return s.tx(ctx, func(q *store.Queries) error {
		c, _, err := s.voiceTarget(ctx, q, p, ref, userID)
		if err != nil {
			return err
		}
		st, ok, err := getVoiceState(ctx, q, userID)
		if err != nil {
			return err
		}
		if !ok || st.PlaceID != c.place.ID || !c.canView(st.ChannelID) {
			return apperr.NotFound("the member is not in a voice channel of this place")
		}
		if !c.has(st.ChannelID, permissions.MoveMembers) {
			return missing(permissions.MoveMembers)
		}
		if _, err := s.removeVoice(ctx, q, st, voiceEndRemoved, true); err != nil {
			return err
		}
		return s.audit(ctx, q, c.place.ID, p, "member.voice_disconnect", "user", &userID, reason,
			map[string]any{"channel_id": st.ChannelID})
	})
}

// syncVoice re-checks voice participants of a place (only users, when given) after
// permissions, roles, channels or membership changed. Participants who can no longer
// connect are disconnected; the others get their SFU permissions updated.
func (s *Service) syncVoice(ctx context.Context, placeID uuid.UUID, users []uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceCallTimeout)
	defer cancel()
	err := s.tx(ctx, func(q *store.Queries) error {
		states, err := q.ListPlaceVoiceStates(ctx, placeID)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			states = slicesFilter(states, func(st store.VoiceState) bool { return slices.Contains(users, st.UserID) })
		}
		if len(states) == 0 {
			return nil
		}
		place, err := q.GetPlaceByID(ctx, placeID)
		if errors.Is(err, pgx.ErrNoRows) {
			for _, st := range states {
				if _, err := s.removeVoice(ctx, q, st, voiceEndPermissions, true); err != nil {
					return err
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		c, err := s.loadChannels(ctx, q, place)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, len(states))
		for i, st := range states {
			ids[i] = st.UserID
		}
		standings, err := s.standings(ctx, q, place, c.defaultRole, ids)
		if err != nil {
			return err
		}
		for _, st := range states {
			m, member := standings[st.UserID]
			perms := c.permsFor(m, st.ChannelID)
			if !member || !c.visibleTo(m, st.ChannelID) || perms&permissions.ConnectVoice == 0 {
				if _, err := s.removeVoice(ctx, q, st, voiceEndPermissions, true); err != nil {
					return err
				}
				continue
			}
			if _, err := s.applyVoiceGrant(ctx, q, st, perms, st.ServerMute, st.ServerDeaf); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.Error("re-checking voice participants", "place", placeID, "error", err)
	}
}

// endVoiceForSessions disconnects the user from voice if they joined from one of the
// ended sessions (every session except keep, when sessions is empty).
func (s *Service) endVoiceForSessions(ctx context.Context, userID uuid.UUID, sessions []uuid.UUID, keep *uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceCallTimeout)
	defer cancel()
	err := s.tx(ctx, func(q *store.Queries) error {
		if err := q.LockUserVoice(ctx, userID.String()); err != nil {
			return err
		}
		st, ok, err := getVoiceState(ctx, q, userID)
		if err != nil || !ok {
			return err
		}
		ended := slices.Contains(sessions, st.AuthSessionID) ||
			(len(sessions) == 0 && (keep == nil || *keep != st.AuthSessionID))
		if !ended {
			return nil
		}
		_, err = s.removeVoice(ctx, q, st, voiceEndSession, true)
		return err
	})
	if err != nil {
		s.log.Error("ending voice for ended sessions", "user", userID, "error", err)
	}
}

// voiceChannelDeleted announces that the participants of a deleted voice channel left,
// and closes its LiveKit room. Their states and sessions are removed with the channel.
func (s *Service) voiceChannelDeleted(ctx context.Context, q *store.Queries, ch store.Channel) error {
	states, err := q.ListChannelVoiceStates(ctx, ch.ID)
	if err != nil {
		return err
	}
	views, err := s.voiceViews(ctx, q, states, true)
	if err != nil {
		return err
	}
	for _, v := range views {
		s.emit(ctx, q, Event{Type: EventVoiceStateUpdate, Data: v, Users: []uuid.UUID{v.State.UserID}})
	}
	if s.voice != nil {
		s.afterCommit(ctx, q, func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceCallTimeout)
			defer cancel()
			if err := s.voice.DeleteRoom(ctx, roomName(ch.ID)); err != nil {
				s.log.Warn("closing voice room", "channel", ch.ID, "error", err)
			}
		})
	}
	return nil
}

// HandleVoiceWebhook applies a verified LiveKit webhook: participants connecting and
// leaving, and rooms closing. Events about unknown rooms or users are ignored.
func (s *Service) HandleVoiceWebhook(ctx context.Context, ev livekit.WebhookEvent) error {
	if ev.Room == nil {
		return nil
	}
	channelID, ok := parseUUID(ev.Room.Name)
	if !ok {
		return nil
	}
	switch ev.Event {
	case "participant_joined", "participant_left":
		if ev.Participant == nil {
			return nil
		}
		userID, ok := parseUUID(ev.Participant.Identity)
		if !ok {
			return nil
		}
		if ev.Event == "participant_joined" {
			return s.markVoiceConnected(ctx, userID, channelID, ev.Participant.SID, nil)
		}
		return s.tx(ctx, func(q *store.Queries) error {
			if err := q.LockUserVoice(ctx, userID.String()); err != nil {
				return err
			}
			st, ok, err := getVoiceState(ctx, q, userID)
			if err != nil || !ok {
				return err
			}
			// Only the connection we know about counts: a replaced connection leaving must
			// not end the newer one.
			if st.ChannelID != channelID || st.ParticipantSid == nil || *st.ParticipantSid != ev.Participant.SID {
				return nil
			}
			_, err = s.removeVoice(ctx, q, st, voiceEndDisconnected, false)
			return err
		})
	case "room_finished":
		return s.tx(ctx, func(q *store.Queries) error {
			states, err := q.ListChannelVoiceStates(ctx, channelID)
			if err != nil {
				return err
			}
			for _, st := range states {
				if st.ConnectedAt == nil {
					continue
				}
				if _, err := s.removeVoice(ctx, q, st, voiceEndDisconnected, false); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return nil
}

// markVoiceConnected records that LiveKit sees the user in the channel's room.
func (s *Service) markVoiceConnected(ctx context.Context, userID, channelID uuid.UUID, sid string, session *uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		if err := q.LockUserVoice(ctx, userID.String()); err != nil {
			return err
		}
		prev, ok, err := getVoiceState(ctx, q, userID)
		if err != nil || !ok || prev.ChannelID != channelID {
			return err
		}
		if session != nil && prev.VoiceSessionID != *session {
			return nil
		}
		if prev.ConnectedAt != nil && prev.ParticipantSid != nil && *prev.ParticipantSid == sid {
			return nil
		}
		st, err := q.MarkVoiceConnected(ctx, store.MarkVoiceConnectedParams{
			ParticipantSid: &sid, UserID: userID, ChannelID: channelID, VoiceSessionID: &prev.VoiceSessionID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := q.ConnectVoiceSession(ctx, st.VoiceSessionID); err != nil {
			return err
		}
		if prev.ConnectedAt == nil {
			return s.emitVoiceState(ctx, q, st, false)
		}
		return nil
	})
}

// RunVoiceMaintenance reconciles voice states with LiveKit until ctx ends.
func (s *Service) RunVoiceMaintenance(ctx context.Context) {
	t := time.NewTicker(voiceSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := s.SweepVoice(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("reconciling voice states", "error", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// SweepVoice reconciles voice states with what LiveKit reports, so voice stays correct
// even without webhooks or when some were missed: participants who never connected within
// voice.join_timeout, or who are no longer in their room, are removed. It also closes
// orphaned sessions and prunes old ones. One replica sweeps at a time.
func (s *Service) SweepVoice(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	locked, err := s.q.WithTx(tx).TryVoiceSweepLock(ctx)
	if err != nil || !locked {
		return err
	}

	states, err := s.q.ListAllVoiceStates(ctx)
	if err != nil {
		return err
	}
	rooms := map[uuid.UUID][]store.VoiceState{}
	var order []uuid.UUID
	for _, st := range states {
		if _, ok := rooms[st.ChannelID]; !ok {
			order = append(order, st.ChannelID)
		}
		rooms[st.ChannelID] = append(rooms[st.ChannelID], st)
	}
	joinDeadline := time.Now().Add(-s.cfg.Voice.JoinTimeout)
	places := map[uuid.UUID]bool{}
	for _, channelID := range order {
		places[rooms[channelID][0].PlaceID] = true
		if err := s.sweepRoom(ctx, channelID, rooms[channelID], joinDeadline); err != nil {
			s.log.Warn("reconciling voice room", "channel", channelID, "error", err)
		}
	}
	// Permissions can also change without an event, e.g. when a timeout runs out.
	for placeID := range places {
		s.syncVoice(ctx, placeID, nil)
	}

	if err := s.q.EndOrphanedVoiceSessions(ctx, time.Now().Add(-time.Minute)); err != nil {
		return err
	}
	before := time.Now().Add(-s.cfg.Voice.SessionRetention)
	return s.q.PruneVoiceSessions(ctx, &before)
}

func (s *Service) sweepRoom(ctx context.Context, channelID uuid.UUID, states []store.VoiceState, joinDeadline time.Time) error {
	remove := func(st store.VoiceState, reason string) error {
		return s.tx(ctx, func(q *store.Queries) error {
			_, err := s.removeVoice(ctx, q, st, reason, false)
			return err
		})
	}
	if s.voice == nil {
		for _, st := range states {
			if err := remove(st, voiceEndDisabled); err != nil {
				return err
			}
		}
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, voiceCallTimeout)
	participants, err := s.voice.ListParticipants(callCtx, roomName(channelID))
	cancel()
	if err != nil {
		return err
	}
	present := make(map[string]string, len(participants))
	for _, p := range participants {
		present[p.Identity] = p.SID
	}
	for _, st := range states {
		sid, ok := present[st.UserID.String()]
		switch {
		case ok:
			if err := s.markVoiceConnected(ctx, st.UserID, channelID, sid, &st.VoiceSessionID); err != nil {
				return err
			}
		case st.ConnectedAt != nil:
			if err := remove(st, voiceEndDisconnected); err != nil {
				return err
			}
		case st.JoinedAt.Before(joinDeadline):
			if err := remove(st, voiceEndTimeout); err != nil {
				return err
			}
		}
	}
	return nil
}
