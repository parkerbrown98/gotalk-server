package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const (
	maxBoardsPerPlace = 200
	// maxBoardDepth allows category > board > sub-board.
	maxBoardDepth = 3
)

// forumScope is a place's board tree plus the caller's standing, loaded once per request
// so per-board permissions can be evaluated in memory.
type forumScope struct {
	place       store.Place
	acc         Access
	viewer      permissions.Member
	defaultRole store.Role
	boards      map[uuid.UUID]store.Board
	ordered     []store.Board
	overwrites  map[uuid.UUID][]permissions.Overwrite
}

func (s *Service) loadBoards(ctx context.Context, q *store.Queries, place store.Place) (*forumScope, error) {
	def, err := q.GetDefaultRole(ctx, place.ID)
	if err != nil {
		return nil, err
	}
	boards, err := q.ListBoards(ctx, place.ID)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListBoardOverwrites(ctx, place.ID)
	if err != nil {
		return nil, err
	}
	f := &forumScope{
		place:       place,
		defaultRole: def,
		boards:      make(map[uuid.UUID]store.Board, len(boards)),
		overwrites:  map[uuid.UUID][]permissions.Overwrite{},
	}
	for _, b := range boards {
		f.boards[b.ID] = b
	}
	f.ordered = treeOrder(boards)
	for _, o := range rows {
		f.overwrites[o.BoardID] = append(f.overwrites[o.BoardID], permissions.Overwrite{
			RoleID: o.RoleID, Allow: permissions.Permission(o.Allow), Deny: permissions.Permission(o.Deny),
		})
	}
	return f, nil
}

// treeOrder sorts boards depth-first: each board is followed by its children, siblings by
// position. boards must already be sorted by position.
func treeOrder(boards []store.Board) []store.Board {
	children := map[uuid.UUID][]store.Board{}
	var roots []store.Board
	for _, b := range boards {
		if b.ParentID == nil {
			roots = append(roots, b)
		} else {
			children[*b.ParentID] = append(children[*b.ParentID], b)
		}
	}
	out := make([]store.Board, 0, len(boards))
	var walk func([]store.Board)
	walk = func(level []store.Board) {
		for _, b := range level {
			out = append(out, b)
			walk(children[b.ID])
		}
	}
	walk(roots)
	return out
}

// forum resolves a place and the caller's standing in it. Members use their roles;
// anyone may read public places as a guest; other places are members-only.
func (s *Service) forum(ctx context.Context, q *store.Queries, p *Principal, ref string) (*forumScope, error) {
	place, acc, err := s.placeFor(ctx, q, p, ref)
	if err != nil {
		return nil, err
	}
	if !acc.IsMember && place.Visibility != "public" {
		return nil, apperr.Forbidden("you must be a member of this place to view its boards")
	}
	f, err := s.loadBoards(ctx, q, place)
	if err != nil {
		return nil, err
	}
	f.acc = acc
	if acc.IsMember {
		f.viewer = acc.Member
	} else {
		f.viewer = f.guest()
	}
	return f, nil
}

func (f *forumScope) guest() permissions.Member {
	return permissions.Member{
		Raw:           permissions.Permission(f.defaultRole.Permissions),
		DefaultRoleID: f.defaultRole.ID,
		RoleIDs:       []uuid.UUID{f.defaultRole.ID},
		Guest:         true,
	}
}

// everyone is a member holding only the @everyone role; what it can see counts as public
// within the place (and may be sent to webhooks).
func (f *forumScope) everyone() permissions.Member {
	m := f.guest()
	m.Guest = false
	return m
}

// chain returns the board's ancestry, root first, or nil if the board is unknown.
func (f *forumScope) chain(boardID uuid.UUID) []store.Board {
	var rev []store.Board
	id := &boardID
	for id != nil && len(rev) <= maxBoardDepth*2 {
		b, ok := f.boards[*id]
		if !ok {
			return nil
		}
		rev = append(rev, b)
		id = b.ParentID
	}
	slices.Reverse(rev)
	return rev
}

func (f *forumScope) levels(chain []store.Board) [][]permissions.Overwrite {
	out := make([][]permissions.Overwrite, len(chain))
	for i, b := range chain {
		out[i] = f.overwrites[b.ID]
	}
	return out
}

func (f *forumScope) permsFor(m permissions.Member, boardID uuid.UUID) permissions.Permission {
	chain := f.chain(boardID)
	if chain == nil {
		return 0
	}
	return m.InScope(f.levels(chain)...)
}

// perms returns the caller's permissions within a board.
func (f *forumScope) perms(boardID uuid.UUID) permissions.Permission {
	return f.permsFor(f.viewer, boardID)
}

func (f *forumScope) has(boardID uuid.UUID, perm permissions.Permission) bool {
	return f.perms(boardID)&perm == perm
}

// visibleTo reports whether m can see the board and every ancestor.
func (f *forumScope) visibleTo(m permissions.Member, boardID uuid.UUID) bool {
	chain := f.chain(boardID)
	if chain == nil {
		return false
	}
	levels := f.levels(chain)
	for i := range chain {
		if m.InScope(levels[:i+1]...)&permissions.ViewBoards == 0 {
			return false
		}
	}
	return true
}

func (f *forumScope) canView(boardID uuid.UUID) bool { return f.visibleTo(f.viewer, boardID) }

// visibleBoardIDs lists boards (not categories) the caller can read.
func (f *forumScope) visibleBoardIDs() []uuid.UUID {
	ids := []uuid.UUID{}
	for _, b := range f.ordered {
		if b.Kind == "board" && f.canView(b.ID) {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

func (f *forumScope) depth(boardID uuid.UUID) int { return len(f.chain(boardID)) }

// height is the number of levels in the subtree rooted at boardID (1 for a leaf).
func (f *forumScope) height(boardID uuid.UUID) int {
	h := 1
	for _, b := range f.ordered {
		if b.ParentID != nil && *b.ParentID == boardID {
			h = max(h, 1+f.height(b.ID))
		}
	}
	return h
}

func (f *forumScope) isDescendant(boardID, ancestor uuid.UUID) bool {
	for _, b := range f.chain(boardID) {
		if b.ID == ancestor {
			return true
		}
	}
	return false
}

func (f *forumScope) hasChildren(boardID uuid.UUID) bool {
	for _, b := range f.ordered {
		if b.ParentID != nil && *b.ParentID == boardID {
			return true
		}
	}
	return false
}

func missing(perm permissions.Permission) error {
	return apperr.Forbidden("missing permission: %s", strings.Join(permissions.Names(perm), ", "))
}

// board resolves a board the caller can see.
func (f *forumScope) board(boardID uuid.UUID) (store.Board, error) {
	b, ok := f.boards[boardID]
	if !ok || !f.canView(boardID) {
		return store.Board{}, apperr.NotFound("board not found")
	}
	return b, nil
}

// boardScope resolves a board by ID together with its place's forum scope.
func (s *Service) boardScope(ctx context.Context, q *store.Queries, p *Principal, boardID uuid.UUID) (*forumScope, store.Board, error) {
	b, err := q.GetBoard(ctx, boardID)
	if err != nil {
		return nil, store.Board{}, notFound(err, "board not found")
	}
	f, err := s.forum(ctx, q, p, b.PlaceID.String())
	if err != nil {
		return nil, store.Board{}, hideAsNotFound(err, "board not found")
	}
	b, err = f.board(boardID)
	return f, b, err
}

// hideAsNotFound turns a missing place into a missing child resource so IDs cannot be
// used to probe for private places.
func hideAsNotFound(err error, msg string) error {
	var ae *apperr.Error
	if errors.As(err, &ae) && ae.Kind == apperr.KindNotFound {
		return apperr.NotFound("%s", msg)
	}
	return err
}

// refreshPublicBoards recomputes which boards signed-out visitors can read, which scopes
// instance-wide search. Call it after anything that changes guest visibility.
func (s *Service) refreshPublicBoards(ctx context.Context, q *store.Queries, placeID uuid.UUID) error {
	place, err := q.GetPlaceByID(ctx, placeID)
	if err != nil {
		return notFound(err, "place not found")
	}
	ids := []uuid.UUID{}
	if place.Visibility == "public" {
		f, err := s.loadBoards(ctx, q, place)
		if err != nil {
			return err
		}
		guest := f.guest()
		for _, b := range f.ordered {
			if b.Kind == "board" && f.visibleTo(guest, b.ID) {
				ids = append(ids, b.ID)
			}
		}
	}
	return q.SetPublicBoards(ctx, store.SetPublicBoardsParams{PlaceID: placeID, PublicIds: ids})
}

// BoardView is a board plus the caller's permissions within it.
type BoardView struct {
	Board        store.Board
	Permissions  permissions.Permission
	Subscription string
}

func (s *Service) boardViews(ctx context.Context, q *store.Queries, p *Principal, f *forumScope, boards []store.Board) ([]BoardView, error) {
	subs := map[uuid.UUID]string{}
	if p != nil && len(boards) > 0 {
		ids := make([]uuid.UUID, len(boards))
		for i, b := range boards {
			ids[i] = b.ID
		}
		rows, err := q.ListUserSubscriptions(ctx, store.ListUserSubscriptionsParams{UserID: p.User.ID, TargetIds: ids})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			subs[r.TargetID] = r.Level
		}
	}
	out := make([]BoardView, len(boards))
	for i, b := range boards {
		level := subs[b.ID]
		if level == "" {
			level = "normal"
		}
		out[i] = BoardView{Board: b, Permissions: f.perms(b.ID), Subscription: level}
	}
	return out, nil
}

// ListBoards returns every board the caller can see, depth-first in display order.
func (s *Service) ListBoards(ctx context.Context, p *Principal, ref string) ([]BoardView, error) {
	f, err := s.forum(ctx, s.q, p, ref)
	if err != nil {
		return nil, err
	}
	visible := []store.Board{}
	for _, b := range f.ordered {
		if f.canView(b.ID) {
			visible = append(visible, b)
		}
	}
	return s.boardViews(ctx, s.q, p, f, visible)
}

func (s *Service) GetBoard(ctx context.Context, p *Principal, boardID uuid.UUID) (BoardView, error) {
	f, b, err := s.boardScope(ctx, s.q, p, boardID)
	if err != nil {
		return BoardView{}, err
	}
	views, err := s.boardViews(ctx, s.q, p, f, []store.Board{b})
	if err != nil {
		return BoardView{}, err
	}
	return views[0], nil
}

type CreateBoardInput struct {
	ParentID         *uuid.UUID
	Kind             string
	Slug             string
	Name             string
	Description      string
	ReplyMode        string
	SolutionsEnabled bool
	IsNSFW           bool
}

type BoardUpdate struct {
	// ParentID moves the board: nil leaves it in place, "" moves it to the root.
	ParentID         *string
	Slug             *string
	Name             *string
	Description      *string
	Position         *int32
	ReplyMode        *string
	SolutionsEnabled *bool
	IsNSFW           *bool
}

func validateBoardName(name *string) error {
	if name == nil {
		return nil
	}
	*name = strings.TrimSpace(*name)
	if *name == "" || len([]rune(*name)) > 100 {
		return apperr.Invalid("board name must be 1-100 characters")
	}
	return nil
}

func validateBoardSlug(slug *string) error {
	if slug == nil {
		return nil
	}
	*slug = strings.ToLower(strings.TrimSpace(*slug))
	if !slugPattern.MatchString(*slug) {
		return apperr.Invalid("slug must be 3-32 lowercase letters, numbers or '-', starting and ending with a letter or number")
	}
	return nil
}

func validateReplyMode(mode *string) error {
	if mode == nil || *mode == "flat" || *mode == "threaded" {
		return nil
	}
	return apperr.Invalid("reply_mode must be flat or threaded")
}

func validateDescription(d *string) error {
	if d != nil && len([]rune(*d)) > 2000 {
		return apperr.Invalid("description must be at most 2000 characters")
	}
	return nil
}

// checkParent validates placing a board (or a subtree of the given height) under parent.
func (f *forumScope) checkParent(kind string, parentID *uuid.UUID, height int) error {
	if parentID == nil {
		return nil
	}
	if kind == "category" {
		return apperr.Invalid("categories must be top-level")
	}
	parent, ok := f.boards[*parentID]
	if !ok || !f.canView(parent.ID) {
		return apperr.Invalid("parent board not found")
	}
	if f.depth(parent.ID)+height > maxBoardDepth {
		return apperr.Invalid("boards can be nested at most %d levels deep", maxBoardDepth)
	}
	return nil
}

// canManageUnder reports whether the caller may create or move boards under parent (or at
// the root when parent is nil).
func (f *forumScope) canManageUnder(parentID *uuid.UUID) bool {
	if parentID == nil {
		return f.viewer.Has(permissions.ManageBoards)
	}
	return f.has(*parentID, permissions.ManageBoards)
}

func (s *Service) CreateBoard(ctx context.Context, p *Principal, ref string, in CreateBoardInput) (BoardView, error) {
	if in.Kind == "" {
		in.Kind = "board"
	}
	if in.ReplyMode == "" {
		in.ReplyMode = "flat"
	}
	if in.Kind != "board" && in.Kind != "category" {
		return BoardView{}, apperr.Invalid("kind must be board or category")
	}
	if err := errors.Join(validateBoardName(&in.Name), validateBoardSlug(&in.Slug),
		validateReplyMode(&in.ReplyMode), validateDescription(&in.Description)); err != nil {
		return BoardView{}, firstAppErr(err)
	}

	var view BoardView
	err := s.tx(ctx, func(q *store.Queries) error {
		f, err := s.forum(ctx, q, p, ref)
		if err != nil {
			return err
		}
		if _, err := q.LockPlace(ctx, f.place.ID); err != nil {
			return err
		}
		if !f.acc.IsMember || !f.canManageUnder(in.ParentID) {
			return missing(permissions.ManageBoards)
		}
		if err := f.checkParent(in.Kind, in.ParentID, 1); err != nil {
			return err
		}
		if len(f.boards) >= maxBoardsPerPlace {
			return apperr.Conflict("a place can have at most %d boards", maxBoardsPerPlace)
		}
		pos, err := q.NextBoardPosition(ctx, store.NextBoardPositionParams{PlaceID: f.place.ID, ParentID: in.ParentID})
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		b, err := q.CreateBoard(ctx, store.CreateBoardParams{
			ID: id, PlaceID: f.place.ID, ParentID: in.ParentID, Kind: in.Kind, Slug: in.Slug, Name: in.Name,
			Description: in.Description, Position: pos, ReplyMode: in.ReplyMode,
			SolutionsEnabled: in.SolutionsEnabled, IsNsfw: in.IsNSFW,
		})
		if database.IsUniqueViolation(err, "boards_slug_key") {
			return apperr.Conflict("slug %q is already used by another board in this place", in.Slug)
		}
		if err != nil {
			return err
		}
		f.boards[b.ID] = b
		f.ordered = append(f.ordered, b)
		if err := s.refreshPublicBoards(ctx, q, f.place.ID); err != nil {
			return err
		}
		if err := s.audit(ctx, q, f.place.ID, p, "board.create", "board", &b.ID, "", map[string]any{"name": b.Name}); err != nil {
			return err
		}
		view = BoardView{Board: b, Permissions: f.perms(b.ID), Subscription: "normal"}
		return nil
	})
	return view, err
}

func (s *Service) UpdateBoard(ctx context.Context, p *Principal, boardID uuid.UUID, in BoardUpdate) (BoardView, error) {
	if err := errors.Join(validateBoardName(in.Name), validateBoardSlug(in.Slug),
		validateReplyMode(in.ReplyMode), validateDescription(in.Description)); err != nil {
		return BoardView{}, firstAppErr(err)
	}
	if in.Position != nil && *in.Position < 0 {
		return BoardView{}, apperr.Invalid("position must not be negative")
	}

	var view BoardView
	err := s.tx(ctx, func(q *store.Queries) error {
		f, b, err := s.boardScope(ctx, q, p, boardID)
		if err != nil {
			return err
		}
		if _, err := q.LockPlace(ctx, f.place.ID); err != nil {
			return err
		}
		if !f.acc.IsMember || !f.has(b.ID, permissions.ManageBoards) {
			return missing(permissions.ManageBoards)
		}
		params := store.UpdateBoardParams{
			ID: b.ID, Name: in.Name, Slug: in.Slug, Description: in.Description, Position: in.Position,
			ReplyMode: in.ReplyMode, SolutionsEnabled: in.SolutionsEnabled, IsNsfw: in.IsNSFW,
		}
		if in.ParentID != nil {
			var parent *uuid.UUID
			if *in.ParentID != "" {
				id, err := uuid.Parse(*in.ParentID)
				if err != nil {
					return apperr.Invalid("parent_id must be a UUID or empty")
				}
				parent = &id
			}
			if !uuidPtrEqual(parent, b.ParentID) {
				if parent != nil && f.isDescendant(*parent, b.ID) {
					return apperr.Invalid("a board cannot be moved under itself")
				}
				if !f.canManageUnder(parent) {
					return apperr.Forbidden("you cannot move boards there")
				}
				if err := f.checkParent(b.Kind, parent, f.height(b.ID)); err != nil {
					return err
				}
				params.SetParent = true
				params.ParentID = parent
			}
		}
		updated, err := q.UpdateBoard(ctx, params)
		if database.IsUniqueViolation(err, "boards_slug_key") {
			return apperr.Conflict("slug is already used by another board in this place")
		}
		if err != nil {
			return err
		}
		if params.SetParent {
			if err := s.refreshPublicBoards(ctx, q, f.place.ID); err != nil {
				return err
			}
		}
		if err := s.audit(ctx, q, f.place.ID, p, "board.update", "board", &b.ID, "", boardChanges(in)); err != nil {
			return err
		}
		f.boards[updated.ID] = updated
		views, err := s.boardViews(ctx, q, p, f, []store.Board{updated})
		if err != nil {
			return err
		}
		view = views[0]
		return nil
	})
	return view, err
}

func boardChanges(in BoardUpdate) map[string]any {
	m := map[string]any{}
	set := func(k string, v any, ok bool) {
		if ok {
			m[k] = v
		}
	}
	set("name", in.Name, in.Name != nil)
	set("slug", in.Slug, in.Slug != nil)
	set("description", in.Description, in.Description != nil)
	set("position", in.Position, in.Position != nil)
	set("reply_mode", in.ReplyMode, in.ReplyMode != nil)
	set("solutions_enabled", in.SolutionsEnabled, in.SolutionsEnabled != nil)
	set("is_nsfw", in.IsNSFW, in.IsNSFW != nil)
	set("parent_id", in.ParentID, in.ParentID != nil)
	return m
}

func uuidPtrEqual(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// DeleteBoard removes an empty board. Child boards and live topics must be moved or
// deleted first so content is never removed by accident.
func (s *Service) DeleteBoard(ctx context.Context, p *Principal, boardID uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		f, b, err := s.boardScope(ctx, q, p, boardID)
		if err != nil {
			return err
		}
		if _, err := q.LockPlace(ctx, f.place.ID); err != nil {
			return err
		}
		if !f.acc.IsMember || !f.has(b.ID, permissions.ManageBoards) {
			return missing(permissions.ManageBoards)
		}
		if f.hasChildren(b.ID) {
			return apperr.Conflict("move or delete this board's child boards first")
		}
		n, err := q.CountLiveBoardTopics(ctx, b.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			return apperr.Conflict("move or delete this board's %d topic(s) first", n)
		}
		if err := q.DeleteTargetSubscriptions(ctx, store.DeleteTargetSubscriptionsParams{TargetType: "board", TargetID: b.ID}); err != nil {
			return err
		}
		if err := q.DeleteBoard(ctx, b.ID); err != nil {
			return err
		}
		return s.audit(ctx, q, f.place.ID, p, "board.delete", "board", &b.ID, "", map[string]any{"name": b.Name})
	})
}

// ListBoardOverwrites returns a board's own overwrites (not inherited ones).
func (s *Service) ListBoardOverwrites(ctx context.Context, p *Principal, boardID uuid.UUID) ([]permissions.Overwrite, error) {
	f, b, err := s.boardScope(ctx, s.q, p, boardID)
	if err != nil {
		return nil, err
	}
	if !f.acc.IsMember || !f.has(b.ID, permissions.ManageBoards) {
		return nil, missing(permissions.ManageBoards)
	}
	ows := f.overwrites[b.ID]
	if ows == nil {
		ows = []permissions.Overwrite{}
	}
	return ows, nil
}

// SetBoardOverwrite allows or denies forum permissions for a role within a board. Callers
// need MANAGE_BOARDS in the board, may only touch roles ranked below them (or @everyone),
// and may only set bits they hold in that board.
func (s *Service) SetBoardOverwrite(ctx context.Context, p *Principal, boardID, roleID uuid.UUID, allow, deny int64) (permissions.Overwrite, error) {
	a, d := permissions.Permission(allow), permissions.Permission(deny)
	if (a|d)&^permissions.Forum != 0 {
		return permissions.Overwrite{}, apperr.Invalid("overwrites may only contain forum permissions: %s",
			strings.Join(permissions.Names(permissions.Forum), ", "))
	}
	if a&d != 0 {
		return permissions.Overwrite{}, apperr.Invalid("a permission cannot be both allowed and denied")
	}
	var out permissions.Overwrite
	err := s.tx(ctx, func(q *store.Queries) error {
		f, b, role, err := s.overwriteGuard(ctx, q, p, boardID, roleID)
		if err != nil {
			return err
		}
		if (a|d)&^f.perms(b.ID) != 0 {
			return apperr.Forbidden("you cannot allow or deny permissions you do not have in this board")
		}
		row, err := q.UpsertBoardOverwrite(ctx, store.UpsertBoardOverwriteParams{
			BoardID: b.ID, RoleID: role.ID, PlaceID: f.place.ID, Allow: allow, Deny: deny,
		})
		if err != nil {
			return err
		}
		if err := s.refreshPublicBoards(ctx, q, f.place.ID); err != nil {
			return err
		}
		out = permissions.Overwrite{RoleID: row.RoleID, Allow: permissions.Permission(row.Allow), Deny: permissions.Permission(row.Deny)}
		return s.audit(ctx, q, f.place.ID, p, "board.overwrite_update", "board", &b.ID, "",
			map[string]any{"role_id": role.ID, "allow": allow, "deny": deny})
	})
	return out, err
}

func (s *Service) DeleteBoardOverwrite(ctx context.Context, p *Principal, boardID, roleID uuid.UUID) error {
	return s.tx(ctx, func(q *store.Queries) error {
		f, b, role, err := s.overwriteGuard(ctx, q, p, boardID, roleID)
		if err != nil {
			return err
		}
		n, err := q.DeleteBoardOverwrite(ctx, store.DeleteBoardOverwriteParams{BoardID: b.ID, RoleID: role.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.NotFound("overwrite not found")
		}
		if err := s.refreshPublicBoards(ctx, q, f.place.ID); err != nil {
			return err
		}
		return s.audit(ctx, q, f.place.ID, p, "board.overwrite_delete", "board", &b.ID, "", map[string]any{"role_id": role.ID})
	})
}

func (s *Service) overwriteGuard(ctx context.Context, q *store.Queries, p *Principal, boardID, roleID uuid.UUID) (*forumScope, store.Board, store.Role, error) {
	f, b, err := s.boardScope(ctx, q, p, boardID)
	if err != nil {
		return nil, b, store.Role{}, err
	}
	if _, err := q.LockPlace(ctx, f.place.ID); err != nil {
		return nil, b, store.Role{}, err
	}
	if !f.acc.IsMember || !f.has(b.ID, permissions.ManageBoards) {
		return nil, b, store.Role{}, missing(permissions.ManageBoards)
	}
	role, err := q.GetRole(ctx, store.GetRoleParams{ID: roleID, PlaceID: f.place.ID})
	if err != nil {
		return nil, b, role, notFound(err, "role not found")
	}
	if !role.IsDefault && !f.viewer.CanManageRoleAt(role.Position) {
		return nil, b, role, apperr.Forbidden("you can only set overwrites for roles ranked below your highest role")
	}
	return f, b, role, nil
}
