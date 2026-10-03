package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const maxSearchQueryLen = 200

type SearchInput struct {
	Query string
	// BoardID narrows a place search to one board.
	BoardID *uuid.UUID
	// Author is a username.
	Author     string
	Tag        string
	Solved     *bool
	After      *time.Time
	Before     *time.Time
	TopicsOnly bool
	// Sort is relevance (default), newest or oldest.
	Sort string
}

type SearchHit struct {
	Post    store.Post
	Topic   store.Topic
	Place   store.Place
	Author  *store.User
	Snippet string
	Rank    float64
}

func (in *SearchInput) params(page Pagination) (store.SearchPostsParams, error) {
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" || utf8.RuneCountInString(in.Query) > maxSearchQueryLen {
		return store.SearchPostsParams{}, apperr.Invalid("q must be 1-%d characters", maxSearchQueryLen)
	}
	switch in.Sort {
	case "":
		in.Sort = "relevance"
	case "relevance", "newest", "oldest":
	default:
		return store.SearchPostsParams{}, apperr.Invalid("sort must be one of relevance, newest, oldest")
	}
	tag, err := optionalTag(in.Tag)
	if err != nil {
		return store.SearchPostsParams{}, err
	}
	page = page.normalized()
	return store.SearchPostsParams{
		Query: in.Query, Sort: in.Sort, BoardIds: []uuid.UUID{}, BoardID: in.BoardID, Tag: tag,
		Solved: in.Solved, After: in.After, Before: in.Before, TopicsOnly: in.TopicsOnly,
		Lim: page.Limit, Off: page.Offset,
	}, nil
}

// resolveAuthor maps an author filter to a user ID; ok is false when no such user exists.
func (s *Service) resolveAuthor(ctx context.Context, username string) (*uuid.UUID, bool, error) {
	if username == "" {
		return nil, true, nil
	}
	u, err := s.q.GetUserByUsername(ctx, strings.TrimPrefix(username, "@"))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &u.ID, true, nil
}

// SearchPlace searches every board of a place that the caller can read.
func (s *Service) SearchPlace(ctx context.Context, p *Principal, ref string, in SearchInput, page Pagination) ([]SearchHit, error) {
	params, err := in.params(page)
	if err != nil {
		return nil, err
	}
	f, err := s.forum(ctx, s.q, p, ref)
	if err != nil {
		return nil, err
	}
	params.PlaceID = &f.place.ID
	params.BoardIds = f.visibleBoardIDs()
	return s.search(ctx, in, params)
}

// SearchPublic searches content that signed-out visitors can read, across every public
// place on the instance.
func (s *Service) SearchPublic(ctx context.Context, in SearchInput, page Pagination) ([]SearchHit, error) {
	params, err := in.params(page)
	if err != nil {
		return nil, err
	}
	params.PublicOnly = true
	return s.search(ctx, in, params)
}

func (s *Service) search(ctx context.Context, in SearchInput, params store.SearchPostsParams) ([]SearchHit, error) {
	author, ok, err := s.resolveAuthor(ctx, in.Author)
	if err != nil || !ok {
		return []SearchHit{}, err
	}
	params.AuthorID = author
	rows, err := s.q.SearchPosts(ctx, params)
	if err != nil {
		return nil, err
	}
	var userIDs, placeIDs []uuid.UUID
	for _, r := range rows {
		placeIDs = append(placeIDs, r.Post.PlaceID)
		if r.Post.AuthorID != nil {
			userIDs = append(userIDs, *r.Post.AuthorID)
		}
	}
	users, err := s.usersByID(ctx, s.q, userIDs)
	if err != nil {
		return nil, err
	}
	places := map[uuid.UUID]store.Place{}
	if len(placeIDs) > 0 {
		list, err := s.q.ListPlacesByIDs(ctx, dedupe(placeIDs))
		if err != nil {
			return nil, err
		}
		for _, pl := range list {
			places[pl.ID] = pl
		}
	}
	out := make([]SearchHit, len(rows))
	for i, r := range rows {
		out[i] = SearchHit{
			Post: r.Post, Topic: r.Topic, Place: places[r.Post.PlaceID],
			Author: ptrUser(users, r.Post.AuthorID), Snippet: r.Snippet, Rank: r.Rank,
		}
	}
	return out, nil
}
