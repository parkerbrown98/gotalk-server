package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/auth"
	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Principal is an authenticated caller.
type Principal struct {
	User      store.User
	SessionID uuid.UUID
}

type AuthResult struct {
	User             store.User
	SessionID        uuid.UUID
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

func (s *Service) createUser(ctx context.Context, q *store.Queries, username, email, password string, admin bool) (store.User, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return store.User{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.User{}, err
	}
	user, err := q.CreateUser(ctx, store.CreateUserParams{
		ID:              id,
		Username:        username,
		Email:           email,
		PasswordHash:    hash,
		DisplayName:     username,
		IsInstanceAdmin: admin,
	})
	switch {
	case database.IsUniqueViolation(err, "users_username_key"):
		return store.User{}, apperr.Conflict("username is already taken")
	case database.IsUniqueViolation(err, "users_email_key"):
		return store.User{}, apperr.Conflict("an account with that email already exists")
	}
	return user, err
}

func (s *Service) createSession(ctx context.Context, q *store.Queries, user store.User, client ClientInfo) (*AuthResult, error) {
	sessionID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	refresh, hash, err := auth.NewRefreshToken(sessionID)
	if err != nil {
		return nil, err
	}
	refreshExp := time.Now().Add(s.cfg.Auth.RefreshTokenTTL)
	if _, err := q.CreateSession(ctx, store.CreateSessionParams{
		ID:               sessionID,
		UserID:           user.ID,
		RefreshTokenHash: hash,
		UserAgent:        truncate(client.UserAgent, 512),
		IpAddress:        client.IP,
		ExpiresAt:        refreshExp,
	}); err != nil {
		return nil, err
	}
	access, accessExp, err := s.tokens.Issue(user.ID, sessionID)
	if err != nil {
		return nil, err
	}
	return &AuthResult{
		User:             user,
		SessionID:        sessionID,
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     refresh,
		RefreshExpiresAt: refreshExp,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

type RegisterInput struct {
	Username   string
	Email      string
	Password   string
	InviteCode string
}

// Register creates an account according to the instance's registration mode. In
// invite_only mode a valid place invite is required and the new user joins that place.
func (s *Service) Register(ctx context.Context, in RegisterInput, client ClientInfo) (*AuthResult, error) {
	in.Email = strings.TrimSpace(in.Email)
	if err := errors.Join(validateUsername(in.Username), validateEmail(in.Email), validatePassword(in.Password)); err != nil {
		return nil, firstAppErr(err)
	}

	settings, err := s.q.GetInstanceSettings(ctx)
	if err != nil {
		return nil, err
	}
	switch settings.RegistrationMode {
	case "closed":
		return nil, apperr.Forbidden("registration is closed on this instance")
	case "invite_only":
		if in.InviteCode == "" {
			return nil, apperr.Forbidden("registration on this instance requires an invite code")
		}
	}

	var result *AuthResult
	err = s.tx(ctx, func(q *store.Queries) error {
		user, err := s.createUser(ctx, q, in.Username, in.Email, in.Password, false)
		if err != nil {
			return err
		}
		if in.InviteCode != "" {
			if _, err := s.redeemInvite(ctx, q, user.ID, in.InviteCode); err != nil {
				return err
			}
		}
		result, err = s.createSession(ctx, q, user, client)
		return err
	})
	return result, err
}

func (s *Service) Login(ctx context.Context, login, password string, client ClientInfo) (*AuthResult, error) {
	invalid := apperr.Unauthorized("invalid username/email or password")
	if len(password) > maxPasswordLen {
		return nil, invalid
	}
	user, err := s.q.GetUserByLogin(ctx, strings.TrimSpace(login))
	if errors.Is(err, pgx.ErrNoRows) {
		auth.EqualizeTiming(password)
		return nil, invalid
	}
	if err != nil {
		return nil, err
	}
	ok, err := auth.VerifyPassword(password, user.PasswordHash)
	if err != nil || !ok {
		return nil, invalid
	}
	return s.createSession(ctx, s.q, user, client)
}

// Refresh rotates a refresh token. Presenting an already-rotated token is treated as
// theft: the whole session is revoked so both the attacker and the victim must log in again.
func (s *Service) Refresh(ctx context.Context, refreshToken string, client ClientInfo) (*AuthResult, error) {
	invalid := apperr.Unauthorized("refresh token is invalid or expired")
	sessionID, secret, err := auth.SplitRefreshToken(refreshToken)
	if err != nil {
		return nil, invalid
	}

	var result *AuthResult
	reused := false
	sessionUser := uuid.Nil
	err = s.tx(ctx, func(q *store.Queries) error {
		sess, err := q.GetSessionForUpdate(ctx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid
		}
		if err != nil {
			return err
		}
		if sess.RevokedAt != nil || time.Now().After(sess.ExpiresAt) {
			return invalid
		}
		if !auth.HashesEqual(sess.RefreshTokenHash, auth.HashRefreshSecret(secret)) {
			reused = true
			sessionUser = sess.UserID
			return invalid
		}
		user, err := q.GetUserByID(ctx, sess.UserID)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid
		}
		if err != nil {
			return err
		}

		newToken, newHash, err := auth.NewRefreshToken(sessionID)
		if err != nil {
			return err
		}
		refreshExp := time.Now().Add(s.cfg.Auth.RefreshTokenTTL)
		if _, err := q.RotateSession(ctx, store.RotateSessionParams{
			ID:               sessionID,
			RefreshTokenHash: newHash,
			ExpiresAt:        refreshExp,
			UserAgent:        truncate(client.UserAgent, 512),
			IpAddress:        client.IP,
		}); err != nil {
			return err
		}
		access, accessExp, err := s.tokens.Issue(user.ID, sessionID)
		if err != nil {
			return err
		}
		result = &AuthResult{
			User:             user,
			SessionID:        sessionID,
			AccessToken:      access,
			AccessExpiresAt:  accessExp,
			RefreshToken:     newToken,
			RefreshExpiresAt: refreshExp,
		}
		return nil
	})
	if reused {
		if rerr := s.q.RevokeSessionByID(ctx, sessionID); rerr != nil {
			s.log.Error("revoking session after refresh token reuse", "session", sessionID, "error", rerr)
		} else if sessionUser != uuid.Nil {
			s.emitSessionsEnded(ctx, s.q, sessionUser, []uuid.UUID{sessionID}, nil)
		}
		s.log.Warn("refresh token reuse detected; session revoked", "session", sessionID, "ip", client.IP)
	}
	return result, err
}

// Authenticate validates a bearer access token and loads its live session, so revoked
// sessions stop working immediately rather than at token expiry.
func (s *Service) Authenticate(ctx context.Context, accessToken string) (*Principal, error) {
	userID, sessionID, err := s.tokens.Parse(accessToken)
	if err != nil {
		return nil, apperr.Unauthorized("access token is invalid or expired")
	}
	row, err := s.q.GetActiveSessionUser(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.User.ID != userID) {
		return nil, apperr.Unauthorized("session has ended; please log in again")
	}
	if err != nil {
		return nil, err
	}
	return &Principal{User: row.User, SessionID: sessionID}, nil
}

func (s *Service) Logout(ctx context.Context, p *Principal) error {
	if err := s.q.RevokeSessionByID(ctx, p.SessionID); err != nil {
		return err
	}
	s.emitSessionsEnded(ctx, s.q, p.User.ID, []uuid.UUID{p.SessionID}, nil)
	return nil
}

func (s *Service) GetUserByUsername(ctx context.Context, username string) (store.User, error) {
	user, err := s.q.GetUserByUsername(ctx, username)
	return user, notFound(err, "user not found")
}

type ProfileUpdate struct {
	DisplayName *string
	Bio         *string
	Pronouns    *string
	AvatarURL   *string
}

func (s *Service) UpdateProfile(ctx context.Context, p *Principal, in ProfileUpdate) (store.User, error) {
	if err := validateOptionalURL("avatar_url", in.AvatarURL); err != nil {
		return store.User{}, err
	}
	if in.DisplayName != nil {
		trimmed := strings.TrimSpace(*in.DisplayName)
		in.DisplayName = &trimmed
	}
	user, err := s.q.UpdateUserProfile(ctx, store.UpdateUserProfileParams{
		ID:          p.User.ID,
		DisplayName: in.DisplayName,
		Bio:         in.Bio,
		Pronouns:    in.Pronouns,
		AvatarUrl:   in.AvatarURL,
	})
	return user, notFound(err, "user not found")
}

// ChangePassword verifies the current password, stores the new one, and signs out every
// other session.
func (s *Service) ChangePassword(ctx context.Context, p *Principal, current, next string) error {
	if ok, err := auth.VerifyPassword(current, p.User.PasswordHash); err != nil || !ok {
		return apperr.Forbidden("current password is incorrect")
	}
	if err := validatePassword(next); err != nil {
		return err
	}
	hash, err := auth.HashPassword(next)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(q *store.Queries) error {
		if err := q.UpdateUserPassword(ctx, store.UpdateUserPasswordParams{ID: p.User.ID, PasswordHash: hash}); err != nil {
			return err
		}
		s.emitSessionsEnded(ctx, q, p.User.ID, nil, &p.SessionID)
		return q.RevokeOtherUserSessions(ctx, store.RevokeOtherUserSessionsParams{
			UserID:        p.User.ID,
			KeepSessionID: p.SessionID,
		})
	})
}

func (s *Service) ListSessions(ctx context.Context, p *Principal) ([]store.Session, error) {
	return s.q.ListActiveSessions(ctx, p.User.ID)
}

func (s *Service) RevokeSession(ctx context.Context, p *Principal, sessionID uuid.UUID) error {
	n, err := s.q.RevokeSession(ctx, store.RevokeSessionParams{ID: sessionID, UserID: p.User.ID})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.NotFound("session not found")
	}
	s.emitSessionsEnded(ctx, s.q, p.User.ID, []uuid.UUID{sessionID}, nil)
	return nil
}

// DeleteAccount soft-deletes the caller's account: memberships are removed, sessions are
// revoked, and personal data is scrubbed. The username stays reserved.
func (s *Service) DeleteAccount(ctx context.Context, p *Principal, password string) error {
	if ok, err := auth.VerifyPassword(password, p.User.PasswordHash); err != nil || !ok {
		return apperr.Forbidden("password is incorrect")
	}
	return s.tx(ctx, func(q *store.Queries) error {
		owned, err := q.CountOwnedPlaces(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if owned > 0 {
			return apperr.Conflict("transfer ownership of or delete your %d place(s) before deleting your account", owned)
		}
		if p.User.IsInstanceAdmin {
			admins, err := q.CountInstanceAdmins(ctx)
			if err != nil {
				return err
			}
			if admins <= 1 {
				return apperr.Conflict("the last instance administrator cannot delete their account")
			}
		}
		if err := q.DecrementMemberCountsForUser(ctx, p.User.ID); err != nil {
			return err
		}
		if err := q.RemoveAllUserMemberships(ctx, p.User.ID); err != nil {
			return err
		}
		// Posts and messages stay (attributed to a deleted account); personal activity is erased.
		for _, erase := range []func(context.Context, uuid.UUID) error{
			q.RemoveUserReactions, q.DeleteUserNotifications, q.DeleteUserSubscriptions,
			q.DeleteUserDrafts, q.DeleteUserTopicReads, q.RemoveUserMessageReactions,
			q.DeleteUserChannelReads, q.ReassignGroupDMOwnership,
		} {
			if err := erase(ctx, p.User.ID); err != nil {
				return err
			}
		}
		if _, err := q.LeaveAllGroupDMs(ctx, p.User.ID); err != nil {
			return err
		}
		if err := q.RevokeAllUserSessions(ctx, p.User.ID); err != nil {
			return err
		}
		s.emitSessionsEnded(ctx, q, p.User.ID, nil, nil)
		return q.SoftDeleteUser(ctx, p.User.ID)
	})
}
