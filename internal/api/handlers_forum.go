package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const (
	tagBoards = "Boards"
	tagTopics = "Topics"
	tagPosts  = "Posts"
	tagSearch = "Search"
)

type BoardPath struct {
	BoardID string `path:"boardID" format:"uuid"`
}

type BoardRolePath struct {
	BoardPath
	RoleID string `path:"roleID" format:"uuid"`
}

type TopicPath struct {
	TopicID string `path:"topicID" format:"uuid"`
}

type PostPath struct {
	PostID string `path:"postID" format:"uuid"`
}

type ReactionPath struct {
	PostPath
	Emoji string `path:"emoji" minLength:"1" maxLength:"128" doc:"A Unicode emoji (URL-encoded) or a shortcode"`
}

// emoji returns the decoded emoji; routers may hand over the raw, still-escaped segment.
func (r ReactionPath) emoji() string {
	if strings.Contains(r.Emoji, "%") {
		if e, err := url.PathUnescape(r.Emoji); err == nil {
			return e
		}
	}
	return r.Emoji
}

type CreateBoardRequest struct {
	ParentID         string `json:"parent_id,omitempty" format:"uuid" doc:"Parent category or board; omit for a top-level board"`
	Kind             string `json:"kind,omitempty" enum:"board,category" default:"board"`
	Slug             string `json:"slug" minLength:"3" maxLength:"32" pattern:"^[a-z0-9][a-z0-9-]*[a-z0-9]$"`
	Name             string `json:"name" minLength:"1" maxLength:"100"`
	Description      string `json:"description,omitempty" maxLength:"2000"`
	ReplyMode        string `json:"reply_mode,omitempty" enum:"flat,threaded" default:"flat"`
	SolutionsEnabled bool   `json:"solutions_enabled,omitempty"`
	IsNSFW           bool   `json:"is_nsfw,omitempty"`
}

type UpdateBoardRequest struct {
	ParentID         *string `json:"parent_id,omitempty" doc:"Move under another board or category; empty string moves to the top level"`
	Slug             *string `json:"slug,omitempty" minLength:"3" maxLength:"32"`
	Name             *string `json:"name,omitempty" minLength:"1" maxLength:"100"`
	Description      *string `json:"description,omitempty" maxLength:"2000"`
	Position         *int32  `json:"position,omitempty" minimum:"0"`
	ReplyMode        *string `json:"reply_mode,omitempty" enum:"flat,threaded"`
	SolutionsEnabled *bool   `json:"solutions_enabled,omitempty"`
	IsNSFW           *bool   `json:"is_nsfw,omitempty"`
}

type OverwriteRequest struct {
	Allow int64 `json:"allow" minimum:"0" doc:"Forum permission bits to grant within the board"`
	Deny  int64 `json:"deny" minimum:"0" doc:"Forum permission bits to remove within the board"`
}

type SubscriptionRequest struct {
	Level string `json:"level" enum:"watching,normal,muted"`
}

type ListTopicsInput struct {
	BoardPath
	PageQuery
	Tag      string `query:"tag" maxLength:"32"`
	Archived bool   `query:"archived" doc:"Include archived topics"`
}

type ListPlaceTopicsInput struct {
	PlacePath
	PageQuery
	Tag string `query:"tag" maxLength:"32"`
}

type ListTagsInput struct {
	PlacePath
	Query string `query:"q" maxLength:"32" doc:"Tag prefix (autocomplete)"`
	Limit int32  `query:"limit" minimum:"1" maximum:"100" default:"20"`
}

type CreateTopicRequest struct {
	Title         string   `json:"title" minLength:"1" maxLength:"200"`
	Content       string   `json:"content,omitempty" maxLength:"50000" doc:"Markdown. May be empty when attachment_ids is set"`
	Tags          []string `json:"tags,omitempty" maxItems:"5"`
	AttachmentIDs []string `json:"attachment_ids,omitempty" maxItems:"10" doc:"Files from POST /attachments for the opening post, in display order"`
}

type UpdateTopicRequest struct {
	Title      *string   `json:"title,omitempty" minLength:"1" maxLength:"200"`
	Tags       *[]string `json:"tags,omitempty" maxItems:"5"`
	IsPinned   *bool     `json:"is_pinned,omitempty" doc:"MANAGE_POSTS"`
	IsLocked   *bool     `json:"is_locked,omitempty" doc:"MANAGE_POSTS"`
	IsArchived *bool     `json:"is_archived,omitempty" doc:"MANAGE_POSTS; archived topics are read-only and hidden from default listings"`
	BoardID    *string   `json:"board_id,omitempty" format:"uuid" doc:"Move to another board (MANAGE_POSTS in both boards)"`
}

type CreatePostRequest struct {
	Content       string   `json:"content,omitempty" maxLength:"50000" doc:"Markdown; @username mentions notify. May be empty when attachment_ids is set"`
	ParentID      string   `json:"parent_id,omitempty" format:"uuid" doc:"The post being replied to"`
	AttachmentIDs []string `json:"attachment_ids,omitempty" maxItems:"10" doc:"Files from POST /attachments, in display order"`
}

type EditPostRequest struct {
	Content string `json:"content" maxLength:"50000" doc:"May be empty when the post has attachments"`
}

type SolutionRequest struct {
	PostID string `json:"post_id" format:"uuid"`
}

type ReadRequest struct {
	PostNumber *int32 `json:"post_number,omitempty" minimum:"1" doc:"Highest post number the user has read; omit to only record that the topic was opened"`
}

type SearchQuery struct {
	PageQuery
	Query      string `query:"q" minLength:"1" maxLength:"200" required:"true" doc:"Words, \"quoted phrases\", OR, and -exclusions"`
	Author     string `query:"author" maxLength:"32" doc:"Username"`
	Tag        string `query:"tag" maxLength:"32"`
	Solved     string `query:"solved" enum:"true,false"`
	After      string `query:"after" format:"date-time"`
	Before     string `query:"before" format:"date-time"`
	TopicsOnly bool   `query:"topics_only" doc:"Only match opening posts"`
	Sort       string `query:"sort" enum:"relevance,newest,oldest" default:"relevance"`
}

func (q SearchQuery) input() (service.SearchInput, error) {
	after, err := parseOptionalTime("after", q.After)
	if err != nil {
		return service.SearchInput{}, err
	}
	before, err := parseOptionalTime("before", q.Before)
	if err != nil {
		return service.SearchInput{}, err
	}
	return service.SearchInput{
		Query: q.Query, Author: q.Author, Tag: q.Tag, Solved: parseOptionalBool(q.Solved),
		After: after, Before: before, TopicsOnly: q.TopicsOnly, Sort: q.Sort,
	}, nil
}

type PlaceSearchInput struct {
	PlacePath
	SearchQuery
	BoardID string `query:"board" format:"uuid"`
}

func (s *Server) registerBoards() {
	huma.Register(s.api, withOptionalAuth(operation("list-boards", http.MethodGet, "/places/{place}/boards",
		"List the boards the caller can see, depth-first in display order", tagBoards)),
		handle(s, func(ctx context.Context, in *PlacePath) (*Body[[]Board], error) {
			boards, err := s.Service.ListBoards(ctx, principalFrom(ctx), in.Place)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(boards, toBoard))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("create-board", http.MethodPost, "/places/{place}/boards",
		"Create a board or category (MANAGE_BOARDS)", tagBoards)), http.StatusCreated),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body CreateBoardRequest
		}) (*Body[Board], error) {
			b := in.Body
			parent, err := parseOptionalID("parent_id", b.ParentID)
			if err != nil {
				return nil, err
			}
			v, err := s.Service.CreateBoard(ctx, mustPrincipal(ctx), in.Place, service.CreateBoardInput{
				ParentID: parent, Kind: b.Kind, Slug: b.Slug, Name: b.Name, Description: b.Description,
				ReplyMode: b.ReplyMode, SolutionsEnabled: b.SolutionsEnabled, IsNSFW: b.IsNSFW,
			})
			if err != nil {
				return nil, err
			}
			return ok(toBoard(v))
		}))

	huma.Register(s.api, withOptionalAuth(operation("get-board", http.MethodGet, "/boards/{boardID}",
		"Get a board", tagBoards)),
		handle(s, func(ctx context.Context, in *BoardPath) (*Body[Board], error) {
			id, err := parseID("boardID", in.BoardID)
			if err != nil {
				return nil, err
			}
			v, err := s.Service.GetBoard(ctx, principalFrom(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toBoard(v))
		}))

	huma.Register(s.api, withAuth(operation("update-board", http.MethodPatch, "/boards/{boardID}",
		"Update, reorder or move a board (MANAGE_BOARDS)", tagBoards)),
		handle(s, func(ctx context.Context, in *struct {
			BoardPath
			Body UpdateBoardRequest
		}) (*Body[Board], error) {
			id, err := parseID("boardID", in.BoardID)
			if err != nil {
				return nil, err
			}
			b := in.Body
			v, err := s.Service.UpdateBoard(ctx, mustPrincipal(ctx), id, service.BoardUpdate{
				ParentID: b.ParentID, Slug: b.Slug, Name: b.Name, Description: b.Description, Position: b.Position,
				ReplyMode: b.ReplyMode, SolutionsEnabled: b.SolutionsEnabled, IsNSFW: b.IsNSFW,
			})
			if err != nil {
				return nil, err
			}
			return ok(toBoard(v))
		}))

	huma.Register(s.api, withAuth(operation("delete-board", http.MethodDelete, "/boards/{boardID}",
		"Delete an empty board (MANAGE_BOARDS)", tagBoards)),
		handle(s, func(ctx context.Context, in *BoardPath) (*struct{}, error) {
			id, err := parseID("boardID", in.BoardID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteBoard(ctx, mustPrincipal(ctx), id)
		}))

	huma.Register(s.api, withAuth(operation("list-board-overwrites", http.MethodGet, "/boards/{boardID}/overwrites",
		"List a board's role permission overwrites (MANAGE_BOARDS)", tagBoards)),
		handle(s, func(ctx context.Context, in *BoardPath) (*Body[[]Overwrite], error) {
			id, err := parseID("boardID", in.BoardID)
			if err != nil {
				return nil, err
			}
			ows, err := s.Service.ListBoardOverwrites(ctx, mustPrincipal(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(ows, toOverwrite))
		}))

	huma.Register(s.api, withAuth(operation("set-board-overwrite", http.MethodPut, "/boards/{boardID}/overwrites/{roleID}",
		"Allow or deny forum permissions for a role within a board and its children (MANAGE_BOARDS)", tagBoards)),
		handle(s, func(ctx context.Context, in *struct {
			BoardRolePath
			Body OverwriteRequest
		}) (*Body[Overwrite], error) {
			boardID, err := parseID("boardID", in.BoardID)
			if err != nil {
				return nil, err
			}
			roleID, err := parseID("roleID", in.RoleID)
			if err != nil {
				return nil, err
			}
			ow, err := s.Service.SetBoardOverwrite(ctx, mustPrincipal(ctx), boardID, roleID, in.Body.Allow, in.Body.Deny)
			if err != nil {
				return nil, err
			}
			return ok(toOverwrite(ow))
		}))

	huma.Register(s.api, withAuth(operation("delete-board-overwrite", http.MethodDelete, "/boards/{boardID}/overwrites/{roleID}",
		"Remove a role's overwrite from a board (MANAGE_BOARDS)", tagBoards)),
		handle(s, func(ctx context.Context, in *BoardRolePath) (*struct{}, error) {
			boardID, err := parseID("boardID", in.BoardID)
			if err != nil {
				return nil, err
			}
			roleID, err := parseID("roleID", in.RoleID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteBoardOverwrite(ctx, mustPrincipal(ctx), boardID, roleID)
		}))

	huma.Register(s.api, withStatus(withAuth(operation("set-board-subscription", http.MethodPut, "/boards/{boardID}/subscription",
		"Watch (notify on new topics), mute, or reset notifications for a board", tagBoards)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *struct {
			BoardPath
			Body SubscriptionRequest
		}) (*struct{}, error) {
			id, err := parseID("boardID", in.BoardID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.SetBoardSubscription(ctx, mustPrincipal(ctx), id, in.Body.Level)
		}))
}

func (s *Server) registerTopics() {
	huma.Register(s.api, withOptionalAuth(operation("list-board-topics", http.MethodGet, "/boards/{boardID}/topics",
		"List a board's topics, pinned first, then by latest activity", tagTopics)),
		handle(s, func(ctx context.Context, in *ListTopicsInput) (*Body[Page[Topic]], error) {
			id, err := parseID("boardID", in.BoardID)
			if err != nil {
				return nil, err
			}
			page := in.pagination()
			topics, err := s.Service.ListBoardTopics(ctx, principalFrom(ctx), id,
				service.TopicFilter{Tag: in.Tag, IncludeArchived: in.Archived}, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(topics, toTopic, page))
		}))

	huma.Register(s.api, withContentRateLimit(withStatus(withAuth(operation("create-topic", http.MethodPost,
		"/boards/{boardID}/topics", "Start a topic (CREATE_TOPICS)", tagTopics)), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *struct {
			BoardPath
			Body CreateTopicRequest
		}) (*Body[TopicWithPost], error) {
			id, err := parseID("boardID", in.BoardID)
			if err != nil {
				return nil, err
			}
			files, err := parseAttachmentIDs(in.Body.AttachmentIDs)
			if err != nil {
				return nil, err
			}
			t, p, err := s.Service.CreateTopic(ctx, mustPrincipal(ctx), id, service.CreateTopicInput{
				Title: in.Body.Title, Content: in.Body.Content, Tags: in.Body.Tags, AttachmentIDs: files,
			})
			if err != nil {
				return nil, err
			}
			return ok(TopicWithPost{Topic: toTopic(t), Post: toPost(p)})
		}))

	huma.Register(s.api, withOptionalAuth(operation("list-place-topics", http.MethodGet, "/places/{place}/topics",
		"Latest topics across every board the caller can see", tagTopics)),
		handle(s, func(ctx context.Context, in *ListPlaceTopicsInput) (*Body[Page[Topic]], error) {
			page := in.pagination()
			topics, err := s.Service.ListPlaceTopics(ctx, principalFrom(ctx), in.Place, in.Tag, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(topics, toTopic, page))
		}))

	huma.Register(s.api, withOptionalAuth(operation("list-place-tags", http.MethodGet, "/places/{place}/tags",
		"Tags in use, most used first (autocomplete with q)", tagTopics)),
		handle(s, func(ctx context.Context, in *ListTagsInput) (*Body[[]Tag], error) {
			tags, err := s.Service.ListTags(ctx, principalFrom(ctx), in.Place, in.Query, in.Limit)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(tags, toTag))
		}))

	huma.Register(s.api, withOptionalAuth(operation("get-topic", http.MethodGet, "/topics/{topicID}",
		"Get a topic", tagTopics)),
		handle(s, func(ctx context.Context, in *TopicPath) (*Body[Topic], error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			t, err := s.Service.GetTopic(ctx, principalFrom(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toTopic(t))
		}))

	huma.Register(s.api, withAuth(operation("update-topic", http.MethodPatch, "/topics/{topicID}",
		"Edit a topic's title/tags (author or MANAGE_POSTS), or pin, lock, archive or move it (MANAGE_POSTS)", tagTopics)),
		handle(s, func(ctx context.Context, in *struct {
			TopicPath
			Body UpdateTopicRequest
		}) (*Body[Topic], error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			b := in.Body
			update := service.TopicUpdate{Title: b.Title, Tags: b.Tags, IsPinned: b.IsPinned, IsLocked: b.IsLocked, IsArchived: b.IsArchived}
			if b.BoardID != nil {
				if update.BoardID, err = parseOptionalID("board_id", *b.BoardID); err != nil {
					return nil, err
				}
			}
			t, err := s.Service.UpdateTopic(ctx, mustPrincipal(ctx), id, update)
			if err != nil {
				return nil, err
			}
			return ok(toTopic(t))
		}))

	huma.Register(s.api, withAuth(operation("delete-topic", http.MethodDelete, "/topics/{topicID}",
		"Delete a topic (its author while it has no replies, or MANAGE_POSTS)", tagTopics)),
		handle(s, func(ctx context.Context, in *struct {
			TopicPath
			ReasonQuery
		}) (*struct{}, error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeleteTopic(ctx, mustPrincipal(ctx), id, in.Reason)
		}))

	huma.Register(s.api, withAuth(operation("set-topic-solution", http.MethodPut, "/topics/{topicID}/solution",
		"Mark a reply as the accepted answer (topic author or MANAGE_POSTS)", tagTopics)),
		handle(s, func(ctx context.Context, in *struct {
			TopicPath
			Body SolutionRequest
		}) (*Body[Topic], error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			postID, err := parseID("post_id", in.Body.PostID)
			if err != nil {
				return nil, err
			}
			t, err := s.Service.SetSolution(ctx, mustPrincipal(ctx), id, &postID)
			if err != nil {
				return nil, err
			}
			return ok(toTopic(t))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("clear-topic-solution", http.MethodDelete, "/topics/{topicID}/solution",
		"Clear the accepted answer (topic author or MANAGE_POSTS)", tagTopics)), http.StatusOK),
		handle(s, func(ctx context.Context, in *TopicPath) (*Body[Topic], error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			t, err := s.Service.SetSolution(ctx, mustPrincipal(ctx), id, nil)
			if err != nil {
				return nil, err
			}
			return ok(toTopic(t))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("mark-topic-read", http.MethodPut, "/topics/{topicID}/read",
		"Record that the caller opened the topic and, with post_number, their read position (never moves backwards)",
		tagTopics)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *struct {
			TopicPath
			Body *ReadRequest `required:"false"`
		}) (*struct{}, error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			var n *int32
			if in.Body != nil {
				n = in.Body.PostNumber
			}
			_, err = s.Service.MarkRead(ctx, mustPrincipal(ctx), id, n)
			return nil, err
		}))

	huma.Register(s.api, withStatus(withAuth(operation("set-topic-subscription", http.MethodPut, "/topics/{topicID}/subscription",
		"Watch (notify on every reply), mute, or reset notifications for a topic", tagTopics)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *struct {
			TopicPath
			Body SubscriptionRequest
		}) (*struct{}, error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.SetTopicSubscription(ctx, mustPrincipal(ctx), id, in.Body.Level)
		}))
}

func (s *Server) registerPosts() {
	huma.Register(s.api, withOptionalAuth(operation("list-posts", http.MethodGet, "/topics/{topicID}/posts",
		"List a topic's posts: chronological, or depth-first in threaded boards", tagPosts)),
		handle(s, func(ctx context.Context, in *struct {
			TopicPath
			PageQuery
		}) (*Body[Page[Post]], error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			page := in.pagination()
			posts, err := s.Service.ListPosts(ctx, principalFrom(ctx), id, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(posts, toPost, page))
		}))

	huma.Register(s.api, withContentRateLimit(withStatus(withAuth(operation("create-post", http.MethodPost,
		"/topics/{topicID}/posts", "Reply to a topic (REPLY_TO_TOPICS)", tagPosts)), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *struct {
			TopicPath
			Body CreatePostRequest
		}) (*Body[Post], error) {
			id, err := parseID("topicID", in.TopicID)
			if err != nil {
				return nil, err
			}
			parent, err := parseOptionalID("parent_id", in.Body.ParentID)
			if err != nil {
				return nil, err
			}
			files, err := parseAttachmentIDs(in.Body.AttachmentIDs)
			if err != nil {
				return nil, err
			}
			p, err := s.Service.CreatePost(ctx, mustPrincipal(ctx), id, service.ReplyInput{
				Content: in.Body.Content, ParentID: parent, AttachmentIDs: files,
			})
			if err != nil {
				return nil, err
			}
			return ok(toPost(p))
		}))

	huma.Register(s.api, withOptionalAuth(operation("get-post", http.MethodGet, "/posts/{postID}",
		"Get a post", tagPosts)),
		handle(s, func(ctx context.Context, in *PostPath) (*Body[Post], error) {
			id, err := parseID("postID", in.PostID)
			if err != nil {
				return nil, err
			}
			p, err := s.Service.GetPost(ctx, principalFrom(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(toPost(p))
		}))

	huma.Register(s.api, withAuth(operation("edit-post", http.MethodPatch, "/posts/{postID}",
		"Edit a post, keeping the previous version (author or MANAGE_POSTS)", tagPosts)),
		handle(s, func(ctx context.Context, in *struct {
			PostPath
			Body EditPostRequest
		}) (*Body[Post], error) {
			id, err := parseID("postID", in.PostID)
			if err != nil {
				return nil, err
			}
			p, err := s.Service.EditPost(ctx, mustPrincipal(ctx), id, in.Body.Content)
			if err != nil {
				return nil, err
			}
			return ok(toPost(p))
		}))

	huma.Register(s.api, withAuth(operation("delete-post", http.MethodDelete, "/posts/{postID}",
		"Delete a reply, leaving a tombstone (author or MANAGE_POSTS)", tagPosts)),
		handle(s, func(ctx context.Context, in *struct {
			PostPath
			ReasonQuery
		}) (*struct{}, error) {
			id, err := parseID("postID", in.PostID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.DeletePost(ctx, mustPrincipal(ctx), id, in.Reason)
		}))

	huma.Register(s.api, withOptionalAuth(operation("list-post-revisions", http.MethodGet, "/posts/{postID}/revisions",
		"List a post's previous versions, newest first", tagPosts)),
		handle(s, func(ctx context.Context, in *PostPath) (*Body[[]Revision], error) {
			id, err := parseID("postID", in.PostID)
			if err != nil {
				return nil, err
			}
			revs, err := s.Service.ListRevisions(ctx, principalFrom(ctx), id)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(revs, toRevision))
		}))

	huma.Register(s.api, withOptionalAuth(operation("list-reaction-users", http.MethodGet, "/posts/{postID}/reactions/{emoji}",
		"List who reacted with an emoji", tagPosts)),
		handle(s, func(ctx context.Context, in *struct {
			ReactionPath
			PageQuery
		}) (*Body[Page[User]], error) {
			id, err := parseID("postID", in.PostID)
			if err != nil {
				return nil, err
			}
			page := in.pagination()
			users, err := s.Service.ListReactionUsers(ctx, principalFrom(ctx), id, in.emoji(), page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(users, toUser, page))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("add-reaction", http.MethodPut, "/posts/{postID}/reactions/{emoji}",
		"React to a post (ADD_REACTIONS)", tagPosts)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *ReactionPath) (*struct{}, error) {
			id, err := parseID("postID", in.PostID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.AddReaction(ctx, mustPrincipal(ctx), id, in.emoji())
		}))

	huma.Register(s.api, withAuth(operation("remove-reaction", http.MethodDelete, "/posts/{postID}/reactions/{emoji}",
		"Remove the caller's reaction", tagPosts)),
		handle(s, func(ctx context.Context, in *ReactionPath) (*struct{}, error) {
			id, err := parseID("postID", in.PostID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.RemoveReaction(ctx, mustPrincipal(ctx), id, in.emoji())
		}))
}

func (s *Server) registerSearch() {
	huma.Register(s.api, operation("search", http.MethodGet, "/search",
		"Search public content across the instance", tagSearch),
		handle(s, func(ctx context.Context, in *SearchQuery) (*Body[Page[SearchResult]], error) {
			input, err := in.input()
			if err != nil {
				return nil, err
			}
			page := in.pagination()
			hits, err := s.Service.SearchPublic(ctx, input, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(hits, toSearchResult, page))
		}))

	huma.Register(s.api, withOptionalAuth(operation("search-place", http.MethodGet, "/places/{place}/search",
		"Search every board of a place the caller can read", tagSearch)),
		handle(s, func(ctx context.Context, in *PlaceSearchInput) (*Body[Page[SearchResult]], error) {
			input, err := in.input()
			if err != nil {
				return nil, err
			}
			if input.BoardID, err = parseOptionalID("board", in.BoardID); err != nil {
				return nil, err
			}
			page := in.pagination()
			hits, err := s.Service.SearchPlace(ctx, principalFrom(ctx), in.Place, input, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(hits, toSearchResult, page))
		}))
}
