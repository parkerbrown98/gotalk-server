package service

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// The public index (docs/api-index.md).
//
// What may be indexed is decided here and nowhere else: placeIndexable and
// forumScope.indexableBoards hold the place and board rules, and the Index* queries in
// internal/store/queries/index.sql hold the topic and post rules. The read endpoints and the
// derived in_index flags (which gate change feed events) both go through these functions.

// Change feed event types.
const (
	IndexPlaceUpsert = "place.upsert"
	IndexPlaceDelete = "place.delete"
	IndexBoardUpsert = "board.upsert"
	IndexBoardDelete = "board.delete"
	IndexTopicUpsert = "topic.upsert"
	IndexTopicDelete = "topic.delete"
	IndexUserExclude = "user.exclude"
	IndexUserInclude = "user.include"
)

// Index paging limits.
const (
	IndexDefaultLimit      = 100
	IndexMaxLimit          = 500
	IndexPostsDefaultLimit = 50
	IndexPostsMaxLimit     = 100
)

// IndexRetention is how long change feed events are kept. The protocol requires at least
// 30 days.
var IndexRetention = 30 * 24 * time.Hour

// ErrIndexCursorExpired means a change feed cursor points before retained history.
var ErrIndexCursorExpired = errors.New("cursor is older than the retained change history")

// placeIndexable is the place rule: listing on, public, opted in, not NSFW, not deleted.
func placeIndexable(p store.Place, listed bool) bool {
	return listed && p.DeletedAt == nil && p.Visibility == "public" && p.Indexable && !p.IsNsfw
}

// indexableBoards is the board rule within an indexable place: readable by signed-out
// visitors, and neither the board nor any ancestor is NSFW or opted out. Categories are
// included so the hierarchy can be rebuilt.
func (f *forumScope) indexableBoards() map[uuid.UUID]bool {
	guest := f.guest()
	out := map[uuid.UUID]bool{}
	for _, b := range f.ordered {
		if !f.visibleTo(guest, b.ID) {
			continue
		}
		ok := true
		for _, c := range f.chain(b.ID) {
			if !c.Indexable || c.IsNsfw {
				ok = false
				break
			}
		}
		if ok {
			out[b.ID] = true
		}
	}
	return out
}

// indexScope is an indexable place with its indexable boards in display order.
type indexScope struct {
	place  store.Place
	boards []store.Board
	ids    map[uuid.UUID]bool
}

// indexScopeOf applies the place and board rules. It returns nil when the place is not
// indexable.
func (s *Service) indexScopeOf(ctx context.Context, q *store.Queries, place store.Place) (*indexScope, error) {
	if !placeIndexable(place, s.directoryListed()) {
		return nil, nil
	}
	f, err := s.loadBoards(ctx, q, place)
	if err != nil {
		return nil, err
	}
	sc := &indexScope{place: place, ids: f.indexableBoards()}
	for _, b := range f.ordered {
		if sc.ids[b.ID] {
			sc.boards = append(sc.boards, b)
		}
	}
	return sc, nil
}

// indexTouch names objects whose details changed, so they are re-announced even when
// their indexability did not change.
type indexTouch struct {
	place  bool
	boards []uuid.UUID
}

// refreshIndexState recomputes whether a place and its boards are in the index, stores the
// result in in_index, and emits change feed events for every difference. Call it after
// anything that can change a place's or board's indexability or public details.
func (s *Service) refreshIndexState(ctx context.Context, q *store.Queries, placeID uuid.UUID, touch indexTouch) error {
	place, err := q.GetPlaceByID(ctx, placeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	boards, err := q.ListBoards(ctx, placeID)
	if err != nil {
		return err
	}
	sc, err := s.indexScopeOf(ctx, q, place)
	if err != nil {
		return err
	}
	in := sc != nil
	now := map[uuid.UUID]bool{}
	if in {
		now = sc.ids
	}

	pid := &place.ID
	switch {
	case place.InIndex && !in:
		if err := s.emitIndex(ctx, q, IndexPlaceDelete, pid, nil, nil, nil); err != nil {
			return err
		}
	case !place.InIndex && in:
		if err := s.emitIndex(ctx, q, IndexPlaceUpsert, pid, nil, nil, nil); err != nil {
			return err
		}
	case in:
		if touch.place {
			if err := s.emitIndex(ctx, q, IndexPlaceUpsert, pid, nil, nil, nil); err != nil {
				return err
			}
		}
		for _, b := range boards {
			typ := ""
			switch {
			case b.InIndex && !now[b.ID]:
				typ = IndexBoardDelete
			case !b.InIndex && now[b.ID]:
				typ = IndexBoardUpsert
			case now[b.ID] && slices.Contains(touch.boards, b.ID):
				typ = IndexBoardUpsert
			}
			if typ != "" {
				if err := s.emitIndex(ctx, q, typ, pid, &b.ID, nil, nil); err != nil {
					return err
				}
			}
		}
	}

	if err := q.SetPlaceInIndex(ctx, store.SetPlaceInIndexParams{ID: place.ID, InIndex: in}); err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(now))
	for id := range now {
		ids = append(ids, id)
	}
	return q.SetBoardsInIndex(ctx, store.SetBoardsInIndexParams{PlaceID: place.ID, Ids: ids})
}

// dropPlaceFromIndex handles a deleted place.
func (s *Service) dropPlaceFromIndex(ctx context.Context, q *store.Queries, place store.Place) error {
	if place.InIndex {
		if err := s.emitIndex(ctx, q, IndexPlaceDelete, &place.ID, nil, nil, nil); err != nil {
			return err
		}
	}
	if err := q.SetPlaceInIndex(ctx, store.SetPlaceInIndexParams{ID: place.ID, InIndex: false}); err != nil {
		return err
	}
	return q.ClearBoardsInIndex(ctx, place.ID)
}

// syncIndexListing refreshes every place whose state depends on directory listing. It runs
// when listing is switched on or off, and at startup.
func (s *Service) syncIndexListing(ctx context.Context) error {
	ids, err := s.q.ListIndexCandidatePlaceIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.tx(ctx, func(q *store.Queries) error {
			if _, err := q.LockPlace(ctx, id); errors.Is(err, pgx.ErrNoRows) {
				return nil
			} else if err != nil {
				return err
			}
			return s.refreshIndexState(ctx, q, id, indexTouch{})
		}); err != nil {
			return err
		}
	}
	return nil
}

// emitIndex records a change feed event in the caller's transaction.
func (s *Service) emitIndex(ctx context.Context, q *store.Queries, typ string, placeID, boardID, topicID, userID *uuid.UUID) error {
	var objType string
	var objID *uuid.UUID
	switch typ {
	case IndexPlaceUpsert, IndexPlaceDelete:
		objType, objID = "place", placeID
	case IndexBoardUpsert, IndexBoardDelete:
		objType, objID = "board", boardID
	case IndexTopicUpsert, IndexTopicDelete:
		objType, objID = "topic", topicID
	case IndexUserExclude, IndexUserInclude:
		objType, objID = "user", userID
	}
	if objID == nil {
		return errors.New("index event " + typ + " without its object ID")
	}
	if err := q.EmitIndexChange(ctx, store.EmitIndexChangeParams{
		ObjectType: objType, ObjectID: *objID, Type: typ,
		PlaceID: placeID, BoardID: boardID, TopicID: topicID, UserID: userID,
	}); err != nil {
		return err
	}
	s.afterCommit(ctx, q, func(context.Context) { s.wakeIndexPings() })
	return nil
}

// indexTopic records that a topic changed. It announces the topic if it is indexable now,
// or removes it if it is not and its board is (or, for a move, was) in the index. from is
// the topic's previous board when it moved.
func (s *Service) indexTopic(ctx context.Context, q *store.Queries, topicID uuid.UUID, from *uuid.UUID) error {
	t, err := q.TouchIndexTopic(ctx, topicID)
	if err != nil {
		return err
	}
	if !t.Deleted && t.BoardInIndex && t.OpeningVisible {
		return s.emitIndex(ctx, q, IndexTopicUpsert, &t.PlaceID, &t.BoardID, &t.ID, nil)
	}
	board, boardIn := t.BoardID, t.BoardInIndex
	if !boardIn && from != nil && *from != t.BoardID {
		if boardIn, err = q.BoardInIndex(ctx, *from); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		board = *from
	}
	if !boardIn {
		return nil
	}
	return s.emitIndex(ctx, q, IndexTopicDelete, &t.PlaceID, &board, &t.ID, nil)
}

// indexUser announces that a person's posts left or re-entered the index. It emits nothing
// when none of their posts are in an indexed board.
func (s *Service) indexUser(ctx context.Context, q *store.Queries, userID uuid.UUID, exclude bool) error {
	has, err := q.UserHasIndexedPosts(ctx, &userID)
	if err != nil || !has {
		return err
	}
	if err := q.TouchUserTopics(ctx, &userID); err != nil {
		return err
	}
	if exclude {
		if err := s.emitIndex(ctx, q, IndexUserExclude, nil, nil, nil, &userID); err != nil {
			return err
		}
		_, err = q.EmitUserTopicDeletes(ctx, &userID)
		return err
	}
	if err := s.emitIndex(ctx, q, IndexUserInclude, nil, nil, nil, &userID); err != nil {
		return err
	}
	_, err = q.EmitUserTopicUpserts(ctx, &userID)
	return err
}

// reindexUserTopics re-announces the indexed topics a person posted in, for example after
// their display name changed.
func (s *Service) reindexUserTopics(ctx context.Context, q *store.Queries, userID uuid.UUID) error {
	has, err := q.UserHasIndexedPosts(ctx, &userID)
	if err != nil || !has {
		return err
	}
	if err := q.TouchUserTopics(ctx, &userID); err != nil {
		return err
	}
	n, err := q.EmitUserTopicUpserts(ctx, &userID)
	if err == nil && n > 0 {
		s.afterCommit(ctx, q, func(context.Context) { s.wakeIndexPings() })
	}
	return err
}

// setIndexExclusion turns the caller's "exclude my posts from public indexes" setting on
// or off. The caller has checked that it changes.
func (s *Service) setIndexExclusion(ctx context.Context, q *store.Queries, p *Principal, exclude bool) (store.User, error) {
	user, err := q.SetUserExcludeFromIndex(ctx, store.SetUserExcludeFromIndexParams{ID: p.User.ID, Exclude: exclude})
	if err != nil {
		return user, notFound(err, "user not found")
	}
	if err := s.indexUser(ctx, q, p.User.ID, exclude); err != nil {
		return user, err
	}
	return user, s.instanceAudit(ctx, q, p, "user.index_exclusion", "user", &p.User.ID, map[string]any{"exclude": exclude})
}

// sequenceIndexFeed numbers committed, unsequenced events in the order they were recorded
// and returns the feed's high-water marks. Sequencing is serialized by the
// instance_settings row lock and only sees committed rows, so a reader can never move past
// an event that commits later with a lower number.
func sequenceIndexFeed(ctx context.Context, q *store.Queries) (store.LockIndexFeedRow, error) {
	hw, err := q.LockIndexFeed(ctx)
	if err != nil {
		return hw, err
	}
	n, err := q.SequenceIndexChanges(ctx, hw.IndexFeedSeq)
	if err != nil || n == 0 {
		return hw, err
	}
	if err := q.AdvanceIndexFeed(ctx, n); err != nil {
		return hw, err
	}
	hw.IndexFeedSeq += n
	return hw, nil
}

// IndexEvent is one change feed entry.
type IndexEvent struct {
	Seq     int64
	Type    string
	At      time.Time
	PlaceID *uuid.UUID
	BoardID *uuid.UUID
	TopicID *uuid.UUID
	UserID  *uuid.UUID
}

// IndexFeedPage is a page of the change feed. Next is the sequence number to resume after.
type IndexFeedPage struct {
	Events  []IndexEvent
	Next    int64
	HasMore bool
}

// IndexChanges reads the change feed after the given sequence number (nil starts at the
// oldest retained event). latest returns no events and the current end of the feed.
func (s *Service) IndexChanges(ctx context.Context, after *int64, latest bool, limit int32) (IndexFeedPage, error) {
	if limit <= 0 || limit > IndexMaxLimit {
		limit = IndexDefaultLimit
	}
	page := IndexFeedPage{Events: []IndexEvent{}}
	err := s.tx(ctx, func(q *store.Queries) error {
		hw, err := sequenceIndexFeed(ctx, q)
		if err != nil {
			return err
		}
		if latest {
			page.Next = hw.IndexFeedSeq
			return nil
		}
		start := int64(0)
		if after != nil {
			if *after < hw.IndexFeedPrunedSeq || *after > hw.IndexFeedSeq {
				return ErrIndexCursorExpired
			}
			start = *after
		}
		rows, err := q.ListIndexChanges(ctx, store.ListIndexChangesParams{After: start, Lim: limit + 1})
		if err != nil {
			return err
		}
		page.HasMore = len(rows) > int(limit)
		if page.HasMore {
			rows = rows[:limit]
		}
		page.Next = start
		for _, r := range rows {
			page.Events = append(page.Events, IndexEvent{
				Seq: *r.Seq, Type: r.Type, At: r.At, PlaceID: r.PlaceID, BoardID: r.BoardID, TopicID: r.TopicID, UserID: r.UserID,
			})
			page.Next = *r.Seq
		}
		if !page.HasMore {
			page.Next = max(page.Next, hw.IndexFeedSeq)
		}
		return nil
	})
	return page, err
}

// pruneIndexChanges drops events older than the retention window and records how far
// history now reaches, so older cursors are refused.
func (s *Service) pruneIndexChanges(ctx context.Context, before time.Time) error {
	return s.tx(ctx, func(q *store.Queries) error {
		if _, err := sequenceIndexFeed(ctx, q); err != nil {
			return err
		}
		pruned, err := q.PruneIndexChanges(ctx, before)
		if err != nil || pruned == 0 {
			return err
		}
		return q.SetIndexFeedPruned(ctx, pruned)
	})
}

// IndexPlace is an indexable place with its indexable boards.
type IndexPlace struct {
	Place  store.Place
	Boards []store.Board
}

// IndexPlaces lists indexable places by ID after the given one.
func (s *Service) IndexPlaces(ctx context.Context, after *uuid.UUID, limit int32) ([]IndexPlace, bool, error) {
	if limit <= 0 || limit > IndexMaxLimit {
		limit = IndexDefaultLimit
	}
	out := []IndexPlace{}
	if !s.directoryListed() {
		return out, false, nil
	}
	rows, err := s.q.ListIndexPlaces(ctx, store.ListIndexPlacesParams{After: after, Lim: limit + 1})
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > int(limit)
	if more {
		rows = rows[:limit]
	}
	for _, p := range rows {
		sc, err := s.indexScopeOf(ctx, s.q, p)
		if err != nil {
			return nil, false, err
		}
		if sc != nil {
			out = append(out, IndexPlace{Place: p, Boards: sc.boards})
		}
	}
	return out, more, nil
}

var errIndexNotFound = apperr.NotFound("not found")

// indexBoard resolves an indexable board and its place.
func (s *Service) indexBoard(ctx context.Context, boardID uuid.UUID) (store.Board, error) {
	b, err := s.q.GetBoard(ctx, boardID)
	if err != nil {
		return b, notFound(err, "not found")
	}
	place, err := s.q.GetPlaceByID(ctx, b.PlaceID)
	if err != nil {
		return b, notFound(err, "not found")
	}
	sc, err := s.indexScopeOf(ctx, s.q, place)
	if err != nil {
		return b, err
	}
	if sc == nil || !sc.ids[b.ID] {
		return b, errIndexNotFound
	}
	return b, nil
}

// IndexAuthor is the public identity of a post author: never email, IP or avatar.
type IndexAuthor struct {
	ID          uuid.UUID
	Username    string
	DisplayName string
}

func indexAuthor(id uuid.UUID, username, display string) IndexAuthor {
	if display == "" {
		display = username
	}
	return IndexAuthor{ID: id, Username: username, DisplayName: display}
}

// IndexTopic is an indexable topic's summary.
type IndexTopic struct {
	Topic      store.Topic
	Author     IndexAuthor
	ReplyCount int32
}

// IndexBoardTopics lists a board's indexable topics, ordered by index update time then ID.
func (s *Service) IndexBoardTopics(ctx context.Context, boardID uuid.UUID, since *time.Time, afterAt *time.Time, afterID *uuid.UUID, limit int32) ([]IndexTopic, bool, error) {
	if limit <= 0 || limit > IndexMaxLimit {
		limit = IndexDefaultLimit
	}
	b, err := s.indexBoard(ctx, boardID)
	if err != nil {
		return nil, false, err
	}
	out := []IndexTopic{}
	if b.Kind != "board" {
		return out, false, nil
	}
	rows, err := s.q.ListIndexBoardTopics(ctx, store.ListIndexBoardTopicsParams{
		BoardID: b.ID, Since: since, AfterAt: afterAt, AfterID: afterID, Lim: limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > int(limit)
	if more {
		rows = rows[:limit]
	}
	for _, r := range rows {
		out = append(out, IndexTopic{Topic: r.Topic, Author: indexAuthor(r.AuthorID, r.AuthorUsername, r.AuthorDisplayName), ReplyCount: r.ReplyCount})
	}
	return out, more, nil
}

// IndexPost is an indexable post.
type IndexPost struct {
	Post   store.ListIndexPostsRow
	Author IndexAuthor
}

// IndexTopicPage is an indexable topic with a page of its indexable posts.
type IndexTopicPage struct {
	IndexTopic
	Posts []IndexPost
	More  bool
}

// IndexTopicPosts returns an indexable topic and its indexable posts after a post number.
func (s *Service) IndexTopicPosts(ctx context.Context, topicID uuid.UUID, afterNumber int32, limit int32) (IndexTopicPage, error) {
	if limit <= 0 || limit > IndexPostsMaxLimit {
		limit = IndexPostsDefaultLimit
	}
	var page IndexTopicPage
	t, err := s.q.GetIndexTopic(ctx, topicID)
	if err != nil {
		return page, notFound(err, "not found")
	}
	if _, err := s.indexBoard(ctx, t.Topic.BoardID); err != nil {
		return page, err
	}
	page.IndexTopic = IndexTopic{Topic: t.Topic, Author: indexAuthor(t.AuthorID, t.AuthorUsername, t.AuthorDisplayName), ReplyCount: t.ReplyCount}
	rows, err := s.q.ListIndexPosts(ctx, store.ListIndexPostsParams{TopicID: t.Topic.ID, After: afterNumber, Lim: limit + 1})
	if err != nil {
		return page, err
	}
	page.More = len(rows) > int(limit)
	if page.More {
		rows = rows[:limit]
	}
	page.Posts = make([]IndexPost, len(rows))
	for i, r := range rows {
		page.Posts[i] = IndexPost{Post: r, Author: indexAuthor(r.AuthorID, r.AuthorUsername, r.AuthorDisplayName)}
	}
	return page, nil
}
