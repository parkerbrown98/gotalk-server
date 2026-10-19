package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/ratelimit"
	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const (
	tagIndex = "Index"
	// indexPath is where the index read endpoints live, under the API prefix.
	indexPath = "/index"
	// problemTypeBase prefixes the problem type of index errors, as the protocol specifies.
	problemTypeBase = "https://gotalk.sh/problems/"
)

// IndexProblem is an RFC 9457 problem with the index protocol's stable code.
type IndexProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	Code   string `json:"code"`
}

func (p *IndexProblem) Error() string               { return p.Detail }
func (p *IndexProblem) GetStatus() int              { return p.Status }
func (p *IndexProblem) ContentType(_ string) string { return "application/problem+json" }
func indexProblem(status int, code, detail string) error {
	return &IndexProblem{Type: problemTypeBase + code, Title: http.StatusText(status), Status: status, Detail: detail, Code: code}
}

var (
	errIndexNotFound = indexProblem(http.StatusNotFound, "not_found", "not found, or not indexable")
	errIndexCursor   = indexProblem(http.StatusBadRequest, "invalid_cursor", "cursor is not valid; pass next_cursor from a previous page")
)

func writeIndexProblem(ctx huma.Context, status int, code, detail string) {
	ctx.SetHeader("Content-Type", "application/problem+json")
	ctx.SetStatus(status)
	_ = json.NewEncoder(ctx.BodyWriter()).Encode(indexProblem(status, code, detail))
}

// indexError maps service errors to index problems. Anything missing or not indexable is a
// plain 404, with no hint about which.
func (s *Server) indexError(ctx context.Context, err error) error {
	var ae *apperr.Error
	switch {
	case errors.Is(err, service.ErrIndexCursorExpired):
		return indexProblem(http.StatusGone, "cursor_expired", err.Error()+"; run a reconcile and resume from cursor=latest")
	case errors.As(err, &ae) && ae.Kind == apperr.KindNotFound:
		return errIndexNotFound
	case errors.As(err, &ae) && ae.Kind == apperr.KindInvalid:
		return indexProblem(http.StatusBadRequest, "invalid_parameter", ae.Message)
	}
	return s.toHTTPError(ctx, err)
}

// Index documents. Field names follow the index protocol; see docs/api-index.md.

type IndexAuthor struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

type IndexBoard struct {
	ID          string    `json:"id"`
	ParentID    *string   `json:"parent_id" nullable:"true" doc:"Parent board or category; null at the top level"`
	Kind        string    `json:"kind" enum:"category,board"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Position    int32     `json:"position"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type IndexPlace struct {
	ID          string       `json:"id"`
	Slug        string       `json:"slug"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	IconURL     *string      `json:"icon_url" nullable:"true"`
	MemberCount int32        `json:"member_count"`
	Tags        []string     `json:"tags"`
	Locale      string       `json:"locale"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Boards      []IndexBoard `json:"boards" doc:"Indexable boards and the categories that group them, in display order"`
}

type IndexPlacesPage struct {
	Items      []IndexPlace `json:"items"`
	NextCursor *string      `json:"next_cursor" nullable:"true"`
}

type IndexTopicSummary struct {
	ID         string      `json:"id"`
	Slug       string      `json:"slug"`
	Title      string      `json:"title"`
	Author     IndexAuthor `json:"author"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at" doc:"Last change visible to the index (edits, replies, removals, moves)"`
	ReplyCount int32       `json:"reply_count" doc:"Indexable replies"`
	Pinned     bool        `json:"pinned"`
	Locked     bool        `json:"locked"`
	Archived   bool        `json:"archived"`
	Solved     bool        `json:"solved"`
	Tags       []string    `json:"tags"`
}

type IndexTopicsPage struct {
	Items      []IndexTopicSummary `json:"items"`
	NextCursor *string             `json:"next_cursor" nullable:"true"`
}

type IndexPost struct {
	ID         string      `json:"id"`
	Number     int32       `json:"number" doc:"Stable; posts left out of the index leave gaps"`
	ParentID   *string     `json:"parent_id" nullable:"true"`
	Author     IndexAuthor `json:"author"`
	Content    string      `json:"content" doc:"Markdown source"`
	CreatedAt  time.Time   `json:"created_at"`
	EditedAt   *time.Time  `json:"edited_at" nullable:"true"`
	IsSolution bool        `json:"is_solution"`
}

type IndexTopicPage struct {
	IndexTopicSummary
	PlaceID    string      `json:"place_id"`
	BoardID    string      `json:"board_id"`
	Posts      []IndexPost `json:"posts"`
	NextCursor *string     `json:"next_cursor" nullable:"true"`
}

type IndexEvent struct {
	Seq     int64     `json:"seq"`
	Type    string    `json:"type" enum:"place.upsert,place.delete,board.upsert,board.delete,topic.upsert,topic.delete,user.exclude,user.include"`
	At      time.Time `json:"at"`
	PlaceID *string   `json:"place_id,omitempty"`
	BoardID *string   `json:"board_id,omitempty"`
	TopicID *string   `json:"topic_id,omitempty"`
	UserID  *string   `json:"user_id,omitempty"`
}

type IndexChangesPage struct {
	Events     []IndexEvent `json:"events"`
	NextCursor string       `json:"next_cursor"`
	HasMore    bool         `json:"has_more"`
}

// Cursors are opaque to clients: unpadded base64url of a versioned position.

func encodeCursor(v string) string { return base64.RawURLEncoding.EncodeToString([]byte("1:" + v)) }

func decodeCursor(c string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !strings.HasPrefix(string(raw), "1:") {
		return "", errIndexCursor
	}
	return string(raw[2:]), nil
}

type IndexPageQuery struct {
	Cursor string `query:"cursor" maxLength:"256" doc:"next_cursor from the previous page"`
	Limit  int32  `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

type IndexBoardTopicsInput struct {
	BoardID string `path:"boardID"`
	IndexPageQuery
	UpdatedSince string `query:"updated_since" doc:"RFC 3339; only topics updated at or after this time"`
}

type IndexTopicInput struct {
	TopicID string `path:"topicID"`
	Cursor  string `query:"cursor" maxLength:"256"`
	Limit   int32  `query:"limit" minimum:"1" maximum:"100" default:"50"`
}

type IndexChangesInput struct {
	Cursor string `query:"cursor" maxLength:"256" doc:"next_cursor from the previous page, or latest for the current end of the feed; omit to start at the oldest retained event"`
	Limit  int32  `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

func indexAuthorOf(a service.IndexAuthor) IndexAuthor {
	return IndexAuthor{ID: a.ID.String(), Username: a.Username, DisplayName: a.DisplayName}
}

func indexTopicOf(t service.IndexTopic) IndexTopicSummary {
	tags := t.Topic.Tags
	if tags == nil {
		tags = []string{}
	}
	return IndexTopicSummary{
		ID: t.Topic.ID.String(), Slug: t.Topic.Slug, Title: t.Topic.Title, Author: indexAuthorOf(t.Author),
		CreatedAt: t.Topic.CreatedAt.UTC(), UpdatedAt: t.Topic.IndexUpdatedAt.UTC(), ReplyCount: t.ReplyCount,
		Pinned: t.Topic.IsPinned, Locked: t.Topic.IsLocked, Archived: t.Topic.IsArchived,
		Solved: t.Topic.SolutionPostID != nil, Tags: tags,
	}
}

func optID(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// absoluteURL makes a stored relative media URL absolute, since index documents are read
// from another origin.
func absoluteURL(base string, u *string) *string {
	if u == nil || !strings.HasPrefix(*u, "/") {
		return u
	}
	abs := base + *u
	return &abs
}

func (s *Server) registerIndex() {
	op := func(id, path, summary, desc string) huma.Operation {
		o := operation(id, http.MethodGet, indexPath+path, summary, tagIndex)
		o.Description = desc + " Unauthenticated, rate limited per IP in the index tier. Responses carry an ETag; " +
			"send If-None-Match to get 304 when nothing changed. See the index protocol in docs/api-index.md."
		o.Errors = []int{http.StatusNotFound, http.StatusTooManyRequests}
		return withMeta(o, rateLimitTierKey, ratelimit.TierIndex)
	}

	huma.Register(s.api, op("list-index-places", "/places", "List indexable places with their indexable boards",
		"Places whose owners opted in to the public index, ordered by ID. Empty when directory listing is off."),
		func(ctx context.Context, in *IndexPageQuery) (*Body[IndexPlacesPage], error) {
			var after *uuid.UUID
			if in.Cursor != "" {
				v, err := decodeCursor(in.Cursor)
				if err != nil {
					return nil, err
				}
				id, err := uuid.Parse(v)
				if err != nil {
					return nil, errIndexCursor
				}
				after = &id
			}
			places, more, err := s.Service.IndexPlaces(ctx, after, in.Limit)
			if err != nil {
				return nil, s.indexError(ctx, err)
			}
			base := baseURLFrom(ctx)
			page := IndexPlacesPage{Items: make([]IndexPlace, len(places))}
			for i, p := range places {
				boards := make([]IndexBoard, len(p.Boards))
				for j, b := range p.Boards {
					boards[j] = IndexBoard{
						ID: b.ID.String(), ParentID: optID(b.ParentID), Kind: b.Kind, Slug: b.Slug, Name: b.Name,
						Description: b.Description, Position: b.Position, UpdatedAt: b.UpdatedAt.UTC(),
					}
				}
				page.Items[i] = IndexPlace{
					ID: p.Place.ID.String(), Slug: p.Place.Slug, Name: p.Place.Name, Description: p.Place.Description,
					IconURL: absoluteURL(base, p.Place.IconUrl), MemberCount: p.Place.MemberCount, Tags: []string{},
					Locale: p.Place.Locale, UpdatedAt: p.Place.UpdatedAt.UTC(), Boards: boards,
				}
			}
			if more && len(places) > 0 {
				c := encodeCursor(places[len(places)-1].Place.ID.String())
				page.NextCursor = &c
			}
			return ok(page)
		})

	huma.Register(s.api, op("list-index-board-topics", "/boards/{boardID}/topics", "List an indexable board's topics",
		"Indexable topics ordered by updated_at, then ID, so a reader can resume after a gap. Categories have no topics."),
		func(ctx context.Context, in *IndexBoardTopicsInput) (*Body[IndexTopicsPage], error) {
			boardID, err := uuid.Parse(in.BoardID)
			if err != nil {
				return nil, errIndexNotFound
			}
			since, err := parseOptionalTime("updated_since", in.UpdatedSince)
			if err != nil {
				return nil, s.indexError(ctx, err)
			}
			var afterAt *time.Time
			var afterID *uuid.UUID
			if in.Cursor != "" {
				v, err := decodeCursor(in.Cursor)
				if err != nil {
					return nil, err
				}
				at, id, ok := strings.Cut(v, "|")
				t, terr := time.Parse(time.RFC3339Nano, at)
				uid, uerr := uuid.Parse(id)
				if !ok || terr != nil || uerr != nil {
					return nil, errIndexCursor
				}
				afterAt, afterID = &t, &uid
			}
			topics, more, err := s.Service.IndexBoardTopics(ctx, boardID, since, afterAt, afterID, in.Limit)
			if err != nil {
				return nil, s.indexError(ctx, err)
			}
			page := IndexTopicsPage{Items: make([]IndexTopicSummary, len(topics))}
			for i, t := range topics {
				page.Items[i] = indexTopicOf(t)
			}
			if more && len(topics) > 0 {
				last := topics[len(topics)-1].Topic
				c := encodeCursor(last.IndexUpdatedAt.UTC().Format(time.RFC3339Nano) + "|" + last.ID.String())
				page.NextCursor = &c
			}
			return ok(page)
		})

	huma.Register(s.api, op("get-index-topic", "/topics/{topicID}", "Get an indexable topic and a page of its posts",
		"The topic summary plus its indexable posts in post-number order, with Markdown content."),
		func(ctx context.Context, in *IndexTopicInput) (*Body[IndexTopicPage], error) {
			topicID, err := uuid.Parse(in.TopicID)
			if err != nil {
				return nil, errIndexNotFound
			}
			after := int32(0)
			if in.Cursor != "" {
				v, err := decodeCursor(in.Cursor)
				if err != nil {
					return nil, err
				}
				n, err := strconv.ParseInt(strings.TrimPrefix(v, "n"), 10, 32)
				if err != nil || !strings.HasPrefix(v, "n") || n < 0 {
					return nil, errIndexCursor
				}
				after = int32(n)
			}
			t, err := s.Service.IndexTopicPosts(ctx, topicID, after, in.Limit)
			if err != nil {
				return nil, s.indexError(ctx, err)
			}
			page := IndexTopicPage{
				IndexTopicSummary: indexTopicOf(t.IndexTopic),
				PlaceID:           t.Topic.PlaceID.String(), BoardID: t.Topic.BoardID.String(),
				Posts: make([]IndexPost, len(t.Posts)),
			}
			for i, p := range t.Posts {
				page.Posts[i] = IndexPost{
					ID: p.Post.ID.String(), Number: p.Post.PostNumber, ParentID: optID(p.Post.ParentID), Author: indexAuthorOf(p.Author),
					Content: p.Post.Content, CreatedAt: p.Post.CreatedAt.UTC(), EditedAt: utcPtr(p.Post.EditedAt),
					IsSolution: t.Topic.SolutionPostID != nil && *t.Topic.SolutionPostID == p.Post.ID,
				}
			}
			if t.More && len(t.Posts) > 0 {
				c := encodeCursor("n" + strconv.Itoa(int(t.Posts[len(t.Posts)-1].Post.PostNumber)))
				page.NextCursor = &c
			}
			return ok(page)
		})

	changes := op("list-index-changes", "/changes", "Read the index change feed",
		"An ordered stream of references to changed objects (IDs, never content). Events for one object may be "+
			"coalesced into the latest. A cursor older than the retained history (30 days) answers 410 cursor_expired.")
	changes.Errors = append(changes.Errors, http.StatusGone)
	huma.Register(s.api, changes,
		func(ctx context.Context, in *IndexChangesInput) (*Body[IndexChangesPage], error) {
			var after *int64
			latest := in.Cursor == "latest"
			if in.Cursor != "" && !latest {
				v, err := decodeCursor(in.Cursor)
				if err != nil {
					return nil, err
				}
				n, err := strconv.ParseInt(strings.TrimPrefix(v, "s"), 10, 64)
				if err != nil || !strings.HasPrefix(v, "s") || n < 0 {
					return nil, errIndexCursor
				}
				after = &n
			}
			feed, err := s.Service.IndexChanges(ctx, after, latest, in.Limit)
			if err != nil {
				return nil, s.indexError(ctx, err)
			}
			page := IndexChangesPage{Events: make([]IndexEvent, len(feed.Events)), HasMore: feed.HasMore,
				NextCursor: encodeCursor("s" + strconv.FormatInt(feed.Next, 10))}
			for i, e := range feed.Events {
				page.Events[i] = IndexEvent{Seq: e.Seq, Type: e.Type, At: e.At.UTC(),
					PlaceID: optID(e.PlaceID), BoardID: optID(e.BoardID), TopicID: optID(e.TopicID), UserID: optID(e.UserID)}
			}
			return ok(page)
		})
}

// indexCaching gives index responses a strong ETag over the body and answers a matching
// If-None-Match with 304, so an unchanged page costs the reader almost nothing.
func indexCaching(next http.Handler) http.Handler {
	prefix := APIPrefix + indexPath + "/"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &bufferedResponse{header: w.Header(), status: http.StatusOK}
		next.ServeHTTP(rec, r)
		h := w.Header()
		h.Set("Cache-Control", "no-cache")
		if rec.status == http.StatusOK {
			sum := sha256.Sum256(rec.body.Bytes())
			etag := `"` + base64.RawURLEncoding.EncodeToString(sum[:18]) + `"`
			h.Set("ETag", etag)
			if etagMatches(r.Header.Get("If-None-Match"), etag) {
				h.Del("Content-Length")
				h.Del("Content-Type")
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		h.Set("Content-Length", strconv.Itoa(rec.body.Len()))
		w.WriteHeader(rec.status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(rec.body.Bytes())
		}
	})
}

func etagMatches(header, etag string) bool {
	for tag := range strings.SplitSeq(header, ",") {
		tag = strings.TrimPrefix(strings.TrimSpace(tag), "W/")
		if tag == etag || tag == "*" {
			return true
		}
	}
	return false
}

type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if !b.wrote {
		b.status, b.wrote = status, true
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.WriteHeader(http.StatusOK)
	return b.body.Write(p)
}

// wellKnownDirectory is the directory block of /.well-known/gotalk-instance, or nil when
// the instance is not listed.
func (s *Server) wellKnownDirectory(ctx context.Context, base string) (map[string]any, error) {
	info, err := s.Service.Directory(ctx)
	if err != nil || !info.Listed {
		return nil, err
	}
	keys := make([]map[string]string, len(info.Keys))
	for i, k := range info.Keys {
		keys[i] = map[string]string{"alg": k.Alg, "public_key": k.PublicKey}
	}
	dir := map[string]any{
		"listed":    true,
		"protocol":  service.IndexProtocolVersion,
		"index_url": base + APIPrefix + indexPath,
		"keys":      keys,
	}
	if info.ContactEmail != "" {
		dir["contact_email"] = info.ContactEmail
	}
	return dir, nil
}

// instanceOrigin is the instance's public origin, for links to the index's status page.
func (s *Server) instanceOrigin(ctx context.Context) string {
	if s.Config.Server.PublicURL != "" {
		return s.Config.Server.PublicURL
	}
	return baseURLFrom(ctx)
}
