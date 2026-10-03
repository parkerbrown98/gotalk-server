package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const (
	maxDraftsPerUser = 50
	maxDraftBytes    = 64 * 1024
)

// Draft keys are chosen by clients, e.g. "topic:<boardID>" or "reply:<topicID>".
var draftKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9:_.-]{1,100}$`)

func validateDraftKey(key string) error {
	if !draftKeyPattern.MatchString(key) {
		return apperr.Invalid("draft key must be 1-100 letters, numbers or ':', '_', '.', '-'")
	}
	return nil
}

func (s *Service) ListDrafts(ctx context.Context, p *Principal) ([]store.Draft, error) {
	return s.q.ListDrafts(ctx, p.User.ID)
}

func (s *Service) GetDraft(ctx context.Context, p *Principal, key string) (store.Draft, error) {
	if err := validateDraftKey(key); err != nil {
		return store.Draft{}, err
	}
	d, err := s.q.GetDraft(ctx, store.GetDraftParams{UserID: p.User.ID, Key: key})
	return d, notFound(err, "draft not found")
}

// SaveDraft stores client-defined JSON (such as a title, content and tags) so unfinished
// posts follow the user across devices.
func (s *Service) SaveDraft(ctx context.Context, p *Principal, key string, data map[string]any) (store.Draft, error) {
	if err := validateDraftKey(key); err != nil {
		return store.Draft{}, err
	}
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return store.Draft{}, apperr.Invalid("draft data must be a JSON object")
	}
	if len(raw) > maxDraftBytes {
		return store.Draft{}, apperr.Invalid("draft data must be at most %d bytes", maxDraftBytes)
	}
	var draft store.Draft
	err = s.tx(ctx, func(q *store.Queries) error {
		_, err := q.GetDraft(ctx, store.GetDraftParams{UserID: p.User.ID, Key: key})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			n, err := q.CountDrafts(ctx, p.User.ID)
			if err != nil {
				return err
			}
			if n >= maxDraftsPerUser {
				return apperr.Conflict("you can keep at most %d drafts", maxDraftsPerUser)
			}
		case err != nil:
			return err
		}
		if err := q.UpsertDraft(ctx, store.UpsertDraftParams{UserID: p.User.ID, Key: key, Data: raw}); err != nil {
			return err
		}
		draft, err = q.GetDraft(ctx, store.GetDraftParams{UserID: p.User.ID, Key: key})
		return err
	})
	return draft, err
}

func (s *Service) DeleteDraft(ctx context.Context, p *Principal, key string) error {
	if err := validateDraftKey(key); err != nil {
		return err
	}
	n, err := s.q.DeleteDraft(ctx, store.DeleteDraftParams{UserID: p.User.ID, Key: key})
	if err == nil && n == 0 {
		return apperr.NotFound("draft not found")
	}
	return err
}
