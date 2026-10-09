package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/realtime"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Gateway dispatch event types.
const (
	EventPlaceJoin          = "PLACE_JOIN"
	EventPlaceLeave         = "PLACE_LEAVE"
	EventPlaceDelete        = "PLACE_DELETE"
	EventChannelCreate      = "CHANNEL_CREATE"
	EventChannelUpdate      = "CHANNEL_UPDATE"
	EventChannelDelete      = "CHANNEL_DELETE"
	EventRecipientAdd       = "CHANNEL_RECIPIENT_ADD"
	EventRecipientRemove    = "CHANNEL_RECIPIENT_REMOVE"
	EventMessageCreate      = "MESSAGE_CREATE"
	EventMessageUpdate      = "MESSAGE_UPDATE"
	EventMessageDelete      = "MESSAGE_DELETE"
	EventReactionAdd        = "MESSAGE_REACTION_ADD"
	EventReactionRemove     = "MESSAGE_REACTION_REMOVE"
	EventTypingStart        = "TYPING_START"
	EventChannelRead        = "CHANNEL_READ"
	EventReadReceipt        = "READ_RECEIPT"
	EventNotificationCreate = "NOTIFICATION_CREATE"
	EventVoiceStateUpdate   = "VOICE_STATE_UPDATE"
	EventVoiceServerUpdate  = "VOICE_SERVER_UPDATE"
	EventVoiceSpeaking      = "VOICE_SPEAKING"
	// EventTopicReadStateUpdate syncs a user's forum read state (opened topics, positions)
	// between their sessions.
	EventTopicReadStateUpdate = "TOPIC_READ_STATE_UPDATE"
)

// Event is a real-time update for gateway clients. Data is a service view (or a plain map)
// that the Publisher converts to its public JSON form; the remaining fields route it.
type Event struct {
	Type      string
	Data      any
	Users     []uuid.UUID
	Places    []uuid.UUID
	ChannelID *uuid.UUID
	Control   *realtime.Control
}

// Publisher delivers events to the gateway. Implementations must not block for long.
type Publisher interface {
	Publish(ctx context.Context, ev Event)
}

type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, Event) {}

// SetPublisher connects the service to the real-time gateway. Without one, events are dropped.
func (s *Service) SetPublisher(p Publisher) {
	if p == nil {
		p = nopPublisher{}
	}
	s.events.Store(&p)
}

func (s *Service) publisher() Publisher {
	if p := s.events.Load(); p != nil {
		return *p
	}
	return nopPublisher{}
}

// emit publishes ev once the transaction that q belongs to commits (or right away when q
// is not transactional), so clients never hear about changes that were rolled back.
func (s *Service) emit(ctx context.Context, q *store.Queries, ev Event) {
	if st, ok := s.pending.Load(q); ok {
		state := st.(*txState)
		state.events = append(state.events, ev)
		return
	}
	s.publish(ctx, ev)
}

// queueVoiceSync re-checks the voice participants of a place (only users, when given)
// once the transaction commits: those who lost access are disconnected and the others get
// their SFU permissions updated.
func (s *Service) queueVoiceSync(ctx context.Context, q *store.Queries, placeID uuid.UUID, users ...uuid.UUID) {
	st, ok := s.pending.Load(q)
	if !ok {
		s.syncVoice(ctx, placeID, users)
		return
	}
	state := st.(*txState)
	if state.voice == nil {
		state.voice = map[uuid.UUID][]uuid.UUID{}
	}
	prev, seen := state.voice[placeID]
	switch {
	case seen && prev == nil:
	case len(users) == 0:
		state.voice[placeID] = nil
	default:
		state.voice[placeID] = append(prev, users...)
	}
}

func (s *Service) publish(ctx context.Context, ev Event) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	s.publisher().Publish(ctx, ev)
}

func (s *Service) emitJoined(ctx context.Context, q *store.Queries, placeID, userID uuid.UUID) {
	s.emit(ctx, q, Event{
		Type: EventPlaceJoin, Data: map[string]any{"place_id": placeID}, Users: []uuid.UUID{userID},
		Control: &realtime.Control{PlaceID: &placeID, Joined: []uuid.UUID{userID}},
	})
}

func (s *Service) emitLeft(ctx context.Context, q *store.Queries, placeID, userID uuid.UUID) {
	s.emit(ctx, q, Event{
		Type: EventPlaceLeave, Data: map[string]any{"place_id": placeID}, Users: []uuid.UUID{userID},
		Control: &realtime.Control{PlaceID: &placeID, Left: []uuid.UUID{userID}},
	})
	s.queueVoiceSync(ctx, q, placeID, userID)
}

// emitPermissionsChanged makes gateways re-check channel visibility for a whole place, or
// only for users when given, and re-checks the place's voice participants.
func (s *Service) emitPermissionsChanged(ctx context.Context, q *store.Queries, placeID uuid.UUID, users ...uuid.UUID) {
	ctl := &realtime.Control{PlaceID: &placeID, Invalidate: len(users) == 0, Refresh: users}
	s.emit(ctx, q, Event{Control: ctl})
	s.queueVoiceSync(ctx, q, placeID, users...)
}

// emitSessionsEnded disconnects gateway connections and voice of ended sessions: the
// listed ones, or all of the user's sessions except keep.
func (s *Service) emitSessionsEnded(ctx context.Context, q *store.Queries, userID uuid.UUID, sessions []uuid.UUID, keep *uuid.UUID) {
	s.emit(ctx, q, Event{Control: &realtime.Control{CloseUser: &userID, CloseSessions: sessions, KeepSession: keep}})
	s.afterCommit(ctx, q, func(ctx context.Context) { s.endVoiceForSessions(ctx, userID, sessions, keep) })
}
