package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const (
	tagChannels = "Channels"
	tagMessages = "Messages"
	tagDMs      = "Direct messages"
	tagPresence = "Presence"
)

type ChannelPath struct {
	ChannelID string `path:"channelID" format:"uuid"`
}

func (p ChannelPath) id() (uuid.UUID, error) { return parseID("channelID", p.ChannelID) }

type ChannelRolePath struct {
	ChannelPath
	RoleID string `path:"roleID" format:"uuid"`
}

type ChannelUserPath struct {
	ChannelPath
	UserID string `path:"userID" format:"uuid"`
}

type ChannelMessagePath struct {
	ChannelPath
	MessageID string `path:"messageID" format:"uuid"`
}

type MessagePath struct {
	MessageID string `path:"messageID" format:"uuid"`
}

func (p MessagePath) id() (uuid.UUID, error) { return parseID("messageID", p.MessageID) }

type MessageReactionPath struct {
	MessagePath
	Emoji string `path:"emoji" minLength:"1" maxLength:"128" doc:"A Unicode emoji (URL-encoded) or a shortcode"`
}

// emoji returns the decoded emoji; routers may hand over the raw, still-escaped segment.
func (r MessageReactionPath) emoji() string {
	if strings.Contains(r.Emoji, "%") {
		if e, err := url.PathUnescape(r.Emoji); err == nil {
			return e
		}
	}
	return r.Emoji
}

type CreateChannelRequest struct {
	Kind      string `json:"kind,omitempty" enum:"text,voice,category" default:"text"`
	Name      string `json:"name" minLength:"1" maxLength:"100"`
	Topic     string `json:"topic,omitempty" maxLength:"1024"`
	ParentID  string `json:"parent_id,omitempty" format:"uuid" doc:"Category to place a text or voice channel in"`
	IsNSFW    bool   `json:"is_nsfw,omitempty"`
	UserLimit int32  `json:"user_limit,omitempty" minimum:"0" maximum:"99" doc:"Voice channels: maximum participants, 0 for unlimited"`
}

type UpdateChannelRequest struct {
	Name       *string `json:"name,omitempty" minLength:"1" maxLength:"100"`
	Topic      *string `json:"topic,omitempty" maxLength:"1024"`
	ParentID   *string `json:"parent_id,omitempty" doc:"Move into a category; empty string moves to the top level"`
	Position   *int32  `json:"position,omitempty" minimum:"0"`
	IsNSFW     *bool   `json:"is_nsfw,omitempty"`
	IsArchived *bool   `json:"is_archived,omitempty" doc:"Threads only"`
	UserLimit  *int32  `json:"user_limit,omitempty" minimum:"0" maximum:"99" doc:"Voice channels only; 0 for unlimited"`
}

type CreateThreadRequest struct {
	Name      string `json:"name,omitempty" maxLength:"100" doc:"Defaults to an excerpt of the starting message"`
	MessageID string `json:"message_id,omitempty" format:"uuid" doc:"Start the thread from this message in the channel"`
}

type ChannelSubscriptionRequest struct {
	Level string `json:"level" enum:"normal,muted" doc:"muted silences mention and direct message notifications from this channel"`
}

type ReadChannelRequest struct {
	MessageID string `json:"message_id" format:"uuid" doc:"The newest message the user has read"`
}

type ListMessagesInput struct {
	ChannelPath
	Before string `query:"before" format:"uuid" doc:"Messages older than this ID"`
	After  string `query:"after" format:"uuid" doc:"Messages newer than this ID"`
	Around string `query:"around" format:"uuid" doc:"Messages around (and including) this ID"`
	Limit  int32  `query:"limit" minimum:"1" maximum:"100" default:"50"`
}

type SendMessageRequest struct {
	Content       string   `json:"content,omitempty" maxLength:"4000" doc:"Markdown; @username mentions notify. May be empty when attachment_ids is set"`
	ReplyToID     string   `json:"reply_to_id,omitempty" format:"uuid" doc:"Reply to a message in the same channel; notifies its author"`
	Nonce         string   `json:"nonce,omitempty" maxLength:"64" doc:"Echoed on the response and MESSAGE_CREATE event so clients can match optimistic messages"`
	AttachmentIDs []string `json:"attachment_ids,omitempty" maxItems:"10" doc:"Files from POST /attachments, in display order"`
}

type EditMessageRequest struct {
	Content string `json:"content" maxLength:"4000" doc:"May be empty when the message has attachments"`
}

type OpenDMRequest struct {
	RecipientIDs []string `json:"recipient_ids" minItems:"1" maxItems:"9" doc:"One user opens (or returns) your direct conversation with them; several start a group conversation"`
	Name         string   `json:"name,omitempty" maxLength:"100" doc:"Group conversations only"`
}

type PresenceQuery struct {
	UserIDs []string `query:"user_ids" minItems:"1" maxItems:"100" doc:"Comma-separated user IDs"`
}

func (s *Server) registerChannels() {
	huma.Register(s.api, withAuth(operation("list-channels", http.MethodGet, "/places/{place}/channels",
		"List the categories, text and voice channels the caller can see, each category followed by its channels", tagChannels)),
		handle(s, func(ctx context.Context, in *PlacePath) (*Body[[]Channel], error) {
			chans, err := s.Service.ListChannels(ctx, mustPrincipal(ctx), in.Place)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(chans, toChannel))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("create-channel", http.MethodPost, "/places/{place}/channels",
		"Create a text channel, voice channel or category (MANAGE_CHANNELS)", tagChannels)), http.StatusCreated),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body CreateChannelRequest
		}) (*Body[Channel], error) {
			parent, err := parseOptionalID("parent_id", in.Body.ParentID)
			if err != nil {
				return nil, err
			}
			b := in.Body
			v, err := s.Service.CreateChannel(ctx, mustPrincipal(ctx), in.Place, service.CreateChannelInput{
				Kind: b.Kind, Name: b.Name, Topic: b.Topic, ParentID: parent, IsNSFW: b.IsNSFW, UserLimit: b.UserLimit,
			})
			if err != nil {
				return nil, err
			}
			return ok(toChannel(v))
		}))

	huma.Register(s.api, withAuth(operation("get-channel", http.MethodGet, "/channels/{channelID}",
		"Get a channel, thread or conversation", tagChannels)),
		handle(s, func(ctx context.Context, in *ChannelPath) (*Body[Channel], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			v, err := s.Service.GetChannel(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toChannel(v))
		}))

	huma.Register(s.api, withAuth(operation("update-channel", http.MethodPatch, "/channels/{channelID}",
		"Update a channel (MANAGE_CHANNELS), a thread (its creator or MANAGE_MESSAGES) or a group conversation's name", tagChannels)),
		handle(s, func(ctx context.Context, in *struct {
			ChannelPath
			Body UpdateChannelRequest
		}) (*Body[Channel], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			b := in.Body
			v, err := s.Service.UpdateChannel(ctx, mustPrincipal(ctx), id, service.ChannelUpdate{
				Name: b.Name, Topic: b.Topic, ParentID: b.ParentID, Position: b.Position, IsNSFW: b.IsNSFW, IsArchived: b.IsArchived,
				UserLimit: b.UserLimit,
			})
			if err != nil {
				return nil, err
			}
			return ok(toChannel(v))
		}))

	huma.Register(s.api, withAuth(operation("delete-channel", http.MethodDelete, "/channels/{channelID}",
		"Delete a channel with its messages and threads (MANAGE_CHANNELS), or a thread (its creator or MANAGE_MESSAGES)", tagChannels)),
		handle(s, func(ctx context.Context, in *ChannelPath) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteChannel(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withAuth(operation("list-channel-overwrites", http.MethodGet, "/channels/{channelID}/overwrites",
		"List a channel's role permission overwrites (MANAGE_CHANNELS)", tagChannels)),
		handle(s, func(ctx context.Context, in *ChannelPath) (*Body[[]Overwrite], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			ows, err := s.Service.ListChannelOverwrites(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(ows, toOverwrite))
		}))

	huma.Register(s.api, withAuth(operation("set-channel-overwrite", http.MethodPut, "/channels/{channelID}/overwrites/{roleID}",
		"Allow or deny permissions for a role within a category, text or voice channel (MANAGE_CHANNELS)", tagChannels)),
		handle(s, func(ctx context.Context, in *struct {
			ChannelRolePath
			Body OverwriteRequest
		}) (*Body[Overwrite], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			roleID, err := parseID("roleID", in.RoleID)
			if err != nil {
				return nil, err
			}
			o, err := s.Service.SetChannelOverwrite(ctx, mustPrincipal(ctx), id, roleID, in.Body.Allow, in.Body.Deny)
			if err != nil {
				return nil, err
			}
			return ok(toOverwrite(o))
		}))

	huma.Register(s.api, withAuth(operation("delete-channel-overwrite", http.MethodDelete, "/channels/{channelID}/overwrites/{roleID}",
		"Remove a role's overwrite from a channel (MANAGE_CHANNELS)", tagChannels)),
		handle(s, func(ctx context.Context, in *ChannelRolePath) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			roleID, err := parseID("roleID", in.RoleID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteChannelOverwrite(ctx, mustPrincipal(ctx), id, roleID)
		}))

	huma.Register(s.api, withAuth(operation("list-threads", http.MethodGet, "/channels/{channelID}/threads",
		"List a text channel's threads, most recently active first", tagChannels)),
		handle(s, func(ctx context.Context, in *struct {
			ChannelPath
			PageQuery
			Archived bool `query:"archived" doc:"List archived threads instead of active ones"`
		}) (*Body[Page[Channel]], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			page := in.pagination()
			threads, err := s.Service.ListThreads(ctx, mustPrincipal(ctx), id, in.Archived, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(threads, toChannel, page))
		}))

	huma.Register(s.api, withChatRateLimit(withStatus(withAuth(operation("create-thread", http.MethodPost, "/channels/{channelID}/threads",
		"Start a thread in a text channel, optionally from a message (SEND_MESSAGES)", tagChannels)), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *struct {
			ChannelPath
			Body CreateThreadRequest
		}) (*Body[Channel], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			msg, err := parseOptionalID("message_id", in.Body.MessageID)
			if err != nil {
				return nil, err
			}
			v, err := s.Service.CreateThread(ctx, mustPrincipal(ctx), id, service.CreateThreadInput{Name: in.Body.Name, MessageID: msg})
			if err != nil {
				return nil, err
			}
			return ok(toChannel(v))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("set-channel-subscription", http.MethodPut, "/channels/{channelID}/subscription",
		"Mute or unmute notifications from a channel or conversation", tagChannels)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *struct {
			ChannelPath
			Body ChannelSubscriptionRequest
		}) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			return nil, s.Service.SetChannelSubscription(ctx, mustPrincipal(ctx), id, in.Body.Level)
		}))

	huma.Register(s.api, withChatRateLimit(withStatus(withAuth(operation("trigger-typing", http.MethodPost, "/channels/{channelID}/typing",
		"Show the caller as typing for about 10 seconds (SEND_MESSAGES)", tagChannels)), http.StatusNoContent)),
		handle(s, func(ctx context.Context, in *ChannelPath) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			return nil, s.Service.StartTyping(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withAuth(operation("mark-channel-read", http.MethodPut, "/channels/{channelID}/read",
		"Record how far the caller has read; also reads the notifications it covers", tagChannels)),
		handle(s, func(ctx context.Context, in *struct {
			ChannelPath
			Body ReadChannelRequest
		}) (*Body[ChannelReadState], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			msg, err := parseID("message_id", in.Body.MessageID)
			if err != nil {
				return nil, err
			}
			st, err := s.Service.MarkChannelRead(ctx, mustPrincipal(ctx), id, msg)
			if err != nil {
				return nil, err
			}
			return ok(ChannelReadState{LastReadMessageID: idString(st.LastReadMessageID), MentionCount: st.MentionCount})
		}))

	huma.Register(s.api, withAuth(operation("list-read-receipts", http.MethodGet, "/channels/{channelID}/receipts",
		"How far each participant has read a direct or group conversation", tagDMs)),
		handle(s, func(ctx context.Context, in *ChannelPath) (*Body[[]ReadReceipt], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			rows, err := s.Service.ListReadReceipts(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(rows, toReadReceipt))
		}))
}

func (s *Server) registerMessages() {
	huma.Register(s.api, withAuth(operation("list-messages", http.MethodGet, "/channels/{channelID}/messages",
		"Page through history in chronological order; without a cursor, returns the latest messages", tagMessages)),
		handle(s, func(ctx context.Context, in *ListMessagesInput) (*Body[[]Message], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			var mq service.MessageQuery
			for _, c := range []struct {
				name string
				raw  string
				dst  **uuid.UUID
			}{{"before", in.Before, &mq.Before}, {"after", in.After, &mq.After}, {"around", in.Around, &mq.Around}} {
				if *c.dst, err = parseOptionalID(c.name, c.raw); err != nil {
					return nil, err
				}
			}
			mq.Limit = in.Limit
			msgs, err := s.Service.ListMessages(ctx, mustPrincipal(ctx), id, mq)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(msgs, toMessage))
		}))

	huma.Register(s.api, withChatRateLimit(withStatus(withAuth(operation("send-message", http.MethodPost, "/channels/{channelID}/messages",
		"Send a message (SEND_MESSAGES)", tagMessages)), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *struct {
			ChannelPath
			Body SendMessageRequest
		}) (*Body[Message], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			reply, err := parseOptionalID("reply_to_id", in.Body.ReplyToID)
			if err != nil {
				return nil, err
			}
			files, err := parseAttachmentIDs(in.Body.AttachmentIDs)
			if err != nil {
				return nil, err
			}
			m, err := s.Service.SendMessage(ctx, mustPrincipal(ctx), id, service.SendMessageInput{
				Content: in.Body.Content, ReplyToID: reply, Nonce: in.Body.Nonce, AttachmentIDs: files,
			})
			if err != nil {
				return nil, err
			}
			return ok(toMessage(m))
		}))

	huma.Register(s.api, withAuth(operation("get-message", http.MethodGet, "/messages/{messageID}",
		"Get a message", tagMessages)),
		handle(s, func(ctx context.Context, in *MessagePath) (*Body[Message], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			m, err := s.Service.GetMessage(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toMessage(m))
		}))

	huma.Register(s.api, withAuth(operation("edit-message", http.MethodPatch, "/messages/{messageID}",
		"Edit your own message; the previous version is kept", tagMessages)),
		handle(s, func(ctx context.Context, in *struct {
			MessagePath
			Body EditMessageRequest
		}) (*Body[Message], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			m, err := s.Service.EditMessage(ctx, mustPrincipal(ctx), id, in.Body.Content)
			if err != nil {
				return nil, err
			}
			return ok(toMessage(m))
		}))

	huma.Register(s.api, withAuth(operation("delete-message", http.MethodDelete, "/messages/{messageID}",
		"Delete a message (its author, or MANAGE_MESSAGES in place channels)", tagMessages)),
		handle(s, func(ctx context.Context, in *struct {
			MessagePath
			ReasonQuery
		}) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteMessage(ctx, mustPrincipal(ctx), id, in.Reason)
		}))

	huma.Register(s.api, withAuth(operation("list-message-revisions", http.MethodGet, "/messages/{messageID}/revisions",
		"A message's previous versions, newest first (its author or MANAGE_MESSAGES)", tagMessages)),
		handle(s, func(ctx context.Context, in *MessagePath) (*Body[[]MessageRevision], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			revs, err := s.Service.ListMessageRevisions(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(revs, toMessageRevision))
		}))

	huma.Register(s.api, withAuth(operation("list-pins", http.MethodGet, "/channels/{channelID}/pins",
		"A channel's pinned messages, most recently pinned first", tagMessages)),
		handle(s, func(ctx context.Context, in *ChannelPath) (*Body[[]Message], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			msgs, err := s.Service.ListPins(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(msgs, toMessage))
		}))

	for _, pin := range []bool{true, false} {
		method, opID, summary := http.MethodPut, "pin-message", "Pin a message (MANAGE_MESSAGES; any participant in direct messages)"
		status := http.StatusNoContent
		if !pin {
			method, opID, summary = http.MethodDelete, "unpin-message", "Unpin a message"
		}
		huma.Register(s.api, withStatus(withAuth(operation(opID, method, "/channels/{channelID}/pins/{messageID}", summary, tagMessages)), status),
			handle(s, func(ctx context.Context, in *ChannelMessagePath) (*struct{}, error) {
				id, err := in.id()
				if err != nil {
					return nil, err
				}
				msg, err := parseID("messageID", in.MessageID)
				if err != nil {
					return nil, err
				}
				return nil, s.Service.SetPinned(ctx, mustPrincipal(ctx), id, msg, pin)
			}))
	}

	huma.Register(s.api, withAuth(operation("list-message-reaction-users", http.MethodGet, "/messages/{messageID}/reactions/{emoji}",
		"List who reacted to a message with an emoji", tagMessages)),
		handle(s, func(ctx context.Context, in *struct {
			MessageReactionPath
			PageQuery
		}) (*Body[Page[User]], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			page := in.pagination()
			users, err := s.Service.ListMessageReactionUsers(ctx, mustPrincipal(ctx), id, in.emoji(), page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(users, toUser, page))
		}))

	huma.Register(s.api, withChatRateLimit(withStatus(withAuth(operation("add-message-reaction", http.MethodPut, "/messages/{messageID}/reactions/{emoji}",
		"React to a message (ADD_REACTIONS)", tagMessages)), http.StatusNoContent)),
		handle(s, func(ctx context.Context, in *MessageReactionPath) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			return nil, s.Service.AddMessageReaction(ctx, mustPrincipal(ctx), id, in.emoji())
		}))

	huma.Register(s.api, withAuth(operation("remove-message-reaction", http.MethodDelete, "/messages/{messageID}/reactions/{emoji}",
		"Remove the caller's reaction from a message", tagMessages)),
		handle(s, func(ctx context.Context, in *MessageReactionPath) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			return nil, s.Service.RemoveMessageReaction(ctx, mustPrincipal(ctx), id, in.emoji())
		}))
}

func (s *Server) registerDirectMessages() {
	huma.Register(s.api, withAuth(operation("list-direct-channels", http.MethodGet, "/users/@me/channels",
		"List the caller's direct and group conversations, most recently active first", tagDMs)),
		handle(s, func(ctx context.Context, in *PageQuery) (*Body[Page[Channel]], error) {
			page := in.pagination()
			chans, err := s.Service.ListDirectChannels(ctx, mustPrincipal(ctx), page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(chans, toChannel, page))
		}))

	huma.Register(s.api, withAuth(operation("open-direct-channel", http.MethodPost, "/users/@me/channels",
		"Open a conversation with people you share a place with", tagDMs)),
		handle(s, func(ctx context.Context, in *Body[OpenDMRequest]) (*Body[Channel], error) {
			ids := make([]uuid.UUID, len(in.Body.RecipientIDs))
			for i, raw := range in.Body.RecipientIDs {
				id, err := parseID("recipient_ids", raw)
				if err != nil {
					return nil, err
				}
				ids[i] = id
			}
			v, err := s.Service.OpenDirectChannel(ctx, mustPrincipal(ctx), service.OpenDMInput{RecipientIDs: ids, Name: in.Body.Name})
			if err != nil {
				return nil, err
			}
			return ok(toChannel(v))
		}))

	huma.Register(s.api, withAuth(operation("add-recipient", http.MethodPut, "/channels/{channelID}/recipients/{userID}",
		"Add someone to a group conversation", tagDMs)),
		handle(s, func(ctx context.Context, in *ChannelUserPath) (*Body[Channel], error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			userID, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			v, err := s.Service.AddRecipient(ctx, mustPrincipal(ctx), id, userID)
			if err != nil {
				return nil, err
			}
			return ok(toChannel(v))
		}))

	huma.Register(s.api, withAuth(operation("remove-recipient", http.MethodDelete, "/channels/{channelID}/recipients/{userID}",
		"Leave a group conversation (your own ID), or remove someone as its owner", tagDMs)),
		handle(s, func(ctx context.Context, in *ChannelUserPath) (*struct{}, error) {
			id, err := in.id()
			if err != nil {
				return nil, err
			}
			userID, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.RemoveRecipient(ctx, mustPrincipal(ctx), id, userID)
		}))

	huma.Register(s.api, withAuth(operation("get-presences", http.MethodGet, "/presences",
		"Online status of people who share a place or conversation with the caller", tagPresence)),
		handle(s, func(ctx context.Context, in *PresenceQuery) (*Body[[]Presence], error) {
			ids := make([]uuid.UUID, len(in.UserIDs))
			for i, raw := range in.UserIDs {
				id, err := parseID("user_ids", raw)
				if err != nil {
					return nil, err
				}
				ids[i] = id
			}
			visible, err := s.Service.PresenceVisible(ctx, mustPrincipal(ctx), ids)
			if err != nil {
				return nil, err
			}
			statuses, err := s.hub.Presences(ctx, visible)
			if err != nil {
				return nil, err
			}
			out := make([]Presence, 0, len(visible))
			for _, id := range visible {
				out = append(out, Presence{UserID: id.String(), Status: statuses[id]})
			}
			return ok(out)
		}))
}
