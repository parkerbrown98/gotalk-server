package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const tagFeeds = "Feeds"

type FeedBoard struct {
	ID     string `json:"id" format:"uuid"`
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	IsNSFW bool   `json:"is_nsfw"`
}

type FeedPlace struct {
	ID            string  `json:"id" format:"uuid"`
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	IconURL       *string `json:"icon_url"`
	IsNSFW        bool    `json:"is_nsfw"`
	VotingEnabled bool    `json:"voting_enabled"`
}

// FeedItem is a topic plus what a feed card needs to show it.
type FeedItem struct {
	Topic
	Excerpt string    `json:"excerpt" doc:"Plain text from the opening post (no Markdown or HTML), at most 280 characters"`
	Board   FeedBoard `json:"board"`
	Place   FeedPlace `json:"place"`
	IsNSFW  bool      `json:"is_nsfw" doc:"The board, one of its parents, or the place is marked NSFW"`
}

type FeedPage struct {
	Items      []FeedItem `json:"items"`
	Pinned     []FeedItem `json:"pinned,omitempty" doc:"With pinned=first, the place's pinned topics (first page only)"`
	NextCursor *string    `json:"next_cursor,omitempty" doc:"Pass as cursor to get the next page; absent on the last page"`
	AsOf       time.Time  `json:"as_of" doc:"When the first page was loaded; pass as before to mark the feed read"`
	Sort       string     `json:"sort"`
	Window     string     `json:"t,omitempty" doc:"Time window, for top and controversial"`
}

func toFeedItem(it service.FeedItem) FeedItem {
	return FeedItem{
		Topic:   toTopic(it.Topic),
		Excerpt: it.Excerpt,
		Board:   FeedBoard{ID: it.Board.ID.String(), Slug: it.Board.Slug, Name: it.Board.Name, IsNSFW: it.Board.IsNsfw},
		Place: FeedPlace{
			ID: it.Place.ID.String(), Slug: it.Place.Slug, Name: it.Place.Name, IconURL: it.Place.IconUrl,
			IsNSFW: it.Place.IsNsfw, VotingEnabled: it.Place.VotingEnabled,
		},
		IsNSFW: it.IsNSFW,
	}
}

func toFeedPage(p service.FeedPage) FeedPage {
	out := FeedPage{Items: mapSlice(p.Items, toFeedItem), AsOf: p.AsOf, Sort: p.Sort, Window: p.Window}
	if p.Pinned != nil {
		out.Pinned = mapSlice(p.Pinned, toFeedItem)
	}
	if p.NextCursor != "" {
		out.NextCursor = &p.NextCursor
	}
	return out
}

type TopicReadState struct {
	TopicID            string `json:"topic_id" format:"uuid"`
	PlaceID            string `json:"place_id" format:"uuid"`
	Read               bool   `json:"read"`
	HasNewReplies      bool   `json:"has_new_replies"`
	LastReadPostNumber *int32 `json:"last_read_post_number"`
	UnreadCount        int32  `json:"unread_count"`
}

func toTopicReadState(s service.TopicReadState) TopicReadState {
	return TopicReadState{
		TopicID: s.TopicID.String(), PlaceID: s.PlaceID.String(), Read: s.Read, HasNewReplies: s.HasNewReplies,
		LastReadPostNumber: s.LastReadPostNumber, UnreadCount: s.UnreadCount,
	}
}

type FeedQuery struct {
	Sort     string `query:"sort" enum:"hot,new,active,top,rising,controversial" default:"hot" doc:"hot weighs score and replies against age; active is the latest reply first; rising is the fastest-growing topics of the last 48 hours; controversial has many votes split between up and down"`
	Window   string `query:"t" enum:"hour,day,week,month,year,all" doc:"Time window for top and controversial (default week)"`
	Tag      string `query:"tag" maxLength:"32"`
	Solved   string `query:"solved" enum:"true,false" doc:"Only topics with (true) or without (false) an accepted answer"`
	HideRead bool   `query:"hide_read" doc:"Leave out topics the caller opened that have no new posts since"`
	NSFW     bool   `query:"nsfw" doc:"Include NSFW boards (and, on instance feeds, NSFW places)"`
	Archived bool   `query:"include_archived" doc:"Include archived topics"`
	Limit    int32  `query:"limit" minimum:"1" maximum:"100" default:"25"`
	Cursor   string `query:"cursor" maxLength:"512" doc:"next_cursor from the previous page; the other parameters must stay the same"`
}

func (q FeedQuery) query() service.FeedQuery {
	return service.FeedQuery{
		Sort: q.Sort, Window: q.Window, Tag: q.Tag, Solved: parseOptionalBool(q.Solved), HideRead: q.HideRead,
		NSFW: q.NSFW, IncludeArchived: q.Archived, Limit: q.Limit, Cursor: q.Cursor,
	}
}

type PlaceFeedInput struct {
	PlacePath
	FeedQuery
	Board  string `query:"board" format:"uuid" doc:"Only this board or category and the boards under it"`
	Pinned string `query:"pinned" enum:"inline,first" default:"inline" doc:"first returns pinned topics separately on the first page"`
}

type InstanceFeedInput struct {
	FeedQuery
	Scope string `query:"scope" enum:"home,all" doc:"home: places the caller joined (default when signed in); all: public topics on the instance (default otherwise)"`
}

type VoteRequest struct {
	Value int16 `json:"value" enum:"1,-1" doc:"1 to vote up, -1 to vote down"`
}

type MarkTopicsReadRequest struct {
	TopicIDs []string `json:"topic_ids" minItems:"1" maxItems:"100" doc:"Topics the caller opened; unknown or hidden topics are skipped"`
}

type MarkFeedReadRequest struct {
	BoardID string `json:"board_id,omitempty" format:"uuid" doc:"Only this board or category and the boards under it"`
	Before  string `json:"before,omitempty" format:"date-time" doc:"Skip topics with activity after this time, usually the feed's as_of (default now)"`
}

type MarkFeedReadResult struct {
	Marked int64 `json:"marked" doc:"Topics marked read"`
}

func (s *Server) registerFeeds() {
	huma.Register(s.api, withOptionalAuth(operation("place-feed", http.MethodGet, "/places/{place}/feed",
		"Topics from every board of a place the caller can read, ranked by sort", tagFeeds)),
		handle(s, func(ctx context.Context, in *PlaceFeedInput) (*Body[FeedPage], error) {
			q := in.query()
			var err error
			if q.BoardID, err = parseOptionalID("board", in.Board); err != nil {
				return nil, err
			}
			q.PinnedFirst = in.Pinned == "first"
			page, err := s.Service.PlaceFeed(ctx, principalFrom(ctx), in.Place, q)
			if err != nil {
				return nil, err
			}
			return ok(toFeedPage(page))
		}))

	huma.Register(s.api, withOptionalAuth(operation("instance-feed", http.MethodGet, "/feed",
		"Topics across the caller's places (home) or public topics on the whole instance (all), ranked by sort", tagFeeds)),
		handle(s, func(ctx context.Context, in *InstanceFeedInput) (*Body[FeedPage], error) {
			page, err := s.Service.InstanceFeed(ctx, principalFrom(ctx), in.Scope, in.query())
			if err != nil {
				return nil, err
			}
			return ok(toFeedPage(page))
		}))

	huma.Register(s.api, withContentRateLimit(withStatus(withAuth(operation("mark-feed-topics-read", http.MethodPost, "/feed/read",
		"Record opens of several topics at once, e.g. replayed after being offline", tagFeeds)), http.StatusOK)),
		handle(s, func(ctx context.Context, in *struct {
			Body MarkTopicsReadRequest
		}) (*Body[[]TopicReadState], error) {
			ids := make([]uuid.UUID, len(in.Body.TopicIDs))
			for i, raw := range in.Body.TopicIDs {
				id, err := parseID("topic_ids", raw)
				if err != nil {
					return nil, err
				}
				ids[i] = id
			}
			states, err := s.Service.MarkTopicsRead(ctx, mustPrincipal(ctx), ids)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(states, toTopicReadState))
		}))

	huma.Register(s.api, withContentRateLimit(withStatus(withAuth(operation("mark-place-feed-read", http.MethodPost, "/places/{place}/feed/read",
		"Mark every topic in a place (or a board) read, up to before; at most the 5000 most recently active", tagFeeds)), http.StatusOK)),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body *MarkFeedReadRequest `required:"false"`
		}) (*Body[MarkFeedReadResult], error) {
			var (
				board  *uuid.UUID
				before *time.Time
				err    error
			)
			if in.Body != nil {
				if board, err = parseOptionalID("board_id", in.Body.BoardID); err != nil {
					return nil, err
				}
				if before, err = parseOptionalTime("before", in.Body.Before); err != nil {
					return nil, err
				}
			}
			n, err := s.Service.MarkFeedRead(ctx, mustPrincipal(ctx), in.Place, board, before)
			if err != nil {
				return nil, err
			}
			return ok(MarkFeedReadResult{Marked: n})
		}))

	huma.Register(s.api, withAuth(operation("mark-topic-unread", http.MethodDelete, "/topics/{topicID}/read",
		"Mark a topic as not opened; the read position is kept", tagTopics)),
		handle(s, func(ctx context.Context, in *TopicPath) (*struct{}, error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			_, err = s.Service.MarkTopicUnread(ctx, mustPrincipal(ctx), id)
			return nil, err
		}))

	huma.Register(s.api, withAuth(operation("vote-topic", http.MethodPut, "/topics/{topicID}/vote",
		"Vote a topic up or down, replacing an earlier vote (ADD_REACTIONS; not on your own topics)", tagTopics)),
		handle(s, func(ctx context.Context, in *struct {
			TopicPath
			Body VoteRequest
		}) (*Body[Topic], error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			t, err := s.Service.SetTopicVote(ctx, mustPrincipal(ctx), id, in.Body.Value)
			if err != nil {
				return nil, err
			}
			return ok(toTopic(t))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("remove-topic-vote", http.MethodDelete, "/topics/{topicID}/vote",
		"Withdraw the caller's vote", tagTopics)), http.StatusOK),
		handle(s, func(ctx context.Context, in *TopicPath) (*Body[Topic], error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			t, err := s.Service.RemoveTopicVote(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toTopic(t))
		}))
}
