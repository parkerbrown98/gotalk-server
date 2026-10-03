package api

import (
	"time"

	"github.com/parkerbrown98/gotalk-server/internal/service"
)

type VoiceState struct {
	UserID     string    `json:"user_id" format:"uuid"`
	User       *User     `json:"user"`
	PlaceID    string    `json:"place_id" format:"uuid"`
	ChannelID  *string   `json:"channel_id" format:"uuid" doc:"null when the user left voice (in VOICE_STATE_UPDATE events)"`
	SessionID  string    `json:"session_id" format:"uuid" doc:"The voice session (one stay in a channel); see the channel's voice sessions"`
	SelfMute   bool      `json:"self_mute"`
	SelfDeaf   bool      `json:"self_deaf"`
	SelfVideo  bool      `json:"self_video" doc:"Camera on"`
	SelfStream bool      `json:"self_stream" doc:"Screen share on"`
	Mute       bool      `json:"mute" doc:"Server-muted by a moderator"`
	Deaf       bool      `json:"deaf" doc:"Server-deafened by a moderator"`
	CanSpeak   bool      `json:"can_speak" doc:"Allowed to publish a microphone (SPEAK, not server-muted, not timed out)"`
	CanStream  bool      `json:"can_stream" doc:"Allowed to publish camera and screen share (SHARE_SCREEN, not timed out)"`
	Connected  bool      `json:"connected" doc:"Whether the media server reports the user connected"`
	JoinedAt   time.Time `json:"joined_at"`
}

func toVoiceState(v service.VoiceStateView) VoiceState {
	st := v.State
	out := VoiceState{
		UserID: st.UserID.String(), User: userPtr(v.User), PlaceID: st.PlaceID.String(), SessionID: st.VoiceSessionID.String(),
		JoinedAt: st.JoinedAt,
	}
	if v.Left {
		return out
	}
	ch := st.ChannelID.String()
	out.ChannelID = &ch
	out.SelfMute, out.SelfDeaf, out.SelfVideo, out.SelfStream = st.SelfMute, st.SelfDeaf, st.SelfVideo, st.SelfStream
	out.Mute, out.Deaf, out.CanSpeak, out.CanStream = st.ServerMute, st.ServerDeaf, st.CanSpeak, st.CanStream
	out.Connected = st.ConnectedAt != nil
	return out
}

type VoiceConnection struct {
	URL       string     `json:"url" doc:"LiveKit WebSocket URL to connect to"`
	Token     string     `json:"token" doc:"LiveKit access token for this channel's room; use it to connect before expires_at"`
	Room      string     `json:"room"`
	ExpiresAt time.Time  `json:"expires_at"`
	State     VoiceState `json:"state"`
}

func toVoiceConnection(c service.VoiceConnection) VoiceConnection {
	return VoiceConnection{URL: c.URL, Token: c.Token, Room: c.Room, ExpiresAt: c.ExpiresAt, State: toVoiceState(c.State)}
}

type VoiceQuality struct {
	Samples        int32   `json:"samples" doc:"Number of telemetry reports"`
	PacketLossAvg  float64 `json:"packet_loss_avg" doc:"Fraction of packets lost, 0 to 1"`
	PacketLossMax  float64 `json:"packet_loss_max"`
	JitterMsAvg    float64 `json:"jitter_ms_avg"`
	RttMsAvg       float64 `json:"rtt_ms_avg"`
	BitrateKbpsAvg float64 `json:"bitrate_kbps_avg"`
}

type VoiceSession struct {
	ID          string       `json:"id" format:"uuid"`
	ChannelID   string       `json:"channel_id" format:"uuid"`
	User        *User        `json:"user"`
	StartedAt   time.Time    `json:"started_at"`
	ConnectedAt *time.Time   `json:"connected_at" doc:"When the media server reported the user connected"`
	EndedAt     *time.Time   `json:"ended_at"`
	EndReason   *string      `json:"end_reason" enum:"left,switched,replaced,moved,removed,disconnected,timeout,permissions,session_ended,voice_disabled"`
	Quality     VoiceQuality `json:"quality"`
}

func toVoiceSession(v service.VoiceSessionView) VoiceSession {
	s := v.Session
	return VoiceSession{
		ID: s.ID.String(), ChannelID: s.ChannelID.String(), User: userPtr(v.User),
		StartedAt: s.StartedAt, ConnectedAt: s.ConnectedAt, EndedAt: s.EndedAt, EndReason: s.EndReason,
		Quality: VoiceQuality{
			Samples: s.TelemetrySamples, PacketLossAvg: s.PacketLossAvg, PacketLossMax: s.PacketLossMax,
			JitterMsAvg: s.JitterMsAvg, RttMsAvg: s.RttMsAvg, BitrateKbpsAvg: s.BitrateKbpsAvg,
		},
	}
}

type MemberVoice struct {
	Mute  bool        `json:"mute" doc:"Server-muted in this place's voice channels"`
	Deaf  bool        `json:"deaf" doc:"Server-deafened in this place's voice channels"`
	State *VoiceState `json:"state" doc:"The member's voice state, if they are in a voice channel of this place you can see"`
}

func toMemberVoice(v service.MemberVoiceView) MemberVoice {
	out := MemberVoice{Mute: v.Mute, Deaf: v.Deaf}
	if v.State != nil {
		st := toVoiceState(*v.State)
		out.State = &st
	}
	return out
}
