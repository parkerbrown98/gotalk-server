package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

func idString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

func userPtr(u *store.User) *User {
	if u == nil {
		return nil
	}
	out := toUser(*u)
	return &out
}

type Board struct {
	ID               string     `json:"id" format:"uuid"`
	PlaceID          string     `json:"place_id" format:"uuid"`
	ParentID         *string    `json:"parent_id" format:"uuid"`
	Kind             string     `json:"kind" enum:"category,board" doc:"Categories group boards; only boards hold topics"`
	Slug             string     `json:"slug"`
	Name             string     `json:"name"`
	Description      string     `json:"description"`
	Position         int32      `json:"position" doc:"Order among siblings, ascending"`
	ReplyMode        string     `json:"reply_mode" enum:"flat,threaded"`
	SolutionsEnabled bool       `json:"solutions_enabled" doc:"Q&A board: topics can mark an accepted answer"`
	IsNSFW           bool       `json:"is_nsfw"`
	IsPublic         bool       `json:"is_public" doc:"Readable by signed-out visitors and included in instance-wide search"`
	TopicCount       int32      `json:"topic_count"`
	PostCount        int32      `json:"post_count"`
	LastPostAt       *time.Time `json:"last_post_at"`
	CreatedAt        time.Time  `json:"created_at"`
	MyPermissions    int64      `json:"my_permissions" doc:"Caller's effective permission bits within this board"`
	Subscription     string     `json:"subscription" enum:"watching,normal,muted"`
}

func toBoard(v service.BoardView) Board {
	b := v.Board
	return Board{
		ID: b.ID.String(), PlaceID: b.PlaceID.String(), ParentID: idString(b.ParentID), Kind: b.Kind,
		Slug: b.Slug, Name: b.Name, Description: b.Description, Position: b.Position, ReplyMode: b.ReplyMode,
		SolutionsEnabled: b.SolutionsEnabled, IsNSFW: b.IsNsfw, IsPublic: b.IsPublic,
		TopicCount: b.TopicCount, PostCount: b.PostCount, LastPostAt: b.LastPostAt, CreatedAt: b.CreatedAt,
		MyPermissions: int64(v.Permissions), Subscription: v.Subscription,
	}
}

type Overwrite struct {
	RoleID     string   `json:"role_id" format:"uuid"`
	Allow      int64    `json:"allow"`
	Deny       int64    `json:"deny"`
	AllowNames []string `json:"allow_names"`
	DenyNames  []string `json:"deny_names"`
}

func toOverwrite(o permissions.Overwrite) Overwrite {
	return Overwrite{
		RoleID: o.RoleID.String(), Allow: int64(o.Allow), Deny: int64(o.Deny),
		AllowNames: permissions.Names(o.Allow), DenyNames: permissions.Names(o.Deny),
	}
}

type Topic struct {
	ID             string    `json:"id" format:"uuid"`
	PlaceID        string    `json:"place_id" format:"uuid"`
	BoardID        string    `json:"board_id" format:"uuid"`
	Author         *User     `json:"author" doc:"null if the author deleted their account"`
	Title          string    `json:"title"`
	Slug           string    `json:"slug" doc:"URL-friendly title; canonical URLs combine it with the ID"`
	Tags           []string  `json:"tags"`
	IsPinned       bool      `json:"is_pinned"`
	IsLocked       bool      `json:"is_locked"`
	IsArchived     bool      `json:"is_archived"`
	SolutionPostID *string   `json:"solution_post_id" format:"uuid"`
	PostCount      int32     `json:"post_count" doc:"Live posts, including the opening post"`
	ReplyCount     int32     `json:"reply_count"`
	LastPostNumber int32     `json:"last_post_number"`
	LastPostAt     time.Time `json:"last_post_at"`
	LastPoster     *User     `json:"last_poster"`
	CreatedAt      time.Time `json:"created_at"`
	Score          int32     `json:"score" doc:"Up minus down votes, or reactions on the opening post when the place has voting off"`
	Upvotes        int32     `json:"upvotes"`
	Downvotes      int32     `json:"downvotes"`
	// Caller state, present only for authenticated requests.
	LastReadPostNumber *int32       `json:"last_read_post_number,omitempty"`
	UnreadCount        *int32       `json:"unread_count,omitempty" doc:"Posts after the caller's read position"`
	Subscription       *string      `json:"subscription,omitempty" enum:"watching,normal,muted"`
	Viewer             *TopicViewer `json:"viewer,omitempty" doc:"The caller's state; present only for authenticated requests"`
}

type TopicViewer struct {
	Read               bool   `json:"read" doc:"The caller has opened this topic and not marked it unread since"`
	HasNewReplies      bool   `json:"has_new_replies" doc:"Posts arrived after the caller last opened the topic"`
	NewReplyCount      int32  `json:"new_reply_count" doc:"How many posts arrived after the caller last opened the topic"`
	UnreadCount        int32  `json:"unread_count" doc:"Posts after the caller's read position"`
	LastReadPostNumber *int32 `json:"last_read_post_number" doc:"How far the caller has read; null if never"`
	Vote               int16  `json:"vote" doc:"The caller's vote: 1, -1, or 0 for none"`
	Subscription       string `json:"subscription" enum:"watching,normal,muted"`
}

func toTopic(v service.TopicView) Topic {
	t := v.Topic
	out := Topic{
		ID: t.ID.String(), PlaceID: t.PlaceID.String(), BoardID: t.BoardID.String(), Author: userPtr(v.Author),
		Title: t.Title, Slug: t.Slug, Tags: t.Tags, IsPinned: t.IsPinned, IsLocked: t.IsLocked,
		IsArchived: t.IsArchived, SolutionPostID: idString(t.SolutionPostID), PostCount: t.PostCount,
		ReplyCount: max(t.PostCount-1, 0), LastPostNumber: t.LastPostNumber, LastPostAt: t.LastPostAt,
		LastPoster: userPtr(v.LastPoster), CreatedAt: t.CreatedAt,
		Score: t.Score, Upvotes: t.Upvotes, Downvotes: t.Downvotes,
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if v.Subscription != "" {
		sub := v.Subscription
		out.Subscription = &sub
		var read int32
		if v.LastReadPostNumber != nil {
			read = *v.LastReadPostNumber
			out.LastReadPostNumber = v.LastReadPostNumber
		}
		unread := max(t.LastPostNumber-read, 0)
		out.UnreadCount = &unread
		out.Viewer = &TopicViewer{
			Read: v.Opened, HasNewReplies: v.HasNewReplies(), NewReplyCount: v.NewReplyCount(), UnreadCount: unread,
			LastReadPostNumber: v.LastReadPostNumber, Vote: v.Vote, Subscription: sub,
		}
	}
	return out
}

type Reaction struct {
	Emoji string `json:"emoji"`
	Count int32  `json:"count"`
	Me    bool   `json:"me" doc:"Whether the caller added this reaction"`
}

type Post struct {
	ID            string     `json:"id" format:"uuid"`
	TopicID       string     `json:"topic_id" format:"uuid"`
	PlaceID       string     `json:"place_id" format:"uuid"`
	BoardID       string     `json:"board_id" format:"uuid"`
	Author        *User      `json:"author" doc:"null if the author deleted their account"`
	ParentID      *string    `json:"parent_id" format:"uuid" doc:"The post this replies to, if any"`
	PostNumber    int32      `json:"post_number" doc:"1 is the opening post; numbers are stable and never reused"`
	Content       string     `json:"content" doc:"Markdown. Empty for deleted posts unless the caller can manage posts"`
	Deleted       bool       `json:"deleted"`
	Depth         *int32     `json:"depth,omitempty" doc:"Nesting depth; present when listing a threaded board"`
	Reactions     []Reaction `json:"reactions"`
	ReactionCount int32      `json:"reaction_count"`
	EditCount     int32      `json:"edit_count"`
	EditedAt      *time.Time `json:"edited_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

func toPost(v service.PostView) Post {
	p := v.Post
	reactions := make([]Reaction, len(v.Reactions))
	for i, r := range v.Reactions {
		reactions[i] = Reaction{Emoji: r.Emoji, Count: r.Count, Me: r.Me}
	}
	return Post{
		ID: p.ID.String(), TopicID: p.TopicID.String(), PlaceID: p.PlaceID.String(), BoardID: p.BoardID.String(),
		Author: userPtr(v.Author), ParentID: idString(p.ParentID), PostNumber: p.PostNumber, Content: p.Content,
		Deleted: v.Deleted, Depth: v.Depth, Reactions: reactions, ReactionCount: p.ReactionCount,
		EditCount: p.EditCount, EditedAt: p.EditedAt, CreatedAt: p.CreatedAt,
	}
}

type TopicWithPost struct {
	Topic Topic `json:"topic"`
	Post  Post  `json:"post"`
}

type Revision struct {
	ID        string    `json:"id" format:"uuid"`
	Editor    *User     `json:"editor"`
	Content   string    `json:"content" doc:"The content before this edit"`
	CreatedAt time.Time `json:"created_at"`
}

func toRevision(r service.RevisionView) Revision {
	return Revision{ID: r.Revision.ID.String(), Editor: userPtr(r.Editor), Content: r.Revision.Content, CreatedAt: r.Revision.CreatedAt}
}

type Tag struct {
	Tag        string `json:"tag"`
	TopicCount int64  `json:"topic_count"`
}

func toTag(t service.TagCount) Tag { return Tag(t) }

type TopicRef struct {
	ID     string   `json:"id" format:"uuid"`
	Title  string   `json:"title"`
	Slug   string   `json:"slug"`
	Tags   []string `json:"tags"`
	Solved bool     `json:"solved"`
}

type PlaceRef struct {
	ID   string `json:"id" format:"uuid"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type SearchResult struct {
	PostID     string    `json:"post_id" format:"uuid"`
	PostNumber int32     `json:"post_number"`
	BoardID    string    `json:"board_id" format:"uuid"`
	Author     *User     `json:"author"`
	Topic      TopicRef  `json:"topic"`
	Place      PlaceRef  `json:"place"`
	Snippet    string    `json:"snippet" doc:"Excerpt with matches wrapped in Markdown bold (**)"`
	Rank       float64   `json:"rank"`
	CreatedAt  time.Time `json:"created_at"`
}

func toSearchResult(h service.SearchHit) SearchResult {
	tags := h.Topic.Tags
	if tags == nil {
		tags = []string{}
	}
	return SearchResult{
		PostID: h.Post.ID.String(), PostNumber: h.Post.PostNumber, BoardID: h.Post.BoardID.String(),
		Author: userPtr(h.Author), Snippet: h.Snippet, Rank: h.Rank, CreatedAt: h.Post.CreatedAt,
		Topic: TopicRef{ID: h.Topic.ID.String(), Title: h.Topic.Title, Slug: h.Topic.Slug, Tags: tags,
			Solved: h.Topic.SolutionPostID != nil},
		Place: PlaceRef{ID: h.Place.ID.String(), Slug: h.Place.Slug, Name: h.Place.Name},
	}
}

type Notification struct {
	ID        string         `json:"id" format:"uuid"`
	Kind      string         `json:"kind" enum:"mention,reply,topic_reply,new_topic,reaction,solution,moderation,direct_message"`
	PlaceID   *string        `json:"place_id" format:"uuid"`
	TopicID   *string        `json:"topic_id" format:"uuid"`
	PostID    *string        `json:"post_id" format:"uuid"`
	ChannelID *string        `json:"channel_id" format:"uuid" doc:"Set for chat mentions, replies and direct messages"`
	MessageID *string        `json:"message_id" format:"uuid"`
	Actor     *User          `json:"actor"`
	Data      map[string]any `json:"data" doc:"Snapshot for rendering: place_name, place_slug, topic_title, channel_name, excerpt, emoji, action, reason, ..."`
	Read      bool           `json:"read"`
	CreatedAt time.Time      `json:"created_at"`
}

func toNotification(v service.NotificationView) Notification {
	n := v.Notification
	data := map[string]any{}
	_ = json.Unmarshal(n.Data, &data)
	return Notification{
		ID: n.ID.String(), Kind: n.Kind, PlaceID: idString(n.PlaceID), TopicID: idString(n.TopicID),
		PostID: idString(n.PostID), ChannelID: idString(n.ChannelID), MessageID: idString(n.MessageID), Actor: userPtr(v.Actor), Data: data, Read: n.ReadAt != nil, CreatedAt: n.CreatedAt,
	}
}

type Draft struct {
	Key       string         `json:"key"`
	Data      map[string]any `json:"data"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

func toDraft(d store.Draft) Draft {
	data := map[string]any{}
	_ = json.Unmarshal(d.Data, &data)
	return Draft{Key: d.Key, Data: data, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
}

type Report struct {
	ID              string     `json:"id" format:"uuid"`
	PlaceID         string     `json:"place_id" format:"uuid"`
	Reporter        *User      `json:"reporter"`
	TargetType      string     `json:"target_type" enum:"post,user,message"`
	PostID          *string    `json:"post_id" format:"uuid"`
	TopicID         *string    `json:"topic_id" format:"uuid"`
	MessageID       *string    `json:"message_id" format:"uuid" doc:"null once the message is deleted; content_snapshot keeps it"`
	ChannelID       *string    `json:"channel_id" format:"uuid"`
	TargetUser      *User      `json:"target_user" doc:"The reported user, or the reported post's author"`
	Reason          string     `json:"reason" enum:"spam,harassment,inappropriate,off_topic,other"`
	Details         string     `json:"details"`
	ContentSnapshot string     `json:"content_snapshot" doc:"The post or message as it was when reported"`
	Status          string     `json:"status" enum:"open,resolved,dismissed"`
	ResolvedBy      *string    `json:"resolved_by" format:"uuid"`
	ResolvedAt      *time.Time `json:"resolved_at"`
	ResolutionNote  string     `json:"resolution_note"`
	CreatedAt       time.Time  `json:"created_at"`
}

func toReport(v service.ReportView) Report {
	r := v.Report
	return Report{
		ID: r.ID.String(), PlaceID: r.PlaceID.String(), Reporter: userPtr(v.Reporter), TargetType: r.TargetType,
		PostID: idString(r.PostID), TopicID: idString(v.TopicID), MessageID: idString(r.MessageID),
		ChannelID: idString(r.ChannelID), TargetUser: userPtr(v.TargetUser),
		Reason: r.Reason, Details: r.Details, ContentSnapshot: r.ContentSnapshot, Status: r.Status,
		ResolvedBy: idString(r.ResolvedBy), ResolvedAt: r.ResolvedAt, ResolutionNote: r.ResolutionNote,
		CreatedAt: r.CreatedAt,
	}
}

// reportForReporter hides moderator-only fields from the person who filed the report.
func reportForReporter(r store.Report) Report {
	return Report{
		ID: r.ID.String(), PlaceID: r.PlaceID.String(), TargetType: r.TargetType, PostID: idString(r.PostID),
		MessageID: idString(r.MessageID), ChannelID: idString(r.ChannelID), Reason: r.Reason, Details: r.Details, Status: r.Status, CreatedAt: r.CreatedAt,
	}
}

type AuditEntry struct {
	ID         string         `json:"id" format:"uuid"`
	Actor      *User          `json:"actor"`
	ActorID    *string        `json:"actor_id" format:"uuid"`
	Action     string         `json:"action" example:"member.ban"`
	TargetType string         `json:"target_type"`
	TargetID   *string        `json:"target_id" format:"uuid"`
	Reason     string         `json:"reason"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  time.Time      `json:"created_at"`
}

func toAuditEntry(v service.AuditView) AuditEntry {
	e := v.Entry
	meta := map[string]any{}
	_ = json.Unmarshal(e.Metadata, &meta)
	return AuditEntry{
		ID: e.ID.String(), Actor: userPtr(v.Actor), ActorID: idString(e.ActorID), Action: e.Action,
		TargetType: e.TargetType, TargetID: idString(e.TargetID), Reason: e.Reason, Metadata: meta,
		CreatedAt: e.CreatedAt,
	}
}
