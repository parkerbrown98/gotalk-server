package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

type ChannelReadState struct {
	LastReadMessageID *string `json:"last_read_message_id" format:"uuid"`
	MentionCount      int32   `json:"mention_count" doc:"Unread messages that mention or reply to the caller; every unread message in direct messages"`
}

type Channel struct {
	ID              string     `json:"id" format:"uuid"`
	PlaceID         *string    `json:"place_id" format:"uuid" doc:"null for direct messages"`
	ParentID        *string    `json:"parent_id" format:"uuid" doc:"The category of a text channel, or the text channel of a thread"`
	Kind            string     `json:"kind" enum:"category,text,thread,dm,group_dm"`
	Name            string     `json:"name"`
	Topic           string     `json:"topic"`
	Position        int32      `json:"position" doc:"Order among siblings, ascending"`
	IsNSFW          bool       `json:"is_nsfw"`
	OwnerID         *string    `json:"owner_id" format:"uuid" doc:"Thread creator or group conversation owner"`
	ThreadMessageID *string    `json:"thread_message_id" format:"uuid" doc:"The message a thread was started from"`
	IsArchived      bool       `json:"is_archived" doc:"Archived threads are hidden from default listings; a new message unarchives them"`
	MessageCount    int32      `json:"message_count"`
	LastMessageID   *string    `json:"last_message_id" format:"uuid"`
	LastMessageAt   *time.Time `json:"last_message_at"`
	CreatedAt       time.Time  `json:"created_at"`
	Recipients      []User     `json:"recipients,omitempty" doc:"Participants of a direct or group conversation"`
	// Caller state, absent from gateway events.
	MyPermissions *int64            `json:"my_permissions,omitempty" doc:"Caller's effective permission bits in this channel"`
	ReadState     *ChannelReadState `json:"read_state,omitempty"`
	Unread        *bool             `json:"unread,omitempty" doc:"Whether there are messages after the caller's read position"`
	Subscription  *string           `json:"subscription,omitempty" enum:"normal,muted"`
}

func toChannel(v service.ChannelView) Channel {
	c := v.Channel
	out := Channel{
		ID: c.ID.String(), PlaceID: idString(c.PlaceID), ParentID: idString(c.ParentID), Kind: c.Kind,
		Name: c.Name, Topic: c.Topic, Position: c.Position, IsNSFW: c.IsNsfw, OwnerID: idString(c.OwnerID),
		ThreadMessageID: idString(c.ThreadMessageID), IsArchived: c.IsArchived, MessageCount: c.MessageCount,
		LastMessageID: idString(c.LastMessageID), LastMessageAt: c.LastMessageAt, CreatedAt: c.CreatedAt,
	}
	if c.PlaceID == nil {
		out.Recipients = mapSlice(v.Recipients, toUser)
		if out.Recipients == nil {
			out.Recipients = []User{}
		}
	}
	if v.Permissions != nil {
		perms := int64(*v.Permissions)
		out.MyPermissions = &perms
	}
	if v.ReadState != nil {
		out.ReadState = &ChannelReadState{LastReadMessageID: idString(v.ReadState.LastReadMessageID), MentionCount: v.ReadState.MentionCount}
		unread := c.LastMessageID != nil && (v.ReadState.LastReadMessageID == nil ||
			c.LastMessageID.String() > v.ReadState.LastReadMessageID.String())
		out.Unread = &unread
	}
	if v.Subscription != "" {
		sub := v.Subscription
		out.Subscription = &sub
	}
	return out
}

type MessageReference struct {
	ID      string `json:"id" format:"uuid"`
	Author  *User  `json:"author"`
	Excerpt string `json:"excerpt"`
}

type ThreadSummary struct {
	ID            string     `json:"id" format:"uuid"`
	Name          string     `json:"name"`
	MessageCount  int32      `json:"message_count"`
	LastMessageAt *time.Time `json:"last_message_at"`
	IsArchived    bool       `json:"is_archived"`
}

type Message struct {
	ID        string            `json:"id" format:"uuid" doc:"Time-ordered (UUIDv7); usable as a pagination cursor"`
	ChannelID string            `json:"channel_id" format:"uuid"`
	PlaceID   *string           `json:"place_id" format:"uuid" doc:"null in direct messages"`
	Author    *User             `json:"author" doc:"null if the author deleted their account"`
	Content   string            `json:"content" doc:"Markdown"`
	ReplyToID *string           `json:"reply_to_id" format:"uuid"`
	ReplyTo   *MessageReference `json:"reply_to" doc:"The replied-to message; null if it was deleted"`
	Mentions  []string          `json:"mentions" doc:"IDs of users this message notified (mentions and the replied-to author)"`
	Reactions []Reaction        `json:"reactions"`
	IsPinned  bool              `json:"is_pinned"`
	PinnedAt  *time.Time        `json:"pinned_at"`
	Thread    *ThreadSummary    `json:"thread" doc:"The thread started from this message, if any"`
	EditCount int32             `json:"edit_count"`
	EditedAt  *time.Time        `json:"edited_at"`
	CreatedAt time.Time         `json:"created_at"`
	Nonce     string            `json:"nonce,omitempty" doc:"Echo of the client's nonce, on the send response and MESSAGE_CREATE only"`
}

func toMessage(v service.MessageView) Message {
	m := v.Message
	out := Message{
		ID: m.ID.String(), ChannelID: m.ChannelID.String(), PlaceID: idString(m.PlaceID), Author: userPtr(v.Author),
		Content: m.Content, ReplyToID: idString(m.ReplyToID), IsPinned: m.IsPinned, PinnedAt: m.PinnedAt,
		EditCount: m.EditCount, EditedAt: m.EditedAt, CreatedAt: m.CreatedAt, Nonce: v.Nonce,
		Mentions:  mapSlice(m.MentionIds, uuid.UUID.String),
		Reactions: mapSlice(v.Reactions, func(r service.ReactionSummary) Reaction { return Reaction(r) }),
	}
	if v.ReplyTo != nil {
		out.ReplyTo = &MessageReference{ID: v.ReplyTo.Message.ID.String(), Author: userPtr(v.ReplyTo.Author),
			Excerpt: excerptOf(v.ReplyTo.Message.Content)}
	}
	if t := v.Thread; t != nil {
		out.Thread = &ThreadSummary{ID: t.ID.String(), Name: t.Name, MessageCount: t.MessageCount,
			LastMessageAt: t.LastMessageAt, IsArchived: t.IsArchived}
	}
	return out
}

// excerptOf shortens reply previews.
func excerptOf(content string) string {
	r := []rune(content)
	if len(r) <= 200 {
		return content
	}
	return string(r[:200]) + "…"
}

type MessageRevision struct {
	ID        string    `json:"id" format:"uuid"`
	Content   string    `json:"content" doc:"The content before this edit"`
	CreatedAt time.Time `json:"created_at"`
}

func toMessageRevision(r store.MessageRevision) MessageRevision {
	return MessageRevision{ID: r.ID.String(), Content: r.Content, CreatedAt: r.CreatedAt}
}

type ReadReceipt struct {
	UserID            string    `json:"user_id" format:"uuid"`
	LastReadMessageID *string   `json:"last_read_message_id" format:"uuid"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func toReadReceipt(r store.ChannelRead) ReadReceipt {
	return ReadReceipt{UserID: r.UserID.String(), LastReadMessageID: idString(r.LastReadMessageID), UpdatedAt: r.UpdatedAt}
}

type Presence struct {
	UserID string `json:"user_id" format:"uuid"`
	Status string `json:"status" enum:"online,idle,dnd,offline"`
}
