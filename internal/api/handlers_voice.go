package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/parkerbrown98/gotalk-server/internal/livekit"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const tagVoice = "Voice"

type JoinVoiceRequest struct {
	SelfMute bool `json:"self_mute,omitempty" doc:"Join muted"`
	SelfDeaf bool `json:"self_deaf,omitempty" doc:"Join deafened (also mutes)"`
}

type VoiceSelfRequest struct {
	SelfMute   *bool `json:"self_mute,omitempty"`
	SelfDeaf   *bool `json:"self_deaf,omitempty" doc:"Deafening also mutes unless self_mute is given"`
	SelfVideo  *bool `json:"self_video,omitempty" doc:"Camera on (SHARE_SCREEN)"`
	SelfStream *bool `json:"self_stream,omitempty" doc:"Screen share on (SHARE_SCREEN)"`
}

type VoiceTelemetryRequest struct {
	PacketLoss  float64 `json:"packet_loss" minimum:"0" maximum:"1" doc:"Fraction of packets lost since the last report"`
	JitterMs    float64 `json:"jitter_ms,omitempty" minimum:"0" maximum:"10000"`
	RttMs       float64 `json:"rtt_ms,omitempty" minimum:"0" maximum:"60000" doc:"Round-trip time to the media server"`
	BitrateKbps float64 `json:"bitrate_kbps,omitempty" minimum:"0" maximum:"100000"`
}

type MemberVoiceRequest struct {
	Mute      *bool   `json:"mute,omitempty" doc:"Server-mute in every voice channel of the place (MUTE_MEMBERS)"`
	Deaf      *bool   `json:"deaf,omitempty" doc:"Server-deafen in every voice channel of the place (MUTE_MEMBERS)"`
	ChannelID *string `json:"channel_id,omitempty" format:"uuid" doc:"Move the member to this voice channel (MOVE_MEMBERS in both channels)"`
}

func (s *Server) registerVoice() {
	huma.Register(s.api, withChatRateLimit(withAuth(operation("join-voice", http.MethodPost, "/channels/{channelID}/voice",
		"Join a voice channel (CONNECT_VOICE) and get a token for its media server", tagVoice))),
		handle(s, func(ctx context.Context, in *struct {
			ChannelPath
			Body JoinVoiceRequest
		}) (*Body[VoiceConnection], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			c, err := s.Service.JoinVoice(ctx, mustPrincipal(ctx), id, service.JoinVoiceInput{SelfMute: in.Body.SelfMute, SelfDeaf: in.Body.SelfDeaf})
			if err != nil {
				return nil, err
			}
			return ok(toVoiceConnection(c))
		}))

	huma.Register(s.api, withAuth(operation("list-voice-sessions", http.MethodGet, "/channels/{channelID}/voice/sessions",
		"A voice channel's recent sessions with call quality, newest first (MANAGE_CHANNELS)", tagVoice)),
		handle(s, func(ctx context.Context, in *struct {
			ChannelPath
			PageQuery
		}) (*Body[Page[VoiceSession]], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			page := in.pagination()
			rows, err := s.Service.ListVoiceSessions(ctx, mustPrincipal(ctx), id, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(rows, toVoiceSession, page))
		}))

	huma.Register(s.api, withAuth(operation("get-my-voice-state", http.MethodGet, "/users/@me/voice",
		"The caller's voice state; 404 when not in a voice channel", tagVoice)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[VoiceState], error) {
			v, err := s.Service.MyVoiceState(ctx, mustPrincipal(ctx).User.ID)
			if err != nil {
				return nil, err
			}
			if v == nil {
				return nil, huma.Error404NotFound("you are not in a voice channel")
			}
			return ok(toVoiceState(*v))
		}))

	huma.Register(s.api, withAuth(operation("update-my-voice-state", http.MethodPatch, "/users/@me/voice",
		"Report your own mute, deafen, camera and screen share state", tagVoice)),
		handle(s, func(ctx context.Context, in *Body[VoiceSelfRequest]) (*Body[VoiceState], error) {
			b := in.Body
			v, err := s.Service.UpdateVoiceSelf(ctx, mustPrincipal(ctx), service.VoiceSelfUpdate{
				SelfMute: b.SelfMute, SelfDeaf: b.SelfDeaf, SelfVideo: b.SelfVideo, SelfStream: b.SelfStream,
			})
			if err != nil {
				return nil, err
			}
			return ok(toVoiceState(v))
		}))

	huma.Register(s.api, withAuth(operation("leave-voice", http.MethodDelete, "/users/@me/voice",
		"Leave your voice channel", tagVoice)),
		handle(s, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
			return nil, s.Service.LeaveVoice(ctx, mustPrincipal(ctx))
		}))

	huma.Register(s.api, withChatRateLimit(withStatus(withAuth(operation("report-voice-telemetry", http.MethodPost, "/users/@me/voice/telemetry",
		"Report call quality for your current voice session, for diagnostics", tagVoice)), http.StatusNoContent)),
		handle(s, func(ctx context.Context, in *Body[VoiceTelemetryRequest]) (*struct{}, error) {
			b := in.Body
			return nil, s.Service.RecordVoiceTelemetry(ctx, mustPrincipal(ctx), service.VoiceTelemetry{
				PacketLoss: b.PacketLoss, JitterMs: b.JitterMs, RttMs: b.RttMs, BitrateKbps: b.BitrateKbps,
			})
		}))

	huma.Register(s.api, withAuth(operation("list-voice-states", http.MethodGet, "/places/{place}/voice-states",
		"Who is in the voice channels of a place that the caller can see", tagVoice)),
		handle(s, func(ctx context.Context, in *PlacePath) (*Body[[]VoiceState], error) {
			states, err := s.Service.ListVoiceStates(ctx, mustPrincipal(ctx), in.Place)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(states, toVoiceState))
		}))

	huma.Register(s.api, withAuth(operation("get-member-voice", http.MethodGet, "/places/{place}/members/{userID}/voice",
		"A member's server mute/deafen and voice state", tagVoice)),
		handle(s, func(ctx context.Context, in *MemberPath) (*Body[MemberVoice], error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			v, err := s.Service.GetMemberVoice(ctx, mustPrincipal(ctx), in.Place, id)
			if err != nil {
				return nil, err
			}
			return ok(toMemberVoice(v))
		}))

	huma.Register(s.api, withAuth(operation("update-member-voice", http.MethodPatch, "/places/{place}/members/{userID}/voice",
		"Server-mute or deafen a member (MUTE_MEMBERS), or move them to another voice channel (MOVE_MEMBERS)", tagVoice)),
		handle(s, func(ctx context.Context, in *struct {
			MemberPath
			Body MemberVoiceRequest
		}) (*Body[MemberVoice], error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			upd := service.MemberVoiceUpdate{Mute: in.Body.Mute, Deaf: in.Body.Deaf}
			if in.Body.ChannelID != nil {
				ch, err := parseID("channel_id", *in.Body.ChannelID)
				if err != nil {
					return nil, err
				}
				upd.ChannelID = &ch
			}
			v, err := s.Service.UpdateMemberVoice(ctx, mustPrincipal(ctx), in.Place, id, upd)
			if err != nil {
				return nil, err
			}
			return ok(toMemberVoice(v))
		}))

	huma.Register(s.api, withAuth(operation("disconnect-member-voice", http.MethodDelete, "/places/{place}/members/{userID}/voice",
		"Disconnect a member from their voice channel (MOVE_MEMBERS)", tagVoice)),
		handle(s, func(ctx context.Context, in *struct {
			MemberPath
			ReasonQuery
		}) (*struct{}, error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DisconnectMember(ctx, mustPrincipal(ctx), in.Place, id, in.Reason)
		}))
}

// handleVoiceWebhook receives LiveKit webhooks, which tell the instance as soon as
// participants connect or leave. Point LiveKit's webhook URL here; it must sign with the
// same API key Gotalk uses.
func (s *Server) handleVoiceWebhook(w http.ResponseWriter, r *http.Request) {
	lk := s.Service.VoiceBackend()
	if lk == nil {
		writeProblem(w, http.StatusNotFound, "voice is not enabled on this instance")
		return
	}
	ev, err := lk.ReceiveWebhook(r)
	if errors.Is(err, livekit.ErrInvalidWebhook) {
		writeProblem(w, http.StatusUnauthorized, "invalid webhook signature")
		return
	}
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid webhook")
		return
	}
	if err := s.Service.HandleVoiceWebhook(r.Context(), ev); err != nil {
		s.Logger.Error("handling voice webhook", "event", ev.Event, "error", err)
		writeProblem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusOK)
}
