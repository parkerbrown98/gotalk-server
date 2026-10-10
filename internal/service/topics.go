package service

import (
	"context"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const (
	maxTitleLen        = 200
	maxPostLen         = 50000
	maxTagsPerTopic    = 5
	maxTopicSlugLen    = 80
	maxReactionsPerPos = 20
)

var (
	tagPattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	tagPrefixPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	shortcodePattern = regexp.MustCompile(`^[a-z0-9_+-]{1,32}$`)
)

func normalizeTitle(title string) (string, error) {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" || utf8.RuneCountInString(title) > maxTitleLen {
		return "", apperr.Invalid("title must be 1-%d characters", maxTitleLen)
	}
	return title, nil
}

// validateContent checks post content; it may only be empty when the post has files.
func validateContent(content string, attachments int) error {
	if strings.TrimSpace(content) == "" && attachments == 0 {
		return apperr.Invalid("content must not be empty")
	}
	if utf8.RuneCountInString(content) > maxPostLen {
		return apperr.Invalid("content must be at most %d characters", maxPostLen)
	}
	return nil
}

func normalizeTag(tag string) string { return strings.ToLower(strings.TrimSpace(tag)) }

func normalizeTags(tags []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		t = normalizeTag(t)
		if !tagPattern.MatchString(t) {
			return nil, apperr.Invalid("tags must be 1-32 lowercase letters, numbers or '-', got %q", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) > maxTagsPerTopic {
		return nil, apperr.Invalid("a topic can have at most %d tags", maxTagsPerTopic)
	}
	return out, nil
}

// slugify derives a readable URL segment from a title. Topic URLs always include the ID,
// so slugs need not be unique.
func slugify(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if utf8.RuneCountInString(b.String()) >= maxTopicSlugLen {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "topic"
	}
	return slug
}

// validateEmoji accepts a Unicode emoji sequence or a shortcode such as "thumbsup".
func validateEmoji(e string) error {
	if shortcodePattern.MatchString(e) {
		return nil
	}
	if e == "" || len(e) > 32 || !utf8.ValidString(e) {
		return apperr.Invalid("emoji must be a Unicode emoji or a shortcode of 1-32 characters")
	}
	nonASCII := false
	for _, r := range e {
		switch {
		case r >= 0x80:
			nonASCII = true
			if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.IsLetter(r) {
				return apperr.Invalid("emoji must be a Unicode emoji or a shortcode")
			}
		case r >= '0' && r <= '9', r == '#', r == '*': // keycap sequences
		default:
			return apperr.Invalid("emoji must be a Unicode emoji or a shortcode")
		}
	}
	if !nonASCII {
		return apperr.Invalid("emoji must be a Unicode emoji or a shortcode")
	}
	return nil
}

// TopicView is a topic plus related users and the caller's state.
type TopicView struct {
	Topic      store.Topic
	Author     *store.User
	LastPoster *store.User
	// The remaining fields are caller state, only set for authenticated callers.
	LastReadPostNumber *int32
	Subscription       string
	// Opened reports whether the caller has opened the topic (and not marked it unread
	// since); SeenPostNumber is the topic's last post number at that open.
	Opened         bool
	SeenPostNumber int32
	// Vote is the caller's vote: 1, -1 or 0.
	Vote int16
}

// HasNewReplies reports whether posts arrived after the caller last opened the topic.
func (v TopicView) HasNewReplies() bool {
	return v.Opened && v.Topic.LastPostNumber > v.SeenPostNumber
}

// NewReplyCount is how many posts arrived after the caller last opened the topic (deleted ones
// included, since post numbers are never reused).
func (v TopicView) NewReplyCount() int32 {
	if !v.HasNewReplies() {
		return 0
	}
	return v.Topic.LastPostNumber - v.SeenPostNumber
}

type ReactionSummary struct {
	Emoji string
	Count int32
	Me    bool
}

// PostView is a post as the caller may see it. Deleted posts keep their place in the
// thread with empty content unless the caller can manage posts.
type PostView struct {
	Post        store.Post
	Author      *store.User
	Depth       *int32
	Reactions   []ReactionSummary
	Attachments []store.Upload
	Embeds      []LinkPreview
	Deleted     bool
}

// topicScope resolves a live topic the caller can see.
func (s *Service) topicScope(ctx context.Context, q *store.Queries, p *Principal, topicID uuid.UUID) (*forumScope, store.Topic, error) {
	t, err := q.GetTopic(ctx, topicID)
	if err != nil {
		return nil, t, notFound(err, "topic not found")
	}
	if t.DeletedAt != nil {
		return nil, t, apperr.NotFound("topic not found")
	}
	f, err := s.forum(ctx, q, p, t.PlaceID.String())
	if err != nil {
		return nil, t, hideAsNotFound(err, "topic not found")
	}
	if !f.canView(t.BoardID) {
		return nil, t, apperr.NotFound("topic not found")
	}
	return f, t, nil
}

func (s *Service) postScope(ctx context.Context, q *store.Queries, p *Principal, postID uuid.UUID) (*forumScope, store.Topic, store.Post, error) {
	post, err := q.GetPost(ctx, postID)
	if err != nil {
		return nil, store.Topic{}, post, notFound(err, "post not found")
	}
	f, topic, err := s.topicScope(ctx, q, p, post.TopicID)
	if err != nil {
		return nil, topic, post, hideAsNotFound(err, "post not found")
	}
	return f, topic, post, nil
}

// canModerate reports whether the caller holds MANAGE_POSTS in the board as a member.
func (f *forumScope) canModerate(boardID uuid.UUID) bool {
	return f.acc.IsMember && f.has(boardID, permissions.ManagePosts)
}

// requireParticipant checks the caller may take part (not just read) in a board.
func (f *forumScope) requireParticipant(boardID uuid.UUID, perm permissions.Permission) error {
	if !f.acc.IsMember {
		return apperr.Forbidden("join this place to participate")
	}
	if !f.has(boardID, perm) {
		if f.viewer.TimedOut {
			return apperr.Forbidden("you are timed out in this place")
		}
		return missing(perm)
	}
	return nil
}

func isAuthor(author *uuid.UUID, p *Principal) bool {
	return p != nil && author != nil && *author == p.User.ID
}

func ptrUser(m map[uuid.UUID]store.User, id *uuid.UUID) *store.User {
	if id == nil {
		return nil
	}
	if u, ok := m[*id]; ok {
		return &u
	}
	return nil
}

func (s *Service) topicViews(ctx context.Context, q *store.Queries, p *Principal, topics []store.Topic) ([]TopicView, error) {
	var userIDs, topicIDs []uuid.UUID
	for _, t := range topics {
		topicIDs = append(topicIDs, t.ID)
		for _, id := range []*uuid.UUID{t.AuthorID, t.LastPosterID} {
			if id != nil {
				userIDs = append(userIDs, *id)
			}
		}
	}
	users, err := s.usersByID(ctx, q, userIDs)
	if err != nil {
		return nil, err
	}
	reads := map[uuid.UUID]store.ListTopicReadsRow{}
	subs := map[uuid.UUID]string{}
	votes := map[uuid.UUID]int16{}
	if p != nil && len(topicIDs) > 0 {
		rows, err := q.ListTopicReads(ctx, store.ListTopicReadsParams{UserID: p.User.ID, TopicIds: topicIDs})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			reads[r.TopicID] = r
		}
		voteRows, err := q.ListTopicVotes(ctx, store.ListTopicVotesParams{UserID: p.User.ID, TopicIds: topicIDs})
		if err != nil {
			return nil, err
		}
		for _, r := range voteRows {
			votes[r.TopicID] = r.Value
		}
		subRows, err := q.ListUserSubscriptions(ctx, store.ListUserSubscriptionsParams{UserID: p.User.ID, TargetIds: topicIDs})
		if err != nil {
			return nil, err
		}
		for _, r := range subRows {
			subs[r.TargetID] = r.Level
		}
	}
	out := make([]TopicView, len(topics))
	for i, t := range topics {
		v := TopicView{Topic: t, Author: ptrUser(users, t.AuthorID), LastPoster: ptrUser(users, t.LastPosterID)}
		if p != nil {
			if r, ok := reads[t.ID]; ok {
				n := r.LastReadPostNumber
				v.LastReadPostNumber = &n
				v.Opened = r.OpenedAt != nil
				v.SeenPostNumber = r.SeenPostNumber
			}
			v.Vote = votes[t.ID]
			v.Subscription = subs[t.ID]
			if v.Subscription == "" {
				v.Subscription = "normal"
			}
		}
		out[i] = v
	}
	return out, nil
}

func (s *Service) postViews(ctx context.Context, q *store.Queries, p *Principal, f *forumScope, posts []store.Post, depths []int32) ([]PostView, error) {
	var userIDs, postIDs []uuid.UUID
	for _, post := range posts {
		postIDs = append(postIDs, post.ID)
		if post.AuthorID != nil {
			userIDs = append(userIDs, *post.AuthorID)
		}
	}
	users, err := s.usersByID(ctx, q, userIDs)
	if err != nil {
		return nil, err
	}
	reactions := map[uuid.UUID][]ReactionSummary{}
	files := map[uuid.UUID][]store.Upload{}
	if len(postIDs) > 0 {
		viewer := uuid.Nil
		if p != nil {
			viewer = p.User.ID
		}
		rows, err := q.ReactionSummaries(ctx, store.ReactionSummariesParams{ViewerID: viewer, PostIds: postIDs})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			reactions[r.PostID] = append(reactions[r.PostID], ReactionSummary{Emoji: r.Emoji, Count: r.Count, Me: r.Me})
		}
		atts, err := q.ListPostAttachments(ctx, postIDs)
		if err != nil {
			return nil, err
		}
		for _, a := range atts {
			files[a.PostID] = append(files[a.PostID], a.Upload)
		}
	}
	contents := make([]string, len(posts))
	for i, post := range posts {
		contents[i] = post.Content
	}
	embeds, err := s.linkPreviews(ctx, q, contents)
	if err != nil {
		return nil, err
	}
	out := make([]PostView, len(posts))
	for i, post := range posts {
		v := PostView{Post: post, Author: ptrUser(users, post.AuthorID), Reactions: reactions[post.ID],
			Attachments: files[post.ID], Embeds: embeds[i]}
		if v.Reactions == nil {
			v.Reactions = []ReactionSummary{}
		}
		if depths != nil {
			d := depths[i]
			v.Depth = &d
		}
		if post.DeletedAt != nil {
			v.Deleted = true
			if !f.canModerate(post.BoardID) {
				v.Post.Content = ""
				v.Reactions = []ReactionSummary{}
				v.Attachments, v.Embeds = nil, nil
			}
		}
		out[i] = v
	}
	return out, nil
}

func (s *Service) postView(ctx context.Context, q *store.Queries, p *Principal, f *forumScope, post store.Post) (PostView, error) {
	views, err := s.postViews(ctx, q, p, f, []store.Post{post}, nil)
	if err != nil {
		return PostView{}, err
	}
	return views[0], nil
}

func (s *Service) topicView(ctx context.Context, q *store.Queries, p *Principal, t store.Topic) (TopicView, error) {
	views, err := s.topicViews(ctx, q, p, []store.Topic{t})
	if err != nil {
		return TopicView{}, err
	}
	return views[0], nil
}

type TopicFilter struct {
	Tag             string
	IncludeArchived bool
}

func optionalTag(tag string) (*string, error) {
	tag = normalizeTag(tag)
	if tag == "" {
		return nil, nil
	}
	if !tagPattern.MatchString(tag) {
		return nil, apperr.Invalid("tag is invalid")
	}
	return &tag, nil
}

// ListBoardTopics lists a board's topics, pinned first, then by latest activity.
func (s *Service) ListBoardTopics(ctx context.Context, p *Principal, boardID uuid.UUID, filter TopicFilter, page Pagination) ([]TopicView, error) {
	tag, err := optionalTag(filter.Tag)
	if err != nil {
		return nil, err
	}
	if _, _, err := s.boardScope(ctx, s.q, p, boardID); err != nil {
		return nil, err
	}
	page = page.normalized()
	topics, err := s.q.ListBoardTopics(ctx, store.ListBoardTopicsParams{
		BoardID: boardID, IncludeArchived: filter.IncludeArchived, Tag: tag, Lim: page.Limit, Off: page.Offset,
	})
	if err != nil {
		return nil, err
	}
	return s.topicViews(ctx, s.q, p, topics)
}

// ListPlaceTopics lists the latest active topics across every board the caller can see.
func (s *Service) ListPlaceTopics(ctx context.Context, p *Principal, ref, tagFilter string, page Pagination) ([]TopicView, error) {
	tag, err := optionalTag(tagFilter)
	if err != nil {
		return nil, err
	}
	f, err := s.forum(ctx, s.q, p, ref)
	if err != nil {
		return nil, err
	}
	page = page.normalized()
	topics, err := s.q.ListPlaceTopics(ctx, store.ListPlaceTopicsParams{
		PlaceID: f.place.ID, BoardIds: f.visibleBoardIDs(), Tag: tag, Lim: page.Limit, Off: page.Offset,
	})
	if err != nil {
		return nil, err
	}
	return s.topicViews(ctx, s.q, p, topics)
}

func (s *Service) GetTopic(ctx context.Context, p *Principal, topicID uuid.UUID) (TopicView, error) {
	_, t, err := s.topicScope(ctx, s.q, p, topicID)
	if err != nil {
		return TopicView{}, err
	}
	return s.topicView(ctx, s.q, p, t)
}

type TagCount struct {
	Tag        string
	TopicCount int64
}

// ListTags returns tags used in the place's visible boards, most used first, for
// autocomplete and tag clouds.
func (s *Service) ListTags(ctx context.Context, p *Principal, ref, prefix string, limit int32) ([]TagCount, error) {
	var pp *string
	if prefix = normalizeTag(prefix); prefix != "" {
		if !tagPrefixPattern.MatchString(prefix) {
			return []TagCount{}, nil
		}
		pp = &prefix
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	f, err := s.forum(ctx, s.q, p, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListPlaceTags(ctx, store.ListPlaceTagsParams{
		PlaceID: f.place.ID, BoardIds: f.visibleBoardIDs(), Prefix: pp, Lim: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]TagCount, len(rows))
	for i, r := range rows {
		out[i] = TagCount{Tag: r.Tag, TopicCount: r.TopicCount}
	}
	return out, nil
}

type CreateTopicInput struct {
	Title   string
	Content string
	Tags    []string
	// AttachmentIDs are files from UploadAttachment for the opening post, in display order.
	AttachmentIDs []uuid.UUID
}

// insertPost appends a post to a locked topic and indexes it for search.
func (s *Service) insertPost(ctx context.Context, q *store.Queries, topic store.Topic, author uuid.UUID, parentID *uuid.UUID, content string, files []store.Upload) (store.Post, error) {
	num, err := q.ClaimPostNumber(ctx, store.ClaimPostNumberParams{ID: topic.ID, PosterID: &author})
	if err != nil {
		return store.Post{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.Post{}, err
	}
	post, err := q.CreatePost(ctx, store.CreatePostParams{
		ID: id, TopicID: topic.ID, PlaceID: topic.PlaceID, BoardID: topic.BoardID, AuthorID: &author,
		ParentID: parentID, PostNumber: num, Content: content,
	})
	if err != nil {
		return store.Post{}, err
	}
	if len(files) > 0 {
		if err := q.AddPostAttachments(ctx, store.AddPostAttachmentsParams{PostID: post.ID, UploadIds: uploadIDs(files)}); err != nil {
			return store.Post{}, err
		}
	}
	s.resolveLinks(ctx, q, content, func(context.Context) {})
	if err := s.indexPost(ctx, q, topic, post); err != nil {
		return store.Post{}, err
	}
	if err := q.AdjustBoardCounts(ctx, store.AdjustBoardCountsParams{ID: topic.BoardID, Posts: 1, Touch: true}); err != nil {
		return store.Post{}, err
	}
	if err := s.refreshRanks(ctx, q, topic.ID); err != nil {
		return store.Post{}, err
	}
	_, err = q.MarkTopicRead(ctx, store.MarkTopicReadParams{UserID: author, TopicID: topic.ID, PostNumber: num, SeenPostNumber: num})
	return post, err
}

func (s *Service) indexPost(ctx context.Context, q *store.Queries, topic store.Topic, post store.Post) error {
	title := ""
	if post.PostNumber == 1 {
		title = topic.Title
	}
	return q.UpsertSearchDocument(ctx, store.UpsertSearchDocumentParams{PostID: post.ID, Title: title, Content: post.Content})
}

// CreateTopic starts a topic with its opening post. The author watches it automatically.
func (s *Service) CreateTopic(ctx context.Context, p *Principal, boardID uuid.UUID, in CreateTopicInput) (TopicView, PostView, error) {
	title, err := normalizeTitle(in.Title)
	if err != nil {
		return TopicView{}, PostView{}, err
	}
	if err := validateContent(in.Content, len(in.AttachmentIDs)); err != nil {
		return TopicView{}, PostView{}, err
	}
	tags, err := normalizeTags(in.Tags)
	if err != nil {
		return TopicView{}, PostView{}, err
	}

	var (
		tv TopicView
		pv PostView
	)
	err = s.tx(ctx, func(q *store.Queries) error {
		f, b, err := s.boardScope(ctx, q, p, boardID)
		if err != nil {
			return err
		}
		if b.Kind != "board" {
			return apperr.Invalid("topics can only be posted in boards, not categories")
		}
		if err := f.requireParticipant(b.ID, permissions.CreateTopics); err != nil {
			return err
		}
		files, err := s.claimAttachments(ctx, q, p, in.AttachmentIDs)
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		topic, err := q.CreateTopic(ctx, store.CreateTopicParams{
			ID: id, PlaceID: f.place.ID, BoardID: b.ID, AuthorID: &p.User.ID, Title: title, Slug: slugify(title), Tags: tags,
		})
		if err != nil {
			return err
		}
		post, err := s.insertPost(ctx, q, topic, p.User.ID, nil, in.Content, files)
		if err != nil {
			return err
		}
		if err := q.AdjustBoardCounts(ctx, store.AdjustBoardCountsParams{ID: b.ID, Topics: 1}); err != nil {
			return err
		}
		if err := q.EnsureSubscription(ctx, store.EnsureSubscriptionParams{
			UserID: p.User.ID, TargetType: "topic", TargetID: topic.ID, PlaceID: &f.place.ID, Level: "watching",
		}); err != nil {
			return err
		}
		if topic, err = q.GetTopic(ctx, topic.ID); err != nil {
			return err
		}

		rs := newRecipients(p.User.ID)
		mentioned, err := s.mentionedUserIDs(ctx, q, in.Content, nil)
		if err != nil {
			return err
		}
		rs.add(NotifyMention, mentioned...)
		scopes := []uuid.UUID{f.place.ID}
		for _, cb := range f.chain(b.ID) {
			scopes = append(scopes, cb.ID)
		}
		watchers, err := q.ListWatchers(ctx, scopes)
		if err != nil {
			return err
		}
		rs.add(NotifyNewTopic, watchers...)
		if err := s.deliver(ctx, q, forumEvent{f: f, topic: topic, post: &post, actorID: p.User.ID}, rs); err != nil {
			return err
		}
		if err := s.queueForumWebhook(ctx, q, f, WebhookTopicCreate, topic, post); err != nil {
			return err
		}

		if tv, err = s.topicView(ctx, q, p, topic); err != nil {
			return err
		}
		pv, err = s.postView(ctx, q, p, f, post)
		return err
	})
	return tv, pv, err
}

type TopicUpdate struct {
	Title      *string
	Tags       *[]string
	IsPinned   *bool
	IsLocked   *bool
	IsArchived *bool
	BoardID    *uuid.UUID
}

// UpdateTopic edits a topic. Authors may change the title and tags of their open topics;
// MANAGE_POSTS is needed for anything else, including moving between boards (which also
// requires MANAGE_POSTS in the destination).
func (s *Service) UpdateTopic(ctx context.Context, p *Principal, topicID uuid.UUID, in TopicUpdate) (TopicView, error) {
	params := store.UpdateTopicParams{ID: topicID, IsPinned: in.IsPinned, IsLocked: in.IsLocked, IsArchived: in.IsArchived}
	if in.Title != nil {
		title, err := normalizeTitle(*in.Title)
		if err != nil {
			return TopicView{}, err
		}
		slug := slugify(title)
		params.Title, params.Slug = &title, &slug
	}
	if in.Tags != nil {
		tags, err := normalizeTags(*in.Tags)
		if err != nil {
			return TopicView{}, err
		}
		params.Tags = tags
	}

	var view TopicView
	err := s.tx(ctx, func(q *store.Queries) error {
		f, topic, err := s.topicScope(ctx, q, p, topicID)
		if err != nil {
			return err
		}
		if topic, err = q.GetTopicForUpdate(ctx, topic.ID); err != nil {
			return err
		}
		mod := f.canModerate(topic.BoardID)
		author := isAuthor(topic.AuthorID, p)
		modFields := in.IsPinned != nil || in.IsLocked != nil || in.IsArchived != nil || in.BoardID != nil
		if modFields && !mod {
			return missing(permissions.ManagePosts)
		}
		if (in.Title != nil || in.Tags != nil) && !mod {
			if !author {
				return apperr.Forbidden("you can only edit your own topics")
			}
			if topic.IsLocked || topic.IsArchived {
				return apperr.Forbidden("this topic is locked")
			}
			if err := f.requireParticipant(topic.BoardID, permissions.ViewBoards); err != nil {
				return err
			}
			if f.viewer.TimedOut {
				return apperr.Forbidden("you are timed out in this place")
			}
		}

		moved := false
		if in.BoardID != nil && *in.BoardID != topic.BoardID {
			dest, err := f.board(*in.BoardID)
			if err != nil {
				return apperr.Invalid("destination board not found")
			}
			if dest.Kind != "board" {
				return apperr.Invalid("topics can only be moved to boards, not categories")
			}
			if !f.has(dest.ID, permissions.ManagePosts) {
				return apperr.Forbidden("you need MANAGE_POSTS in the destination board")
			}
			params.BoardID = &dest.ID
			moved = true
		}

		updated, err := q.UpdateTopic(ctx, params)
		if err != nil {
			return err
		}
		if moved {
			if err := q.MoveTopicPosts(ctx, store.MoveTopicPostsParams{TopicID: topic.ID, BoardID: updated.BoardID}); err != nil {
				return err
			}
			if err := q.AdjustBoardCounts(ctx, store.AdjustBoardCountsParams{ID: topic.BoardID, Topics: -1, Posts: -topic.PostCount}); err != nil {
				return err
			}
			if err := q.AdjustBoardCounts(ctx, store.AdjustBoardCountsParams{ID: updated.BoardID, Topics: 1, Posts: topic.PostCount}); err != nil {
				return err
			}
		}
		if in.Title != nil {
			first, err := q.GetPostByNumber(ctx, store.GetPostByNumberParams{TopicID: topic.ID, PostNumber: 1})
			if err != nil {
				return err
			}
			if err := s.indexPost(ctx, q, updated, first); err != nil {
				return err
			}
		}
		if modFields || !author {
			meta := map[string]any{"title": updated.Title}
			setIf(meta, "is_pinned", in.IsPinned)
			setIf(meta, "is_locked", in.IsLocked)
			setIf(meta, "is_archived", in.IsArchived)
			if moved {
				meta["from_board_id"], meta["to_board_id"] = topic.BoardID, updated.BoardID
			}
			if err := s.audit(ctx, q, f.place.ID, p, "topic.update", "topic", &topic.ID, "", meta); err != nil {
				return err
			}
		}
		view, err = s.topicView(ctx, q, p, updated)
		return err
	})
	return view, err
}

func setIf[T any](m map[string]any, key string, v *T) {
	if v != nil {
		m[key] = *v
	}
}

// DeleteTopic soft-deletes a topic. Authors may delete their own topic until someone
// replies; moderators may delete any topic.
func (s *Service) DeleteTopic(ctx context.Context, p *Principal, topicID uuid.UUID, reason string) error {
	if len(reason) > 512 {
		return apperr.Invalid("reason must be at most 512 characters")
	}
	return s.tx(ctx, func(q *store.Queries) error {
		f, topic, err := s.topicScope(ctx, q, p, topicID)
		if err != nil {
			return err
		}
		if topic, err = q.GetTopicForUpdate(ctx, topic.ID); err != nil {
			return err
		}
		mod := f.canModerate(topic.BoardID)
		author := isAuthor(topic.AuthorID, p)
		if !mod {
			if !author {
				return missing(permissions.ManagePosts)
			}
			if topic.PostCount > 1 {
				return apperr.Conflict("topics with replies can only be deleted by a moderator")
			}
		}
		if err := q.SoftDeleteTopic(ctx, store.SoftDeleteTopicParams{ID: topic.ID, DeletedBy: &p.User.ID}); err != nil {
			return err
		}
		if err := q.AdjustBoardCounts(ctx, store.AdjustBoardCountsParams{ID: topic.BoardID, Topics: -1, Posts: -topic.PostCount}); err != nil {
			return err
		}
		if author {
			return nil
		}
		if err := s.audit(ctx, q, f.place.ID, p, "topic.delete", "topic", &topic.ID, reason, map[string]any{"title": topic.Title}); err != nil {
			return err
		}
		if topic.AuthorID == nil {
			return nil
		}
		return s.notifyModeration(ctx, q, f.place, *topic.AuthorID, p, "topic.delete", reason,
			map[string]any{"topic_title": topic.Title})
	})
}

// ListPosts returns a page of a topic's posts: chronological in flat boards, depth-first
// in threaded boards.
func (s *Service) ListPosts(ctx context.Context, p *Principal, topicID uuid.UUID, page Pagination) ([]PostView, error) {
	f, topic, err := s.topicScope(ctx, s.q, p, topicID)
	if err != nil {
		return nil, err
	}
	page = page.normalized()
	if f.boards[topic.BoardID].ReplyMode == "threaded" {
		rows, err := s.q.ListTopicPostsThreaded(ctx, store.ListTopicPostsThreadedParams{TopicID: topic.ID, Lim: page.Limit, Off: page.Offset})
		if err != nil {
			return nil, err
		}
		posts := make([]store.Post, len(rows))
		depths := make([]int32, len(rows))
		for i, r := range rows {
			posts[i], depths[i] = r.Post, r.Depth
		}
		return s.postViews(ctx, s.q, p, f, posts, depths)
	}
	posts, err := s.q.ListTopicPosts(ctx, store.ListTopicPostsParams{TopicID: topic.ID, Lim: page.Limit, Off: page.Offset})
	if err != nil {
		return nil, err
	}
	return s.postViews(ctx, s.q, p, f, posts, nil)
}

func (s *Service) GetPost(ctx context.Context, p *Principal, postID uuid.UUID) (PostView, error) {
	f, _, post, err := s.postScope(ctx, s.q, p, postID)
	if err != nil {
		return PostView{}, err
	}
	return s.postView(ctx, s.q, p, f, post)
}

type ReplyInput struct {
	Content  string
	ParentID *uuid.UUID
	// AttachmentIDs are files from UploadAttachment, in display order.
	AttachmentIDs []uuid.UUID
}

// CreatePost replies to a topic. ParentID marks a direct reply to another post; threaded
// boards nest replies under it.
func (s *Service) CreatePost(ctx context.Context, p *Principal, topicID uuid.UUID, in ReplyInput) (PostView, error) {
	if err := validateContent(in.Content, len(in.AttachmentIDs)); err != nil {
		return PostView{}, err
	}
	var view PostView
	err := s.tx(ctx, func(q *store.Queries) error {
		f, topic, err := s.topicScope(ctx, q, p, topicID)
		if err != nil {
			return err
		}
		if topic, err = q.GetTopicForUpdate(ctx, topic.ID); err != nil {
			return err
		}
		if err := f.requireParticipant(topic.BoardID, permissions.ReplyToTopics); err != nil {
			return err
		}
		if topic.IsArchived {
			return apperr.Conflict("this topic is archived")
		}
		if topic.IsLocked && !f.canModerate(topic.BoardID) {
			return apperr.Forbidden("this topic is locked")
		}
		var parent *store.Post
		if in.ParentID != nil {
			pp, err := q.GetPost(ctx, *in.ParentID)
			if err != nil || pp.TopicID != topic.ID || pp.DeletedAt != nil {
				return apperr.Invalid("parent post not found in this topic")
			}
			parent = &pp
		}
		files, err := s.claimAttachments(ctx, q, p, in.AttachmentIDs)
		if err != nil {
			return err
		}
		post, err := s.insertPost(ctx, q, topic, p.User.ID, in.ParentID, in.Content, files)
		if err != nil {
			return err
		}

		rs := newRecipients(p.User.ID)
		mentioned, err := s.mentionedUserIDs(ctx, q, in.Content, nil)
		if err != nil {
			return err
		}
		rs.add(NotifyMention, mentioned...)
		if parent != nil && parent.AuthorID != nil {
			rs.add(NotifyReply, *parent.AuthorID)
		} else if topic.AuthorID != nil {
			rs.add(NotifyReply, *topic.AuthorID)
		}
		watchers, err := q.ListWatchers(ctx, []uuid.UUID{topic.ID})
		if err != nil {
			return err
		}
		rs.add(NotifyTopicReply, watchers...)
		if err := s.deliver(ctx, q, forumEvent{f: f, topic: topic, post: &post, actorID: p.User.ID}, rs); err != nil {
			return err
		}
		if err := s.queueForumWebhook(ctx, q, f, WebhookPostCreate, topic, post); err != nil {
			return err
		}
		view, err = s.postView(ctx, q, p, f, post)
		return err
	})
	return view, err
}

// EditPost replaces a post's content, keeping the previous version as a revision. Authors
// may edit their posts in open topics; MANAGE_POSTS may edit any post.
func (s *Service) EditPost(ctx context.Context, p *Principal, postID uuid.UUID, content string) (PostView, error) {
	if err := validateContent(content, 1); err != nil {
		return PostView{}, err
	}
	var view PostView
	err := s.tx(ctx, func(q *store.Queries) error {
		f, topic, post, err := s.postScope(ctx, q, p, postID)
		if err != nil {
			return err
		}
		if post.DeletedAt != nil {
			return apperr.Conflict("deleted posts cannot be edited")
		}
		if strings.TrimSpace(content) == "" {
			files, err := q.ListPostAttachments(ctx, []uuid.UUID{post.ID})
			if err != nil {
				return err
			}
			if err := validateContent(content, len(files)); err != nil {
				return err
			}
		}
		mod := f.canModerate(post.BoardID)
		author := isAuthor(post.AuthorID, p)
		if !mod {
			if !author {
				return apperr.Forbidden("you can only edit your own posts")
			}
			if !f.acc.IsMember {
				return apperr.Forbidden("join this place to participate")
			}
			if f.viewer.TimedOut {
				return apperr.Forbidden("you are timed out in this place")
			}
			if topic.IsLocked || topic.IsArchived {
				return apperr.Forbidden("this topic is locked")
			}
		}
		if content == post.Content {
			view, err = s.postView(ctx, q, p, f, post)
			return err
		}
		revID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if err := q.CreatePostRevision(ctx, store.CreatePostRevisionParams{
			ID: revID, PostID: post.ID, EditorID: &p.User.ID, Content: post.Content,
		}); err != nil {
			return err
		}
		updated, err := q.UpdatePostContent(ctx, store.UpdatePostContentParams{ID: post.ID, Content: content})
		if err != nil {
			return err
		}
		s.resolveLinks(ctx, q, updated.Content, func(context.Context) {})
		if err := s.indexPost(ctx, q, topic, updated); err != nil {
			return err
		}
		// Only newly added mentions notify.
		added, err := s.mentionedUserIDs(ctx, q, content, mentions(post.Content))
		if err != nil {
			return err
		}
		rs := newRecipients(p.User.ID)
		rs.add(NotifyMention, added...)
		if err := s.deliver(ctx, q, forumEvent{f: f, topic: topic, post: &updated, actorID: p.User.ID}, rs); err != nil {
			return err
		}
		if !author {
			if err := s.audit(ctx, q, f.place.ID, p, "post.edit", "post", &post.ID, "",
				map[string]any{"topic_id": topic.ID, "author_id": post.AuthorID}); err != nil {
				return err
			}
		}
		view, err = s.postView(ctx, q, p, f, updated)
		return err
	})
	return view, err
}

// DeletePost leaves a tombstone so replies keep their context. The opening post can only
// be removed by deleting the topic.
func (s *Service) DeletePost(ctx context.Context, p *Principal, postID uuid.UUID, reason string) error {
	if len(reason) > 512 {
		return apperr.Invalid("reason must be at most 512 characters")
	}
	return s.tx(ctx, func(q *store.Queries) error {
		f, topic, post, err := s.postScope(ctx, q, p, postID)
		if err != nil {
			return err
		}
		if topic, err = q.GetTopicForUpdate(ctx, topic.ID); err != nil {
			return err
		}
		if post.DeletedAt != nil {
			return apperr.NotFound("post not found")
		}
		if post.PostNumber == 1 {
			return apperr.Invalid("the opening post cannot be deleted on its own; delete the topic instead")
		}
		author := isAuthor(post.AuthorID, p)
		if !author && !f.canModerate(post.BoardID) {
			return missing(permissions.ManagePosts)
		}
		if _, err := q.SoftDeletePost(ctx, store.SoftDeletePostParams{ID: post.ID, DeletedBy: &p.User.ID}); err != nil {
			return err
		}
		if err := q.AdjustTopicPostCount(ctx, store.AdjustTopicPostCountParams{ID: topic.ID, Delta: -1}); err != nil {
			return err
		}
		if err := s.refreshRanks(ctx, q, topic.ID); err != nil {
			return err
		}
		if err := q.AdjustBoardCounts(ctx, store.AdjustBoardCountsParams{ID: post.BoardID, Posts: -1}); err != nil {
			return err
		}
		if err := q.DeleteSearchDocument(ctx, post.ID); err != nil {
			return err
		}
		if topic.SolutionPostID != nil && *topic.SolutionPostID == post.ID {
			if _, err := q.SetTopicSolution(ctx, store.SetTopicSolutionParams{ID: topic.ID}); err != nil {
				return err
			}
		}
		if author {
			return nil
		}
		if err := s.audit(ctx, q, f.place.ID, p, "post.delete", "post", &post.ID, reason,
			map[string]any{"topic_id": topic.ID, "author_id": post.AuthorID}); err != nil {
			return err
		}
		if post.AuthorID == nil {
			return nil
		}
		return s.notifyModeration(ctx, q, f.place, *post.AuthorID, p, "post.delete", reason,
			map[string]any{"topic_id": topic.ID, "topic_title": topic.Title, "post_number": post.PostNumber})
	})
}

type RevisionView struct {
	Revision store.PostRevision
	Editor   *store.User
}

// ListRevisions returns a post's previous versions, newest first.
func (s *Service) ListRevisions(ctx context.Context, p *Principal, postID uuid.UUID) ([]RevisionView, error) {
	f, _, post, err := s.postScope(ctx, s.q, p, postID)
	if err != nil {
		return nil, err
	}
	if post.DeletedAt != nil && !f.canModerate(post.BoardID) {
		return nil, apperr.NotFound("post not found")
	}
	revs, err := s.q.ListPostRevisions(ctx, post.ID)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, r := range revs {
		if r.EditorID != nil {
			ids = append(ids, *r.EditorID)
		}
	}
	users, err := s.usersByID(ctx, s.q, ids)
	if err != nil {
		return nil, err
	}
	out := make([]RevisionView, len(revs))
	for i, r := range revs {
		out[i] = RevisionView{Revision: r, Editor: ptrUser(users, r.EditorID)}
	}
	return out, nil
}

// SetSolution marks (or with a nil postID, clears) the accepted answer of a topic in a
// board with solutions enabled. The topic author or MANAGE_POSTS may do this.
func (s *Service) SetSolution(ctx context.Context, p *Principal, topicID uuid.UUID, postID *uuid.UUID) (TopicView, error) {
	var view TopicView
	err := s.tx(ctx, func(q *store.Queries) error {
		f, topic, err := s.topicScope(ctx, q, p, topicID)
		if err != nil {
			return err
		}
		if topic, err = q.GetTopicForUpdate(ctx, topic.ID); err != nil {
			return err
		}
		if !f.boards[topic.BoardID].SolutionsEnabled {
			return apperr.Invalid("this board does not use solutions")
		}
		if !f.canModerate(topic.BoardID) {
			if !isAuthor(topic.AuthorID, p) {
				return apperr.Forbidden("only the topic author or a moderator can choose the solution")
			}
			if err := f.requireParticipant(topic.BoardID, permissions.ViewBoards); err != nil {
				return err
			}
		}
		var post store.Post
		if postID != nil {
			post, err = q.GetPost(ctx, *postID)
			if err != nil || post.TopicID != topic.ID || post.DeletedAt != nil || post.PostNumber == 1 {
				return apperr.Invalid("the solution must be a reply in this topic")
			}
		}
		updated, err := q.SetTopicSolution(ctx, store.SetTopicSolutionParams{ID: topic.ID, PostID: postID})
		if err != nil {
			return err
		}
		if postID != nil && post.AuthorID != nil {
			rs := newRecipients(p.User.ID)
			rs.add(NotifySolution, *post.AuthorID)
			if err := s.deliver(ctx, q, forumEvent{f: f, topic: updated, post: &post, actorID: p.User.ID}, rs); err != nil {
				return err
			}
		}
		view, err = s.topicView(ctx, q, p, updated)
		return err
	})
	return view, err
}

// MarkRead records that the caller opened a topic and, when postNumber is given, has read
// it up to that post. Read positions never move backwards.
func (s *Service) MarkRead(ctx context.Context, p *Principal, topicID uuid.UUID, postNumber *int32) (TopicReadState, error) {
	_, topic, err := s.topicScope(ctx, s.q, p, topicID)
	if err != nil {
		return TopicReadState{}, err
	}
	n := int32(1)
	if postNumber != nil {
		n = *postNumber
	}
	n = min(max(n, 1), topic.LastPostNumber)
	row, err := s.q.MarkTopicRead(ctx, store.MarkTopicReadParams{
		UserID: p.User.ID, TopicID: topic.ID, PostNumber: n, SeenPostNumber: topic.LastPostNumber,
	})
	if err != nil {
		return TopicReadState{}, err
	}
	state := readStateOf(topic, &row)
	s.emitReadStates(ctx, s.q, p.User.ID, []TopicReadState{state})
	return state, nil
}

// AddReaction reacts to a post. Adding the same reaction twice is a no-op.
func (s *Service) AddReaction(ctx context.Context, p *Principal, postID uuid.UUID, emoji string) error {
	if err := validateEmoji(emoji); err != nil {
		return err
	}
	return s.tx(ctx, func(q *store.Queries) error {
		f, topic, post, err := s.postScope(ctx, q, p, postID)
		if err != nil {
			return err
		}
		if post.DeletedAt != nil {
			return apperr.NotFound("post not found")
		}
		if err := f.requireParticipant(post.BoardID, permissions.AddReactions); err != nil {
			return err
		}
		if topic.IsArchived {
			return apperr.Conflict("this topic is archived")
		}
		exists, err := q.PostHasEmoji(ctx, store.PostHasEmojiParams{PostID: post.ID, Emoji: emoji})
		if err != nil {
			return err
		}
		if !exists {
			n, err := q.CountDistinctReactions(ctx, post.ID)
			if err != nil {
				return err
			}
			if n >= maxReactionsPerPos {
				return apperr.Conflict("a post can have at most %d different reactions", maxReactionsPerPos)
			}
		}
		added, err := q.AddReaction(ctx, store.AddReactionParams{PostID: post.ID, UserID: p.User.ID, Emoji: emoji})
		if err != nil || added == 0 {
			return err
		}
		if err := q.AdjustReactionCount(ctx, store.AdjustReactionCountParams{ID: post.ID, Delta: 1}); err != nil {
			return err
		}
		if err := s.refreshOpeningPostRank(ctx, q, post); err != nil {
			return err
		}
		if post.AuthorID == nil {
			return nil
		}
		rs := newRecipients(p.User.ID)
		rs.add(NotifyReaction, *post.AuthorID)
		return s.deliver(ctx, q, forumEvent{f: f, topic: topic, post: &post, actorID: p.User.ID,
			extra: map[string]any{"emoji": emoji}}, rs)
	})
}

func (s *Service) RemoveReaction(ctx context.Context, p *Principal, postID uuid.UUID, emoji string) error {
	return s.tx(ctx, func(q *store.Queries) error {
		_, _, post, err := s.postScope(ctx, q, p, postID)
		if err != nil {
			return err
		}
		n, err := q.RemoveReaction(ctx, store.RemoveReactionParams{PostID: post.ID, UserID: p.User.ID, Emoji: emoji})
		if err != nil || n == 0 {
			return err
		}
		if err := q.AdjustReactionCount(ctx, store.AdjustReactionCountParams{ID: post.ID, Delta: -1}); err != nil {
			return err
		}
		return s.refreshOpeningPostRank(ctx, q, post)
	})
}

// ListReactionUsers lists who reacted to a post with an emoji, earliest first.
func (s *Service) ListReactionUsers(ctx context.Context, p *Principal, postID uuid.UUID, emoji string, page Pagination) ([]store.User, error) {
	_, _, post, err := s.postScope(ctx, s.q, p, postID)
	if err != nil {
		return nil, err
	}
	if post.DeletedAt != nil {
		return nil, apperr.NotFound("post not found")
	}
	page = page.normalized()
	rows, err := s.q.ListReactionUsers(ctx, store.ListReactionUsersParams{PostID: post.ID, Emoji: emoji, Lim: page.Limit, Off: page.Offset})
	if err != nil {
		return nil, err
	}
	users := make([]store.User, len(rows))
	for i, r := range rows {
		users[i] = r.User
	}
	return users, nil
}
