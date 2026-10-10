package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Feed sort orders.
const (
	FeedSortHot           = "hot"
	FeedSortNew           = "new"
	FeedSortActive        = "active"
	FeedSortTop           = "top"
	FeedSortRising        = "rising"
	FeedSortControversial = "controversial"
)

// Instance feed scopes.
const (
	FeedScopeHome = "home"
	FeedScopeAll  = "all"
)

const (
	DefaultFeedPageSize = 25
	MaxFeedPageSize     = 100
	// MaxFeedReadBatch caps the topics one POST /feed/read call may mark.
	MaxFeedReadBatch = 100
	// MaxMarkAllRead caps how many topics one "mark all as read" touches, most recent first.
	MaxMarkAllRead = 5000
	// DefaultFeedWindow applies to top and controversial when no window is given.
	DefaultFeedWindow = "week"
	maxFeedPinned     = 25
	risingWindow      = 48 * time.Hour
	maxCursorLen      = 512
)

var (
	// FeedSorts lists every sort, default first.
	FeedSorts = []string{FeedSortHot, FeedSortNew, FeedSortActive, FeedSortTop, FeedSortRising, FeedSortControversial}
	// FeedWindows lists the time windows of the top and controversial sorts.
	FeedWindows   = []string{"hour", "day", "week", "month", "year", "all"}
	windowLengths = map[string]time.Duration{
		"hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour,
		"month": 30 * 24 * time.Hour, "year": 365 * 24 * time.Hour, "all": 0,
	}
)

// FeedQuery selects a page of a feed.
type FeedQuery struct {
	Sort   string
	Window string
	// BoardID narrows a place feed to a board or category and everything below it.
	BoardID         *uuid.UUID
	Tag             string
	Solved          *bool
	HideRead        bool
	NSFW            bool
	IncludeArchived bool
	// PinnedFirst returns the place's pinned topics separately on the first page and leaves
	// them out of the ranked items.
	PinnedFirst bool
	Limit       int32
	Cursor      string
}

func (fq *FeedQuery) normalize() error {
	if fq.Sort == "" {
		fq.Sort = FeedSortHot
	}
	if !slices.Contains(FeedSorts, fq.Sort) {
		return apperr.Invalid("sort must be one of %v", FeedSorts)
	}
	if fq.Sort == FeedSortTop || fq.Sort == FeedSortControversial {
		if fq.Window == "" {
			fq.Window = DefaultFeedWindow
		}
		if _, ok := windowLengths[fq.Window]; !ok {
			return apperr.Invalid("t must be one of %v", FeedWindows)
		}
	} else {
		fq.Window = ""
	}
	if fq.Limit <= 0 {
		fq.Limit = DefaultFeedPageSize
	}
	fq.Limit = min(fq.Limit, MaxFeedPageSize)
	return nil
}

// FeedItem is a topic as shown in a feed.
type FeedItem struct {
	Topic   TopicView
	Board   store.Board
	Place   store.Place
	Excerpt string
	// Images are the opening post's image attachments (at most feedImages); ImageCount
	// counts all of them.
	Images     []store.Upload
	ImageCount int
	// Embed is the first link preview of the opening post, if any.
	Embed *LinkPreview
	// IsNSFW is set when the board, one of its parents or the place is marked NSFW.
	IsNSFW bool
}

// feedImages is how many of an opening post's images a feed item carries.
const feedImages = 4

type FeedPage struct {
	Items []FeedItem
	// Pinned is only set on the first page of a place feed with PinnedFirst.
	Pinned     []FeedItem
	NextCursor string
	// AsOf is when the first page was loaded; later pages and "mark all as read" use it.
	AsOf   time.Time
	Sort   string
	Window string
}

// feedCursor is the position after the last item of a page. It is opaque to clients.
type feedCursor struct {
	Sort   string     `json:"s"`
	Window string     `json:"w,omitempty"`
	AsOf   time.Time  `json:"a"`
	ID     uuid.UUID  `json:"i"`
	Rank   *float64   `json:"r,omitempty"`
	Time   *time.Time `json:"t,omitempty"`
	Score  *int32     `json:"n,omitempty"`
}

func encodeCursor(c feedCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(raw string, fq FeedQuery) (*feedCursor, error) {
	if raw == "" {
		return nil, nil
	}
	bad := apperr.Invalid("cursor is invalid or belongs to another sort")
	if len(raw) > maxCursorLen {
		return nil, bad
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, bad
	}
	var c feedCursor
	if json.Unmarshal(b, &c) != nil || c.Sort != fq.Sort || c.Window != fq.Window || c.ID == uuid.Nil || c.AsOf.IsZero() {
		return nil, bad
	}
	switch fq.Sort {
	case FeedSortNew, FeedSortActive:
		if c.Time == nil {
			return nil, bad
		}
	case FeedSortTop:
		if c.Score == nil {
			return nil, bad
		}
	default:
		if c.Rank == nil {
			return nil, bad
		}
	}
	return &c, nil
}

// feedSource is the set of boards a feed draws from, resolved for the caller.
type feedSource struct {
	placeID    *uuid.UUID
	boardIDs   []uuid.UUID
	mutedIDs   []uuid.UUID
	applyMutes bool
	pinned     bool
	boards     map[uuid.UUID]store.Board
	nsfw       map[uuid.UUID]bool
	places     map[uuid.UUID]store.Place
}

func newFeedSource() feedSource {
	return feedSource{
		boardIDs: []uuid.UUID{}, mutedIDs: []uuid.UUID{},
		boards: map[uuid.UUID]store.Board{}, nsfw: map[uuid.UUID]bool{}, places: map[uuid.UUID]store.Place{},
	}
}

// nsfw reports whether a board or any of its parents is marked NSFW.
func (f *forumScope) nsfw(boardID uuid.UUID) bool {
	for _, b := range f.chain(boardID) {
		if b.IsNsfw {
			return true
		}
	}
	return false
}

// muted resolves the caller's notification level for a board the way notifications do:
// the most specific preference (the board, then its parents, then the place) wins.
func (f *forumScope) muted(boardID uuid.UUID, levels map[uuid.UUID]string) bool {
	chain := f.chain(boardID)
	for i := len(chain) - 1; i >= 0; i-- {
		if lvl, ok := levels[chain[i].ID]; ok {
			return lvl == "muted"
		}
	}
	return levels[f.place.ID] == "muted"
}

// PlaceFeed ranks the topics of every board in a place the caller can read.
func (s *Service) PlaceFeed(ctx context.Context, p *Principal, ref string, fq FeedQuery) (FeedPage, error) {
	if err := fq.normalize(); err != nil {
		return FeedPage{}, err
	}
	f, err := s.forum(ctx, s.q, p, ref)
	if err != nil {
		return FeedPage{}, err
	}
	if fq.Sort == FeedSortControversial && !f.place.VotingEnabled {
		return FeedPage{}, apperr.Invalid("this place has voting turned off, so it has no controversial sort")
	}
	var root *uuid.UUID
	if fq.BoardID != nil {
		b, err := f.board(*fq.BoardID)
		if err != nil {
			return FeedPage{}, err
		}
		root = &b.ID
	}
	src := newFeedSource()
	src.placeID, src.pinned = &f.place.ID, fq.PinnedFirst
	src.places[f.place.ID] = f.place
	// The place's own NSFW flag does not hide its content here: the caller chose to open it.
	for _, id := range f.visibleBoardIDs() {
		if root != nil && !f.isDescendant(id, *root) {
			continue
		}
		nsfw := f.nsfw(id)
		if nsfw && !fq.NSFW {
			continue
		}
		src.boardIDs = append(src.boardIDs, id)
		src.boards[id], src.nsfw[id] = f.boards[id], nsfw || f.place.IsNsfw
	}
	return s.runFeed(ctx, p, src, fq)
}

// InstanceFeed ranks topics across places: the caller's joined places (home, the default
// when signed in) or public content on the whole instance (all, the default otherwise).
func (s *Service) InstanceFeed(ctx context.Context, p *Principal, scope string, fq FeedQuery) (FeedPage, error) {
	if err := fq.normalize(); err != nil {
		return FeedPage{}, err
	}
	if fq.BoardID != nil || fq.PinnedFirst {
		return FeedPage{}, apperr.Invalid("board and pinned filters only apply to place feeds")
	}
	if scope == "" {
		scope = FeedScopeAll
		if p != nil {
			scope = FeedScopeHome
		}
	}
	var (
		src feedSource
		err error
	)
	switch scope {
	case FeedScopeHome:
		if p == nil {
			return FeedPage{}, apperr.Unauthorized("sign in to see your home feed")
		}
		src, err = s.homeFeedSource(ctx, p, fq.NSFW)
	case FeedScopeAll:
		src, err = s.publicFeedSource(ctx, fq.NSFW)
	default:
		return FeedPage{}, apperr.Invalid("scope must be home or all")
	}
	if err != nil {
		return FeedPage{}, err
	}
	return s.runFeed(ctx, p, src, fq)
}

// homeFeedSource resolves the boards the caller can read in every place they belong to,
// loading all places' boards, overwrites and roles in a handful of queries.
func (s *Service) homeFeedSource(ctx context.Context, p *Principal, includeNSFW bool) (feedSource, error) {
	src := newFeedSource()
	src.applyMutes = true
	places, err := s.q.ListUserPlaces(ctx, p.User.ID)
	if err != nil || len(places) == 0 {
		return src, err
	}
	ids := make([]uuid.UUID, len(places))
	for i, pl := range places {
		ids[i] = pl.ID
	}
	boards, err := s.q.ListBoardsForPlaces(ctx, ids)
	if err != nil {
		return src, err
	}
	overwrites, err := s.q.ListBoardOverwritesForPlaces(ctx, ids)
	if err != nil {
		return src, err
	}
	roles, err := s.q.ListMemberRolesForPlaces(ctx, store.ListMemberRolesForPlacesParams{UserID: p.User.ID, PlaceIds: ids})
	if err != nil {
		return src, err
	}
	subs, err := s.q.ListForumMutes(ctx, store.ListForumMutesParams{UserID: p.User.ID, PlaceIds: ids})
	if err != nil {
		return src, err
	}
	levels := make(map[uuid.UUID]string, len(subs))
	for _, sub := range subs {
		levels[sub.TargetID] = sub.Level
	}
	boardsBy := map[uuid.UUID][]store.Board{}
	for _, b := range boards {
		boardsBy[b.PlaceID] = append(boardsBy[b.PlaceID], b)
	}
	owsBy := map[uuid.UUID][]store.BoardOverwrite{}
	for _, o := range overwrites {
		owsBy[o.PlaceID] = append(owsBy[o.PlaceID], o)
	}
	rolesBy := map[uuid.UUID][]store.Role{}
	for _, r := range roles {
		rolesBy[r.PlaceID] = append(rolesBy[r.PlaceID], r)
	}

	for _, place := range places {
		f := &forumScope{place: place, boards: map[uuid.UUID]store.Board{}, overwrites: map[uuid.UUID][]permissions.Overwrite{}}
		for _, b := range boardsBy[place.ID] {
			f.boards[b.ID] = b
		}
		f.ordered = treeOrder(boardsBy[place.ID])
		for _, o := range owsBy[place.ID] {
			f.overwrites[o.BoardID] = append(f.overwrites[o.BoardID], permissions.Overwrite{
				RoleID: o.RoleID, Allow: permissions.Permission(o.Allow), Deny: permissions.Permission(o.Deny),
			})
		}
		m := permissions.Member{IsOwner: place.OwnerID == p.User.ID}
		for _, r := range rolesBy[place.ID] {
			m.Raw |= permissions.Permission(r.Permissions)
			m.TopPosition = max(m.TopPosition, r.Position)
			m.RoleIDs = append(m.RoleIDs, r.ID)
			if r.IsDefault {
				m.DefaultRoleID, f.defaultRole = r.ID, r
			}
		}
		f.acc, f.viewer = Access{IsMember: true, Member: m}, m
		src.places[place.ID] = place
		for _, id := range f.visibleBoardIDs() {
			nsfw := place.IsNsfw || f.nsfw(id)
			if nsfw && !includeNSFW {
				continue
			}
			src.boardIDs = append(src.boardIDs, id)
			src.boards[id], src.nsfw[id] = f.boards[id], nsfw
			if f.muted(id, levels) {
				src.mutedIDs = append(src.mutedIDs, id)
			}
		}
	}
	return src, nil
}

// publicFeedSource uses the boards signed-out visitors can read, like instance-wide search.
func (s *Service) publicFeedSource(ctx context.Context, includeNSFW bool) (feedSource, error) {
	src := newFeedSource()
	rows, err := s.q.ListPublicFeedBoards(ctx)
	if err != nil {
		return src, err
	}
	for _, r := range rows {
		if r.Nsfw && !includeNSFW {
			continue
		}
		src.boardIDs = append(src.boardIDs, r.Board.ID)
		src.boards[r.Board.ID], src.nsfw[r.Board.ID] = r.Board, r.Nsfw
	}
	return src, nil
}

// feedFilter holds the parameters every feed query shares.
type feedFilter struct {
	boardIDs        []uuid.UUID
	placeID         *uuid.UUID
	includeArchived bool
	excludePinned   bool
	tag             *string
	solved          *bool
	since           *time.Time
	hideRead        bool
	viewerID        uuid.UUID
	applyMutes      bool
	mutedIDs        []uuid.UUID
}

func (s *Service) runFeed(ctx context.Context, p *Principal, src feedSource, fq FeedQuery) (FeedPage, error) {
	tag, err := optionalTag(fq.Tag)
	if err != nil {
		return FeedPage{}, err
	}
	cur, err := decodeCursor(fq.Cursor, fq)
	if err != nil {
		return FeedPage{}, err
	}
	asOf := time.Now().UTC().Truncate(time.Microsecond)
	if cur != nil {
		asOf = cur.AsOf
	}
	page := FeedPage{Items: []FeedItem{}, AsOf: asOf, Sort: fq.Sort, Window: fq.Window}
	if len(src.boardIDs) == 0 {
		return page, nil
	}

	ff := feedFilter{
		boardIDs: src.boardIDs, placeID: src.placeID, includeArchived: fq.IncludeArchived, tag: tag,
		solved: fq.Solved, applyMutes: src.applyMutes && p != nil, mutedIDs: src.mutedIDs,
	}
	if p != nil {
		ff.viewerID, ff.hideRead = p.User.ID, fq.HideRead
	}
	switch fq.Sort {
	case FeedSortTop, FeedSortControversial:
		if d := windowLengths[fq.Window]; d > 0 {
			since := asOf.Add(-d)
			ff.since = &since
		}
	case FeedSortRising:
		since := asOf.Add(-risingWindow)
		ff.since = &since
	}

	var pinned []store.Topic
	if src.pinned {
		ff.excludePinned = true
		if cur == nil {
			if pinned, err = s.q.ListFeedPinned(ctx, store.ListFeedPinnedParams{
				BoardIds: ff.boardIDs, PlaceID: ff.placeID, IncludeArchived: ff.includeArchived, Tag: ff.tag,
				Solved: ff.solved, HideRead: ff.hideRead, ViewerID: ff.viewerID, ApplyMutes: ff.applyMutes,
				MutedBoardIds: ff.mutedIDs, Lim: maxFeedPinned,
			}); err != nil {
				return FeedPage{}, err
			}
		}
	}

	topics, next, err := s.queryFeed(ctx, ff, fq, cur, asOf)
	if err != nil {
		return FeedPage{}, err
	}
	page.NextCursor = next
	items, err := s.feedItems(ctx, p, src, append(pinned, topics...))
	if err != nil {
		return FeedPage{}, err
	}
	if src.pinned && cur == nil {
		page.Pinned = items[:len(pinned)]
	}
	page.Items = items[len(pinned):]
	return page, nil
}

// queryFeed runs the query for the sort and returns a page plus the cursor to the next one.
// It asks for one extra row to know whether another page exists.
func (s *Service) queryFeed(ctx context.Context, ff feedFilter, fq FeedQuery, cur *feedCursor, asOf time.Time) ([]store.Topic, string, error) {
	lim := fq.Limit + 1
	var (
		afterID   *uuid.UUID
		afterRank *float64
		afterTime *time.Time
		afterNum  *int32
	)
	if cur != nil {
		afterID, afterRank, afterTime, afterNum = &cur.ID, cur.Rank, cur.Time, cur.Score
	}
	var (
		topics []store.Topic
		ranks  []float64
		err    error
	)
	switch fq.Sort {
	case FeedSortHot, FeedSortControversial:
		arg := store.ListFeedHotParams{
			BoardIds: ff.boardIDs, PlaceID: ff.placeID, IncludeArchived: ff.includeArchived, ExcludePinned: ff.excludePinned,
			Tag: ff.tag, Solved: ff.solved, Since: ff.since, HideRead: ff.hideRead, ViewerID: ff.viewerID,
			ApplyMutes: ff.applyMutes, MutedBoardIds: ff.mutedIDs, AfterID: afterID, AfterRank: afterRank, Lim: lim,
		}
		if fq.Sort == FeedSortHot {
			topics, err = s.q.ListFeedHot(ctx, arg)
		} else {
			topics, err = s.q.ListFeedControversial(ctx, store.ListFeedControversialParams(arg))
		}
	case FeedSortNew, FeedSortActive:
		arg := store.ListFeedNewParams{
			BoardIds: ff.boardIDs, PlaceID: ff.placeID, IncludeArchived: ff.includeArchived, ExcludePinned: ff.excludePinned,
			Tag: ff.tag, Solved: ff.solved, Since: ff.since, HideRead: ff.hideRead, ViewerID: ff.viewerID,
			ApplyMutes: ff.applyMutes, MutedBoardIds: ff.mutedIDs, AfterID: afterID, AfterTime: afterTime, Lim: lim,
		}
		if fq.Sort == FeedSortNew {
			topics, err = s.q.ListFeedNew(ctx, arg)
		} else {
			topics, err = s.q.ListFeedActive(ctx, store.ListFeedActiveParams(arg))
		}
	case FeedSortTop:
		topics, err = s.q.ListFeedTop(ctx, store.ListFeedTopParams{
			BoardIds: ff.boardIDs, PlaceID: ff.placeID, IncludeArchived: ff.includeArchived, ExcludePinned: ff.excludePinned,
			Tag: ff.tag, Solved: ff.solved, Since: ff.since, HideRead: ff.hideRead, ViewerID: ff.viewerID,
			ApplyMutes: ff.applyMutes, MutedBoardIds: ff.mutedIDs, AfterID: afterID, AfterScore: afterNum, Lim: lim,
		})
	case FeedSortRising:
		var rows []store.ListFeedRisingRow
		rows, err = s.q.ListFeedRising(ctx, store.ListFeedRisingParams{
			BoardIds: ff.boardIDs, PlaceID: ff.placeID, IncludeArchived: ff.includeArchived, ExcludePinned: ff.excludePinned,
			Tag: ff.tag, Solved: ff.solved, Since: ff.since, HideRead: ff.hideRead, ViewerID: ff.viewerID,
			ApplyMutes: ff.applyMutes, MutedBoardIds: ff.mutedIDs, AfterID: afterID, AfterRank: afterRank, Lim: lim,
			AsOf: asOf,
		})
		for _, r := range rows {
			topics, ranks = append(topics, r.Topic), append(ranks, r.Rank)
		}
	}
	if err != nil {
		return nil, "", err
	}
	if len(topics) <= int(fq.Limit) {
		return topics, "", nil
	}
	topics = topics[:fq.Limit]
	last := topics[len(topics)-1]
	c := feedCursor{Sort: fq.Sort, Window: fq.Window, AsOf: asOf, ID: last.ID}
	switch fq.Sort {
	case FeedSortHot:
		c.Rank = &last.HotRank
	case FeedSortControversial:
		c.Rank = &last.Controversy
	case FeedSortNew:
		c.Time = &last.CreatedAt
	case FeedSortActive:
		c.Time = &last.LastPostAt
	case FeedSortTop:
		c.Score = &last.Score
	case FeedSortRising:
		c.Rank = &ranks[len(topics)-1]
	}
	return topics, encodeCursor(c), nil
}

// feedItems adds authors, the caller's state, boards, places and excerpts to topics.
func (s *Service) feedItems(ctx context.Context, p *Principal, src feedSource, topics []store.Topic) ([]FeedItem, error) {
	if len(topics) == 0 {
		return []FeedItem{}, nil
	}
	views, err := s.topicViews(ctx, s.q, p, topics)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(topics))
	var missing []uuid.UUID
	for i, t := range topics {
		ids[i] = t.ID
		if _, ok := src.places[t.PlaceID]; !ok {
			missing = append(missing, t.PlaceID)
		}
	}
	if len(missing) > 0 {
		places, err := s.q.ListPlacesByIDs(ctx, dedupe(missing))
		if err != nil {
			return nil, err
		}
		for _, pl := range places {
			src.places[pl.ID] = pl
		}
	}
	posts, err := s.q.ListOpeningPosts(ctx, ids)
	if err != nil {
		return nil, err
	}
	excerpts := make(map[uuid.UUID]string, len(posts))
	postIDs := make([]uuid.UUID, len(posts))
	contents := make([]string, len(posts))
	topicOf := make(map[uuid.UUID]uuid.UUID, len(posts))
	for i, post := range posts {
		excerpts[post.TopicID] = plainExcerpt(post.Content, ExcerptLength)
		postIDs[i], contents[i], topicOf[post.ID] = post.ID, post.Content, post.TopicID
	}
	images := map[uuid.UUID][]store.Upload{}
	imageCounts := map[uuid.UUID]int{}
	if len(postIDs) > 0 {
		atts, err := s.q.ListPostAttachments(ctx, postIDs)
		if err != nil {
			return nil, err
		}
		for _, a := range atts {
			if !IsImage(a.Upload) {
				continue
			}
			t := topicOf[a.PostID]
			imageCounts[t]++
			if len(images[t]) < feedImages {
				images[t] = append(images[t], a.Upload)
			}
		}
	}
	previews, err := s.linkPreviews(ctx, s.q, contents)
	if err != nil {
		return nil, err
	}
	embeds := map[uuid.UUID]*LinkPreview{}
	for i, ps := range previews {
		if len(ps) > 0 {
			embeds[posts[i].TopicID] = &ps[0]
		}
	}
	items := make([]FeedItem, len(views))
	for i, v := range views {
		items[i] = FeedItem{
			Topic: v, Board: src.boards[v.Topic.BoardID], Place: src.places[v.Topic.PlaceID],
			Excerpt: excerpts[v.Topic.ID], IsNSFW: src.nsfw[v.Topic.BoardID],
			Images: images[v.Topic.ID], ImageCount: imageCounts[v.Topic.ID], Embed: embeds[v.Topic.ID],
		}
	}
	return items, nil
}

// refreshRanks recomputes the feed ranking of topics after their votes, replies or opening
// post reactions change.
func (s *Service) refreshRanks(ctx context.Context, q *store.Queries, topicIDs ...uuid.UUID) error {
	if len(topicIDs) == 0 {
		return nil
	}
	return q.RefreshTopicRanks(ctx, store.RefreshTopicRanksParams{TopicIds: topicIDs})
}

// refreshOpeningPostRank re-ranks a topic when reactions on its opening post change; places
// with voting turned off rank topics by those reactions.
func (s *Service) refreshOpeningPostRank(ctx context.Context, q *store.Queries, post store.Post) error {
	if post.PostNumber != 1 {
		return nil
	}
	return s.refreshRanks(ctx, q, post.TopicID)
}

func voteDelta(prev, next int16) (up, down int32) {
	b := func(v bool) int32 {
		if v {
			return 1
		}
		return 0
	}
	return b(next == 1) - b(prev == 1), b(next == -1) - b(prev == -1)
}

// SetTopicVote votes a topic up (1) or down (-1), replacing any earlier vote. Voting needs
// ADD_REACTIONS in the board; people cannot vote on their own topics.
func (s *Service) SetTopicVote(ctx context.Context, p *Principal, topicID uuid.UUID, value int16) (TopicView, error) {
	if value != 1 && value != -1 {
		return TopicView{}, apperr.Invalid("value must be 1 or -1")
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
		if !f.place.VotingEnabled {
			return apperr.Conflict("voting is turned off in this place")
		}
		if err := f.requireParticipant(topic.BoardID, permissions.AddReactions); err != nil {
			return err
		}
		if topic.IsArchived {
			return apperr.Conflict("this topic is archived")
		}
		if isAuthor(topic.AuthorID, p) {
			return apperr.Forbidden("you cannot vote on your own topic")
		}
		prev, err := q.GetTopicVote(ctx, store.GetTopicVoteParams{TopicID: topic.ID, UserID: p.User.ID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if prev != value {
			if err := q.UpsertTopicVote(ctx, store.UpsertTopicVoteParams{TopicID: topic.ID, UserID: p.User.ID, Value: value}); err != nil {
				return err
			}
			if err := s.applyVote(ctx, q, topic.ID, prev, value); err != nil {
				return err
			}
		}
		view, err = s.reloadTopicView(ctx, q, p, topic.ID)
		return err
	})
	return view, err
}

// RemoveTopicVote withdraws the caller's vote. It is allowed even when voting is off.
func (s *Service) RemoveTopicVote(ctx context.Context, p *Principal, topicID uuid.UUID) (TopicView, error) {
	var view TopicView
	err := s.tx(ctx, func(q *store.Queries) error {
		_, topic, err := s.topicScope(ctx, q, p, topicID)
		if err != nil {
			return err
		}
		if topic, err = q.GetTopicForUpdate(ctx, topic.ID); err != nil {
			return err
		}
		prev, err := q.DeleteTopicVote(ctx, store.DeleteTopicVoteParams{TopicID: topic.ID, UserID: p.User.ID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if err := s.applyVote(ctx, q, topic.ID, prev, 0); err != nil {
				return err
			}
		}
		view, err = s.reloadTopicView(ctx, q, p, topic.ID)
		return err
	})
	return view, err
}

func (s *Service) applyVote(ctx context.Context, q *store.Queries, topicID uuid.UUID, prev, next int16) error {
	up, down := voteDelta(prev, next)
	if err := q.AdjustTopicVotes(ctx, store.AdjustTopicVotesParams{ID: topicID, UpDelta: up, DownDelta: down}); err != nil {
		return err
	}
	return s.refreshRanks(ctx, q, topicID)
}

func (s *Service) reloadTopicView(ctx context.Context, q *store.Queries, p *Principal, topicID uuid.UUID) (TopicView, error) {
	t, err := q.GetTopic(ctx, topicID)
	if err != nil {
		return TopicView{}, err
	}
	return s.topicView(ctx, q, p, t)
}

// TopicReadState is the caller's read state of one topic, as sent to their other sessions.
type TopicReadState struct {
	TopicID            uuid.UUID
	PlaceID            uuid.UUID
	Read               bool
	HasNewReplies      bool
	NewReplyCount      int32
	LastReadPostNumber *int32
	UnreadCount        int32
}

func readStateOf(t store.Topic, r *store.TopicRead) TopicReadState {
	st := TopicReadState{TopicID: t.ID, PlaceID: t.PlaceID, UnreadCount: t.LastPostNumber}
	if r != nil {
		n := r.LastReadPostNumber
		st.LastReadPostNumber = &n
		st.UnreadCount = max(t.LastPostNumber-n, 0)
		st.Read = r.OpenedAt != nil
		st.HasNewReplies = st.Read && t.LastPostNumber > r.SeenPostNumber
		if st.HasNewReplies {
			st.NewReplyCount = t.LastPostNumber - r.SeenPostNumber
		}
	}
	return st
}

func (st TopicReadState) data() map[string]any {
	return map[string]any{
		"topic_id": st.TopicID, "place_id": st.PlaceID, "read": st.Read, "has_new_replies": st.HasNewReplies,
		"new_reply_count":       st.NewReplyCount,
		"last_read_post_number": st.LastReadPostNumber, "unread_count": st.UnreadCount,
	}
}

// emitReadStates tells the user's sessions about read state changes so other devices can
// restyle the topics.
func (s *Service) emitReadStates(ctx context.Context, q *store.Queries, userID uuid.UUID, states []TopicReadState) {
	if len(states) == 0 {
		return
	}
	topics := make([]map[string]any, len(states))
	for i, st := range states {
		topics[i] = st.data()
	}
	s.emit(ctx, q, Event{Type: EventTopicReadStateUpdate, Users: []uuid.UUID{userID}, Data: map[string]any{"topics": topics}})
}

// MarkTopicUnread clears the "read" mark of a topic the caller opened. The read position
// is kept.
func (s *Service) MarkTopicUnread(ctx context.Context, p *Principal, topicID uuid.UUID) (TopicReadState, error) {
	_, topic, err := s.topicScope(ctx, s.q, p, topicID)
	if err != nil {
		return TopicReadState{}, err
	}
	if _, err := s.q.MarkTopicUnread(ctx, store.MarkTopicUnreadParams{UserID: p.User.ID, TopicID: topic.ID}); err != nil {
		return TopicReadState{}, err
	}
	states, err := s.readStates(ctx, s.q, p.User.ID, []store.Topic{topic})
	if err != nil {
		return TopicReadState{}, err
	}
	s.emitReadStates(ctx, s.q, p.User.ID, states)
	return states[0], nil
}

func (s *Service) readStates(ctx context.Context, q *store.Queries, userID uuid.UUID, topics []store.Topic) ([]TopicReadState, error) {
	ids := make([]uuid.UUID, len(topics))
	for i, t := range topics {
		ids[i] = t.ID
	}
	rows, err := q.ListTopicReads(ctx, store.ListTopicReadsParams{UserID: userID, TopicIds: ids})
	if err != nil {
		return nil, err
	}
	reads := make(map[uuid.UUID]store.TopicRead, len(rows))
	for _, r := range rows {
		reads[r.TopicID] = store.TopicRead{
			TopicID: r.TopicID, LastReadPostNumber: r.LastReadPostNumber, OpenedAt: r.OpenedAt, SeenPostNumber: r.SeenPostNumber,
		}
	}
	out := make([]TopicReadState, len(topics))
	for i, t := range topics {
		var r *store.TopicRead
		if row, ok := reads[t.ID]; ok {
			r = &row
		}
		out[i] = readStateOf(t, r)
	}
	return out, nil
}

// MarkTopicsRead records opens of several topics, for clients replaying opens made offline
// or importing what someone read while signed out. Topics that no longer exist or that the
// caller cannot see are skipped rather than failing the batch.
func (s *Service) MarkTopicsRead(ctx context.Context, p *Principal, topicIDs []uuid.UUID) ([]TopicReadState, error) {
	topicIDs = dedupe(topicIDs)
	if len(topicIDs) > MaxFeedReadBatch {
		return nil, apperr.Invalid("at most %d topics can be marked at once", MaxFeedReadBatch)
	}
	if len(topicIDs) == 0 {
		return []TopicReadState{}, nil
	}
	topics, err := s.q.ListTopicsByIDs(ctx, topicIDs)
	if err != nil {
		return nil, err
	}
	scopes := map[uuid.UUID]*forumScope{}
	var allowed []store.Topic
	for _, t := range topics {
		if t.DeletedAt != nil {
			continue
		}
		f, seen := scopes[t.PlaceID]
		if !seen {
			if f, err = s.forum(ctx, s.q, p, t.PlaceID.String()); err != nil {
				var ae *apperr.Error
				if !errors.As(err, &ae) {
					return nil, err
				}
				f = nil
			}
			scopes[t.PlaceID] = f
		}
		if f != nil && f.canView(t.BoardID) {
			allowed = append(allowed, t)
		}
	}
	if len(allowed) == 0 {
		return []TopicReadState{}, nil
	}
	ids := make([]uuid.UUID, len(allowed))
	for i, t := range allowed {
		ids[i] = t.ID
	}
	if _, err := s.q.MarkTopicsOpened(ctx, store.MarkTopicsOpenedParams{UserID: p.User.ID, TopicIds: ids}); err != nil {
		return nil, err
	}
	states, err := s.readStates(ctx, s.q, p.User.ID, allowed)
	if err != nil {
		return nil, err
	}
	s.emitReadStates(ctx, s.q, p.User.ID, states)
	return states, nil
}

// MarkFeedRead marks every topic the caller can read in a place (or one board and its
// children) as fully read, skipping topics with activity after before so posts the caller
// has not seen stay unread. At most MaxMarkAllRead topics, most recently active first.
func (s *Service) MarkFeedRead(ctx context.Context, p *Principal, ref string, boardID *uuid.UUID, before *time.Time) (int64, error) {
	f, err := s.forum(ctx, s.q, p, ref)
	if err != nil {
		return 0, err
	}
	var root *uuid.UUID
	if boardID != nil {
		b, err := f.board(*boardID)
		if err != nil {
			return 0, err
		}
		root = &b.ID
	}
	ids := []uuid.UUID{}
	for _, id := range f.visibleBoardIDs() {
		if root == nil || f.isDescendant(id, *root) {
			ids = append(ids, id)
		}
	}
	cutoff := time.Now()
	if before != nil && before.Before(cutoff) {
		cutoff = *before
	}
	n, err := s.q.MarkFeedRead(ctx, store.MarkFeedReadParams{UserID: p.User.ID, BoardIds: ids, Before: cutoff, Lim: MaxMarkAllRead})
	if err != nil {
		return 0, err
	}
	s.emit(ctx, s.q, Event{Type: EventTopicReadStateUpdate, Users: []uuid.UUID{p.User.ID}, Data: map[string]any{
		"topics": []map[string]any{}, "all": true, "place_id": f.place.ID, "board_id": root, "before": cutoff,
	}})
	return n, nil
}
