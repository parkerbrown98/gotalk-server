package service

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const (
	maxMessageLen          = 4000
	maxPinsPerChannel      = 50
	maxReactionsPerMessage = 20
	maxNonceLen            = 64
)

func validateMessageContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return apperr.Invalid("content must not be empty")
	}
	if utf8.RuneCountInString(content) > maxMessageLen {
		return apperr.Invalid("content must be at most %d characters", maxMessageLen)
	}
	return nil
}

// MessageRef summarizes the message a reply points at.
type MessageRef struct {
	Message store.Message
	Author  *store.User
}

// MessageView is a message with its author, reply target, reactions and thread.
type MessageView struct {
	Message   store.Message
	Author    *store.User
	ReplyTo   *MessageRef
	Reactions []ReactionSummary
	Thread    *store.Channel
	// Nonce echoes the client's value on the create response and MESSAGE_CREATE event.
	Nonce string
}

// messageViews decorates messages. viewer decides each reaction's "me" flag (uuid.Nil for
// gateway events, which are shared by every recipient).
func (s *Service) messageViews(ctx context.Context, q *store.Queries, viewer uuid.UUID, msgs []store.Message) ([]MessageView, error) {
	out := make([]MessageView, len(msgs))
	if len(msgs) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(msgs))
	var userIDs, replyIDs []uuid.UUID
	for i, m := range msgs {
		ids[i] = m.ID
		if m.AuthorID != nil {
			userIDs = append(userIDs, *m.AuthorID)
		}
		if m.ReplyToID != nil {
			replyIDs = append(replyIDs, *m.ReplyToID)
		}
	}
	replies := map[uuid.UUID]store.Message{}
	if len(replyIDs) > 0 {
		rows, err := q.ListMessagesByIDs(ctx, dedupe(replyIDs))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			replies[r.ID] = r
			if r.AuthorID != nil {
				userIDs = append(userIDs, *r.AuthorID)
			}
		}
	}
	users, err := s.usersByID(ctx, q, userIDs)
	if err != nil {
		return nil, err
	}
	reactions := map[uuid.UUID][]ReactionSummary{}
	rows, err := q.MessageReactionSummaries(ctx, store.MessageReactionSummariesParams{ViewerID: viewer, MessageIds: ids})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		reactions[r.MessageID] = append(reactions[r.MessageID], ReactionSummary{Emoji: r.Emoji, Count: r.Count, Me: r.Me})
	}
	threads := map[uuid.UUID]store.Channel{}
	ths, err := q.ListThreadsByMessage(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, t := range ths {
		threads[*t.ThreadMessageID] = t
	}
	for i, m := range msgs {
		v := MessageView{Message: m, Author: ptrUser(users, m.AuthorID), Reactions: reactions[m.ID]}
		if v.Reactions == nil {
			v.Reactions = []ReactionSummary{}
		}
		if m.ReplyToID != nil {
			if r, ok := replies[*m.ReplyToID]; ok {
				v.ReplyTo = &MessageRef{Message: r, Author: ptrUser(users, r.AuthorID)}
			}
		}
		if t, ok := threads[m.ID]; ok {
			v.Thread = &t
		}
		out[i] = v
	}
	return out, nil
}

func (s *Service) messageView(ctx context.Context, q *store.Queries, viewer uuid.UUID, m store.Message) (MessageView, error) {
	views, err := s.messageViews(ctx, q, viewer, []store.Message{m})
	if err != nil {
		return MessageView{}, err
	}
	return views[0], nil
}

// messageFor resolves a message in a channel the caller can see.
func (s *Service) messageFor(ctx context.Context, q *store.Queries, p *Principal, messageID uuid.UUID) (*channelCtx, store.Message, error) {
	m, err := q.GetMessage(ctx, messageID)
	if err != nil {
		return nil, m, notFound(err, "message not found")
	}
	cc, err := s.channelFor(ctx, q, p, m.ChannelID)
	if err != nil {
		return nil, m, hideAsNotFound(err, "message not found")
	}
	return cc, m, nil
}

// MessageQuery selects a page of history. At most one cursor may be set; with none, the
// latest messages are returned.
type MessageQuery struct {
	Before *uuid.UUID
	After  *uuid.UUID
	Around *uuid.UUID
	Limit  int32
}

// ListMessages returns up to Limit messages in chronological order.
func (s *Service) ListMessages(ctx context.Context, p *Principal, channelID uuid.UUID, mq MessageQuery) ([]MessageView, error) {
	set := 0
	for _, c := range []*uuid.UUID{mq.Before, mq.After, mq.Around} {
		if c != nil {
			set++
		}
	}
	if set > 1 {
		return nil, apperr.Invalid("use at most one of before, after and around")
	}
	if mq.Limit <= 0 || mq.Limit > 100 {
		mq.Limit = 50
	}
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return nil, err
	}
	if cc.ch.Kind == kindCategory {
		return nil, apperr.Invalid("categories do not have messages")
	}
	var msgs []store.Message
	switch {
	case mq.After != nil:
		msgs, err = s.q.ListMessagesAfter(ctx, store.ListMessagesAfterParams{ChannelID: cc.ch.ID, After: *mq.After, Lim: mq.Limit})
	case mq.Around != nil:
		msgs, err = s.messagesAround(ctx, cc.ch.ID, *mq.Around, mq.Limit)
	default:
		msgs, err = s.q.ListMessagesBefore(ctx, store.ListMessagesBeforeParams{ChannelID: cc.ch.ID, Before: mq.Before, Lim: mq.Limit})
		slices.Reverse(msgs)
	}
	if err != nil {
		return nil, err
	}
	return s.messageViews(ctx, s.q, p.User.ID, msgs)
}

func (s *Service) messagesAround(ctx context.Context, channelID, around uuid.UUID, limit int32) ([]store.Message, error) {
	before, err := s.q.ListMessagesBefore(ctx, store.ListMessagesBeforeParams{ChannelID: channelID, Before: &around, Lim: limit / 2})
	if err != nil {
		return nil, err
	}
	slices.Reverse(before)
	out := before
	afterLimit := limit - limit/2
	if m, err := s.q.GetMessage(ctx, around); err == nil && m.ChannelID == channelID {
		out = append(out, m)
		afterLimit--
	}
	after, err := s.q.ListMessagesAfter(ctx, store.ListMessagesAfterParams{ChannelID: channelID, After: around, Lim: afterLimit})
	if err != nil {
		return nil, err
	}
	return append(out, after...), nil
}

func (s *Service) GetMessage(ctx context.Context, p *Principal, messageID uuid.UUID) (MessageView, error) {
	_, m, err := s.messageFor(ctx, s.q, p, messageID)
	if err != nil {
		return MessageView{}, err
	}
	return s.messageView(ctx, s.q, p.User.ID, m)
}

// viewersAmong keeps the users who are members of the channel's place and can view it.
func (s *Service) viewersAmong(ctx context.Context, q *store.Queries, cc *channelCtx, users []uuid.UUID) ([]uuid.UUID, error) {
	if len(users) == 0 {
		return nil, nil
	}
	st, err := s.standings(ctx, q, cc.scope.place, cc.scope.defaultRole, users)
	if err != nil {
		return nil, err
	}
	var out []uuid.UUID
	for _, u := range users {
		if m, ok := st[u]; ok && cc.scope.visibleTo(m, cc.permID) {
			out = append(out, u)
		}
	}
	return out, nil
}

// unmuted drops users who muted the channel or, for place channels, its parent channel,
// category or place. The most specific setting wins.
func (s *Service) unmuted(ctx context.Context, q *store.Queries, cc *channelCtx, users []uuid.UUID) ([]uuid.UUID, error) {
	if len(users) == 0 {
		return nil, nil
	}
	targets := []uuid.UUID{cc.ch.ID}
	if cc.scope != nil {
		chain := cc.scope.chain(cc.permID)
		for i := len(chain) - 1; i >= 0; i-- {
			targets = append(targets, chain[i].ID)
		}
		targets = append(targets, cc.scope.place.ID)
	}
	targets = dedupe(targets)
	rows, err := q.ListSubscriptionsFor(ctx, store.ListSubscriptionsForParams{UserIds: users, TargetIds: targets})
	if err != nil {
		return nil, err
	}
	levels := map[uuid.UUID]map[uuid.UUID]string{}
	for _, r := range rows {
		if levels[r.UserID] == nil {
			levels[r.UserID] = map[uuid.UUID]string{}
		}
		levels[r.UserID][r.TargetID] = r.Level
	}
	var out []uuid.UUID
	for _, u := range users {
		muted := false
		for _, t := range targets {
			if level, ok := levels[u][t]; ok {
				muted = level == "muted"
				break
			}
		}
		if !muted {
			out = append(out, u)
		}
	}
	return out, nil
}

// notifyTargets works out who a place-channel message notifies: mentioned users and the
// replied-to author who can see the channel, excluding the author.
func (s *Service) notifyTargets(ctx context.Context, q *store.Queries, cc *channelCtx, author uuid.UUID, content string, exclude []string, replyAuthor *uuid.UUID) (mentioned, replied []uuid.UUID, err error) {
	ids, err := s.mentionedUserIDs(ctx, q, content, exclude)
	if err != nil {
		return nil, nil, err
	}
	ids = slicesFilter(ids, func(id uuid.UUID) bool { return id != author })
	if replyAuthor != nil && *replyAuthor != author && !slices.Contains(ids, *replyAuthor) {
		replied = []uuid.UUID{*replyAuthor}
	}
	visible, err := s.viewersAmong(ctx, q, cc, append(slices.Clone(ids), replied...))
	if err != nil {
		return nil, nil, err
	}
	for _, id := range visible {
		if slices.Contains(ids, id) {
			mentioned = append(mentioned, id)
		}
	}
	if len(replied) > 0 && !slices.Contains(visible, replied[0]) {
		replied = nil
	}
	return mentioned, replied, nil
}

func (s *Service) messageNotificationData(cc *channelCtx, content string) map[string]any {
	data := map[string]any{"channel_name": cc.ch.Name, "excerpt": excerpt(content)}
	if cc.scope != nil {
		data["place_name"], data["place_slug"] = cc.scope.place.Name, cc.scope.place.Slug
	}
	return data
}

// notifyMessage stores mention, reply and direct-message notifications for a new or
// edited message and bumps recipients' unread mention counts.
func (s *Service) notifyMessage(ctx context.Context, q *store.Queries, cc *channelCtx, m store.Message, mentioned, replied []uuid.UUID, dmRecipients []uuid.UUID) error {
	bump := append(append(slices.Clone(mentioned), replied...), dmRecipients...)
	if len(bump) > 0 {
		if err := q.AddMentions(ctx, store.AddMentionsParams{UserIds: bump, ChannelID: cc.ch.ID, MessageID: m.ID}); err != nil {
			return err
		}
	}
	rs := newRecipients(*m.AuthorID)
	rs.add(NotifyMention, mentioned...)
	rs.add(NotifyReply, replied...)
	rs.add(NotifyDirectMessage, dmRecipients...)
	ids := make([]uuid.UUID, len(rs.order))
	for i, r := range rs.order {
		ids[i] = r.userID
	}
	keep, err := s.unmuted(ctx, q, cc, ids)
	if err != nil {
		return err
	}
	out := slicesFilter(rs.order, func(r recipient) bool { return slices.Contains(keep, r.userID) })
	return s.insertNotifications(ctx, q, out,
		notifTarget{placeID: cc.placeID(), channelID: &cc.ch.ID, messageID: &m.ID},
		*m.AuthorID, s.messageNotificationData(cc, m.Content))
}

// canMessageUser checks whether a one-to-one conversation with other may continue: their
// account must exist and the two must share a place, unless either is an instance admin.
func (s *Service) canMessageUser(ctx context.Context, q *store.Queries, p *Principal, other uuid.UUID) error {
	u, err := q.GetUserByID(ctx, other)
	if err != nil {
		return notFound(err, "user not found")
	}
	if p.User.IsInstanceAdmin || u.IsInstanceAdmin {
		return nil
	}
	shares, err := q.SharesPlace(ctx, store.SharesPlaceParams{UserA: p.User.ID, UserB: other})
	if err != nil {
		return err
	}
	if !shares {
		return apperr.Forbidden("you can only message people you share a place with")
	}
	return nil
}

type SendMessageInput struct {
	Content   string
	ReplyToID *uuid.UUID
	Nonce     string
}

// SendMessage posts a message (SEND_MESSAGES). Mentions and replies notify people who can
// see the channel; direct messages notify every other recipient.
func (s *Service) SendMessage(ctx context.Context, p *Principal, channelID uuid.UUID, in SendMessageInput) (MessageView, error) {
	if err := validateMessageContent(in.Content); err != nil {
		return MessageView{}, err
	}
	if len(in.Nonce) > maxNonceLen {
		return MessageView{}, apperr.Invalid("nonce must be at most %d characters", maxNonceLen)
	}
	var view MessageView
	err := s.tx(ctx, func(q *store.Queries) error {
		ch, err := q.GetChannelForUpdate(ctx, channelID)
		if err != nil {
			return notFound(err, "channel not found")
		}
		cc, err := s.channelCtxOf(ctx, q, p, ch)
		if err != nil {
			return err
		}
		if ch.Kind == kindCategory {
			return apperr.Invalid("categories do not have messages")
		}
		if err := cc.require(permissions.SendMessages); err != nil {
			return err
		}
		var others []uuid.UUID
		for _, r := range cc.recipients {
			if r != p.User.ID {
				others = append(others, r)
			}
		}
		if ch.Kind == kindDM {
			if len(others) != 1 {
				return apperr.Forbidden("this conversation has ended")
			}
			if err := s.canMessageUser(ctx, q, p, others[0]); err != nil {
				return err
			}
		}
		var replyAuthor *uuid.UUID
		if in.ReplyToID != nil {
			target, err := q.GetMessage(ctx, *in.ReplyToID)
			if err != nil || target.ChannelID != ch.ID {
				return apperr.Invalid("reply_to_id must be a message in this channel")
			}
			replyAuthor = target.AuthorID
		}
		var mentioned, replied []uuid.UUID
		if !cc.isDM() {
			if mentioned, replied, err = s.notifyTargets(ctx, q, cc, p.User.ID, in.Content, nil, replyAuthor); err != nil {
				return err
			}
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		m, err := q.CreateMessage(ctx, store.CreateMessageParams{
			ID: id, ChannelID: ch.ID, PlaceID: ch.PlaceID, AuthorID: &p.User.ID, Content: in.Content,
			ReplyToID: in.ReplyToID, MentionIds: append(append([]uuid.UUID{}, mentioned...), replied...),
		})
		if err != nil {
			return err
		}
		if err := q.RecordChannelMessage(ctx, store.RecordChannelMessageParams{ID: ch.ID, MessageID: &m.ID, CreatedAt: &m.CreatedAt}); err != nil {
			return err
		}
		if ch.IsArchived {
			ch.IsArchived = false
			ch.LastMessageID, ch.LastMessageAt = &m.ID, &m.CreatedAt
			ch.MessageCount++
			if err := s.emitChannelChange(ctx, q, EventChannelUpdate, ch, false); err != nil {
				return err
			}
		}
		if _, err := s.ack(ctx, q, p.User.ID, cc, m.ID); err != nil {
			return err
		}
		pub, err := s.messageView(ctx, q, uuid.Nil, m)
		if err != nil {
			return err
		}
		pub.Nonce = in.Nonce
		s.emit(ctx, q, cc.event(EventMessageCreate, pub))
		view = pub
		var dmRecipients []uuid.UUID
		if cc.isDM() {
			dmRecipients = others
		}
		return s.notifyMessage(ctx, q, cc, m, mentioned, replied, dmRecipients)
	})
	return view, err
}

// EditMessage replaces a message's content, keeping the previous version. Only the author
// may edit, while they can still send messages in the channel. Newly added mentions notify.
func (s *Service) EditMessage(ctx context.Context, p *Principal, messageID uuid.UUID, content string) (MessageView, error) {
	if err := validateMessageContent(content); err != nil {
		return MessageView{}, err
	}
	var view MessageView
	err := s.tx(ctx, func(q *store.Queries) error {
		cc, m, err := s.messageFor(ctx, q, p, messageID)
		if err != nil {
			return err
		}
		if !isAuthor(m.AuthorID, p) {
			return apperr.Forbidden("you can only edit your own messages")
		}
		if err := cc.require(permissions.SendMessages); err != nil {
			return err
		}
		if content == m.Content {
			view, err = s.messageView(ctx, q, p.User.ID, m)
			return err
		}
		revID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if err := q.CreateMessageRevision(ctx, store.CreateMessageRevisionParams{ID: revID, MessageID: m.ID, Content: m.Content}); err != nil {
			return err
		}
		var added []uuid.UUID
		mentionIDs := append([]uuid.UUID{}, m.MentionIds...)
		if !cc.isDM() {
			if added, _, err = s.notifyTargets(ctx, q, cc, p.User.ID, content, mentions(m.Content), nil); err != nil {
				return err
			}
			added = slicesFilter(added, func(id uuid.UUID) bool { return !slices.Contains(m.MentionIds, id) })
			mentionIDs = append(mentionIDs, added...)
		}
		updated, err := q.UpdateMessageContent(ctx, store.UpdateMessageContentParams{ID: m.ID, Content: content, MentionIds: mentionIDs})
		if err != nil {
			return err
		}
		if err := s.notifyMessage(ctx, q, cc, updated, added, nil, nil); err != nil {
			return err
		}
		pub, err := s.messageView(ctx, q, uuid.Nil, updated)
		if err != nil {
			return err
		}
		s.emit(ctx, q, cc.event(EventMessageUpdate, pub))
		view, err = s.messageView(ctx, q, p.User.ID, updated)
		return err
	})
	return view, err
}

func messageRef(cc *channelCtx, m store.Message) map[string]any {
	return map[string]any{"id": m.ID, "channel_id": m.ChannelID, "place_id": cc.placeID()}
}

// DeleteMessage removes a message. Authors may delete their own; MANAGE_MESSAGES may
// delete any message in a place channel, which is audited and tells the author why.
func (s *Service) DeleteMessage(ctx context.Context, p *Principal, messageID uuid.UUID, reason string) error {
	if len([]rune(reason)) > maxReasonLen {
		return apperr.Invalid("reason must be at most %d characters", maxReasonLen)
	}
	return s.tx(ctx, func(q *store.Queries) error {
		cc, m, err := s.messageFor(ctx, q, p, messageID)
		if err != nil {
			return err
		}
		if _, err := q.GetChannelForUpdate(ctx, cc.ch.ID); err != nil {
			return err
		}
		author := isAuthor(m.AuthorID, p)
		if !author && !cc.canModerate() {
			if cc.isDM() {
				return apperr.Forbidden("you can only delete your own messages")
			}
			return missing(permissions.ManageMessages)
		}
		if _, err := q.DeleteMessage(ctx, m.ID); err != nil {
			return err
		}
		if err := q.RefreshChannelAfterDelete(ctx, cc.ch.ID); err != nil {
			return err
		}
		s.emit(ctx, q, cc.event(EventMessageDelete, messageRef(cc, m)))
		if author {
			return nil
		}
		place := cc.scope.place
		if err := s.audit(ctx, q, place.ID, p, "message.delete", "message", &m.ID, reason,
			map[string]any{"channel_id": cc.ch.ID, "author_id": m.AuthorID, "content": m.Content}); err != nil {
			return err
		}
		if m.AuthorID == nil {
			return nil
		}
		return s.notifyModeration(ctx, q, place, *m.AuthorID, p, "message.delete", reason,
			map[string]any{"channel_id": cc.ch.ID, "channel_name": cc.ch.Name, "excerpt": excerpt(m.Content)})
	})
}

// ListMessageRevisions returns a message's previous versions, newest first, to its author
// or to MANAGE_MESSAGES.
func (s *Service) ListMessageRevisions(ctx context.Context, p *Principal, messageID uuid.UUID) ([]store.MessageRevision, error) {
	cc, m, err := s.messageFor(ctx, s.q, p, messageID)
	if err != nil {
		return nil, err
	}
	if !isAuthor(m.AuthorID, p) && !cc.canModerate() {
		return nil, apperr.Forbidden("only the author or a moderator can see edit history")
	}
	return s.q.ListMessageRevisions(ctx, m.ID)
}

// SetPinned pins or unpins a message. Any recipient may pin in direct messages; place
// channels need MANAGE_MESSAGES.
func (s *Service) SetPinned(ctx context.Context, p *Principal, channelID, messageID uuid.UUID, pinned bool) error {
	return s.tx(ctx, func(q *store.Queries) error {
		cc, m, err := s.messageFor(ctx, q, p, messageID)
		if err != nil {
			return err
		}
		if m.ChannelID != channelID {
			return apperr.NotFound("message not found")
		}
		if _, err := q.GetChannelForUpdate(ctx, cc.ch.ID); err != nil {
			return err
		}
		if !cc.isDM() && !cc.has(permissions.ManageMessages) {
			return missing(permissions.ManageMessages)
		}
		if m.IsPinned == pinned {
			return nil
		}
		if pinned {
			n, err := q.CountPins(ctx, cc.ch.ID)
			if err != nil {
				return err
			}
			if n >= maxPinsPerChannel {
				return apperr.Conflict("a channel can have at most %d pinned messages", maxPinsPerChannel)
			}
		}
		updated, err := q.SetMessagePinned(ctx, store.SetMessagePinnedParams{ID: m.ID, Pinned: pinned, PinnedBy: &p.User.ID})
		if err != nil {
			return err
		}
		pub, err := s.messageView(ctx, q, uuid.Nil, updated)
		if err != nil {
			return err
		}
		s.emit(ctx, q, cc.event(EventMessageUpdate, pub))
		if cc.isDM() {
			return nil
		}
		action := map[bool]string{true: "message.pin", false: "message.unpin"}[pinned]
		return s.audit(ctx, q, cc.scope.place.ID, p, action, "message", &m.ID, "", map[string]any{"channel_id": cc.ch.ID})
	})
}

// ListPins returns a channel's pinned messages, most recently pinned first.
func (s *Service) ListPins(ctx context.Context, p *Principal, channelID uuid.UUID) ([]MessageView, error) {
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return nil, err
	}
	msgs, err := s.q.ListPins(ctx, cc.ch.ID)
	if err != nil {
		return nil, err
	}
	return s.messageViews(ctx, s.q, p.User.ID, msgs)
}

func reactionData(cc *channelCtx, m store.Message, userID uuid.UUID, emoji string) map[string]any {
	d := messageRef(cc, m)
	d["message_id"], d["user_id"], d["emoji"] = m.ID, userID, emoji
	delete(d, "id")
	return d
}

// AddMessageReaction reacts to a message (ADD_REACTIONS). Adding the same reaction twice
// is a no-op.
func (s *Service) AddMessageReaction(ctx context.Context, p *Principal, messageID uuid.UUID, emoji string) error {
	if err := validateEmoji(emoji); err != nil {
		return err
	}
	return s.tx(ctx, func(q *store.Queries) error {
		cc, m, err := s.messageFor(ctx, q, p, messageID)
		if err != nil {
			return err
		}
		if err := cc.require(permissions.AddReactions); err != nil {
			return err
		}
		exists, err := q.MessageHasEmoji(ctx, store.MessageHasEmojiParams{MessageID: m.ID, Emoji: emoji})
		if err != nil {
			return err
		}
		if !exists {
			n, err := q.CountDistinctMessageReactions(ctx, m.ID)
			if err != nil {
				return err
			}
			if n >= maxReactionsPerMessage {
				return apperr.Conflict("a message can have at most %d different reactions", maxReactionsPerMessage)
			}
		}
		added, err := q.AddMessageReaction(ctx, store.AddMessageReactionParams{MessageID: m.ID, UserID: p.User.ID, Emoji: emoji})
		if err != nil || added == 0 {
			return err
		}
		s.emit(ctx, q, cc.event(EventReactionAdd, reactionData(cc, m, p.User.ID, emoji)))
		return nil
	})
}

func (s *Service) RemoveMessageReaction(ctx context.Context, p *Principal, messageID uuid.UUID, emoji string) error {
	cc, m, err := s.messageFor(ctx, s.q, p, messageID)
	if err != nil {
		return err
	}
	n, err := s.q.RemoveMessageReaction(ctx, store.RemoveMessageReactionParams{MessageID: m.ID, UserID: p.User.ID, Emoji: emoji})
	if err != nil || n == 0 {
		return err
	}
	s.emit(ctx, s.q, cc.event(EventReactionRemove, reactionData(cc, m, p.User.ID, emoji)))
	return nil
}

// ListMessageReactionUsers lists who reacted to a message with an emoji, earliest first.
func (s *Service) ListMessageReactionUsers(ctx context.Context, p *Principal, messageID uuid.UUID, emoji string, page Pagination) ([]store.User, error) {
	_, m, err := s.messageFor(ctx, s.q, p, messageID)
	if err != nil {
		return nil, err
	}
	page = page.normalized()
	rows, err := s.q.ListMessageReactionUsers(ctx, store.ListMessageReactionUsersParams{
		MessageID: m.ID, Emoji: emoji, Lim: page.Limit, Off: page.Offset,
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.User, len(rows))
	for i, r := range rows {
		out[i] = r.User
	}
	return out, nil
}

// ack moves a user's read position forward and recounts their unread mentions.
func (s *Service) ack(ctx context.Context, q *store.Queries, userID uuid.UUID, cc *channelCtx, messageID uuid.UUID) (store.ChannelRead, error) {
	if err := q.AckChannel(ctx, store.AckChannelParams{UserID: userID, ChannelID: cc.ch.ID, MessageID: &messageID}); err != nil {
		return store.ChannelRead{}, err
	}
	return q.RecountMentions(ctx, store.RecountMentionsParams{UserID: userID, ChannelID: cc.ch.ID, IsDm: cc.isDM()})
}

// MarkChannelRead records that the caller has read a channel up to a message, reads the
// notifications it caused, syncs the caller's other sessions and, in direct messages,
// sends a read receipt to the other recipients.
func (s *Service) MarkChannelRead(ctx context.Context, p *Principal, channelID, messageID uuid.UUID) (ReadState, error) {
	var state ReadState
	err := s.tx(ctx, func(q *store.Queries) error {
		cc, err := s.channelFor(ctx, q, p, channelID)
		if err != nil {
			return err
		}
		m, err := q.GetMessage(ctx, messageID)
		if err != nil || m.ChannelID != cc.ch.ID {
			return apperr.Invalid("message_id must be a message in this channel")
		}
		row, err := s.ack(ctx, q, p.User.ID, cc, messageID)
		if err != nil {
			return err
		}
		if err := q.MarkChannelNotificationsRead(ctx, store.MarkChannelNotificationsReadParams{
			UserID: p.User.ID, ChannelID: &cc.ch.ID, MessageID: row.LastReadMessageID,
		}); err != nil {
			return err
		}
		state = ReadState{LastReadMessageID: row.LastReadMessageID, MentionCount: row.MentionCount}
		s.emit(ctx, q, Event{Type: EventChannelRead, Users: []uuid.UUID{p.User.ID}, Data: map[string]any{
			"channel_id": cc.ch.ID, "last_read_message_id": row.LastReadMessageID, "mention_count": row.MentionCount,
		}})
		if cc.isDM() {
			others := slicesFilter(cc.recipients, func(id uuid.UUID) bool { return id != p.User.ID })
			s.emit(ctx, q, Event{Type: EventReadReceipt, Users: others, Data: map[string]any{
				"channel_id": cc.ch.ID, "user_id": p.User.ID, "last_read_message_id": row.LastReadMessageID,
			}})
		}
		return nil
	})
	return state, err
}

// ListReadReceipts returns how far each recipient of a direct message has read.
func (s *Service) ListReadReceipts(ctx context.Context, p *Principal, channelID uuid.UUID) ([]store.ChannelRead, error) {
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return nil, err
	}
	if !cc.isDM() {
		return nil, apperr.Invalid("read receipts are only shared in direct messages")
	}
	rows, err := s.q.ListChannelReceipts(ctx, cc.ch.ID)
	if err != nil {
		return nil, err
	}
	return slicesFilter(rows, func(r store.ChannelRead) bool { return slices.Contains(cc.recipients, r.UserID) }), nil
}

// StartTyping tells everyone in the channel the caller is typing. Clients repeat it every
// few seconds while typing and treat it as expired after 10 seconds.
func (s *Service) StartTyping(ctx context.Context, p *Principal, channelID uuid.UUID) error {
	cc, err := s.channelFor(ctx, s.q, p, channelID)
	if err != nil {
		return err
	}
	if cc.ch.Kind == kindCategory {
		return apperr.Invalid("categories do not have messages")
	}
	if err := cc.require(permissions.SendMessages); err != nil {
		return err
	}
	s.emit(ctx, s.q, cc.event(EventTypingStart, map[string]any{
		"channel_id": cc.ch.ID, "place_id": cc.placeID(), "user_id": p.User.ID, "timestamp": time.Now().UTC(),
	}))
	return nil
}
