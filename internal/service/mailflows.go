package service

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"strconv"
	"strings"
	"sync"
	texttemplate "text/template"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/auth"
	"github.com/parkerbrown98/gotalk-server/internal/mail"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Email token purposes.
const (
	PurposePasswordReset     = "password_reset"
	PurposeEmailVerification = "email_verification"
)

const (
	passwordResetTTL     = time.Hour
	emailVerificationTTL = 48 * time.Hour
	// emailCooldown spaces out emails of one kind to one account, so the endpoints cannot
	// be used to flood an inbox.
	emailCooldown = time.Minute
	mailBatch     = 20
	mailLock      = 2 * time.Minute
	// MaxMailAttempts bounds retries of one message.
	MaxMailAttempts = 6
)

// mailBackoff is the delay before each retry.
var mailBackoff = []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute}

var mailPoll = 5 * time.Second

type emailContent struct {
	Instance string
	Username string
	Link     string
	Expires  string
}

type emailTemplate struct {
	subject string
	text    *texttemplate.Template
	html    *template.Template
}

const htmlLayout = `<!doctype html><html><body style="font-family:system-ui,-apple-system,Segoe UI,sans-serif;line-height:1.5;color:#1f2328;max-width:560px;margin:24px auto;padding:0 16px">%s<p style="color:#6e7781;font-size:13px;margin-top:32px">Sent by {{.Instance}}. If you did not expect this email, you can ignore it.</p></body></html>`

func newEmailTemplate(subject, text, html string) emailTemplate {
	return emailTemplate{
		subject: subject,
		text:    texttemplate.Must(texttemplate.New("text").Parse(text)),
		html:    template.Must(template.New("html").Parse(strings.Replace(htmlLayout, "%s", html, 1))),
	}
}

var (
	passwordResetEmail = newEmailTemplate("Reset your {{.Instance}} password", `Hi {{.Username}},

Someone (hopefully you) asked to reset the password of your account on {{.Instance}}.
Open this link to choose a new password:

{{.Link}}

The link works once and expires in {{.Expires}}. If you did not ask for this, ignore this
email; your password stays the same.
`, `<p>Hi {{.Username}},</p>
<p>Someone (hopefully you) asked to reset the password of your account on <strong>{{.Instance}}</strong>.</p>
<p><a href="{{.Link}}" style="display:inline-block;background:#5865f2;color:#fff;padding:10px 18px;border-radius:8px;text-decoration:none;font-weight:600">Choose a new password</a></p>
<p>Or paste this link into your browser:<br><a href="{{.Link}}">{{.Link}}</a></p>
<p>The link works once and expires in {{.Expires}}. If you did not ask for this, your password stays the same.</p>`)

	verificationEmail = newEmailTemplate("Confirm your email address for {{.Instance}}", `Hi {{.Username}},

Confirm that this is your email address for {{.Instance}} by opening this link:

{{.Link}}

The link expires in {{.Expires}}.
`, `<p>Hi {{.Username}},</p>
<p>Confirm that this is your email address for <strong>{{.Instance}}</strong>.</p>
<p><a href="{{.Link}}" style="display:inline-block;background:#5865f2;color:#fff;padding:10px 18px;border-radius:8px;text-decoration:none;font-weight:600">Confirm email address</a></p>
<p>Or paste this link into your browser:<br><a href="{{.Link}}">{{.Link}}</a></p>
<p>The link expires in {{.Expires}}.</p>`)

	testEmail = newEmailTemplate("Test email from {{.Instance}}", `This is a test email from {{.Instance}}.

Email delivery is working.
`, `<p>This is a test email from <strong>{{.Instance}}</strong>.</p><p>Email delivery is working. &#127881;</p>`)
)

func (t emailTemplate) render(c emailContent) (mail.Message, error) {
	var subject, text, html bytes.Buffer
	if err := texttemplate.Must(texttemplate.New("s").Parse(t.subject)).Execute(&subject, c); err != nil {
		return mail.Message{}, err
	}
	if err := t.text.Execute(&text, c); err != nil {
		return mail.Message{}, err
	}
	if err := t.html.Execute(&html, c); err != nil {
		return mail.Message{}, err
	}
	return mail.Message{Subject: subject.String(), Text: text.String(), HTML: html.String()}, nil
}

func renderTestEmail(instance string) mail.Message {
	msg, _ := testEmail.render(emailContent{Instance: instance})
	return msg
}

func humanDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		if h := int(d / time.Hour); h != 1 {
			return strconv.Itoa(h) + " hours"
		}
		return "1 hour"
	}
	return strconv.Itoa(int(d/time.Minute)) + " minutes"
}

// queueMail renders and enqueues an email in q's transaction; delivery happens after
// commit on whichever replica claims it.
func (s *Service) queueMail(ctx context.Context, q *store.Queries, kind, to string, tmpl emailTemplate, c emailContent, expires time.Time) error {
	msg, err := tmpl.render(c)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := q.EnqueueMail(ctx, store.EnqueueMailParams{
		ID: id, Kind: kind, ToAddress: to, Subject: msg.Subject, TextBody: msg.Text, HtmlBody: msg.HTML, ExpiresAt: expires,
	}); err != nil {
		return err
	}
	s.afterCommit(ctx, q, func(context.Context) { s.wakeMail() })
	return nil
}

func (s *Service) wakeMail() {
	select {
	case s.mailWake <- struct{}{}:
	default:
	}
}

// issueEmailToken creates a single-use token and returns it (the hash is stored).
func issueEmailToken(ctx context.Context, q *store.Queries, user store.User, purpose string, ttl time.Duration) (string, time.Time, error) {
	token, err := auth.RandomString(43, auth.Base62)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(ttl)
	err = q.CreateEmailToken(ctx, store.CreateEmailTokenParams{
		TokenHash: auth.HashAPIToken(token), UserID: user.ID, Purpose: purpose, Email: user.Email, ExpiresAt: expires,
	})
	return token, expires, err
}

// recentlySent reports whether an email of purpose went to the user within emailCooldown.
func recentlySent(ctx context.Context, q *store.Queries, userID uuid.UUID, purpose string) (bool, error) {
	last, err := q.LatestEmailTokenAt(ctx, store.LatestEmailTokenAtParams{UserID: userID, Purpose: purpose})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && time.Since(last) < emailCooldown, err
}

func (s *Service) instanceName(ctx context.Context, q *store.Queries) string {
	settings, err := q.GetInstanceSettings(ctx)
	if err != nil || settings.Name == "" {
		return "Gotalk"
	}
	return settings.Name
}

func (s *Service) requireMail() error {
	if !s.MailEnabled() {
		return apperr.Unavailable("email is not configured on this instance; ask an administrator for help")
	}
	return nil
}

// RequestPasswordReset emails a reset link if an account uses the address. It answers the
// same way whether or not one does, so it cannot be used to find out who has an account.
// baseURL is the instance's public URL; links point at its /reset-password page.
func (s *Service) RequestPasswordReset(ctx context.Context, email, baseURL string) error {
	if err := s.requireMail(); err != nil {
		return err
	}
	email = strings.TrimSpace(email)
	if err := validateEmail(email); err != nil {
		return err
	}
	return s.tx(ctx, func(q *store.Queries) error {
		user, err := q.GetUserByEmail(ctx, email)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || user.IsBot {
			return err
		}
		if recent, err := recentlySent(ctx, q, user.ID, PurposePasswordReset); err != nil || recent {
			return err
		}
		token, expires, err := issueEmailToken(ctx, q, user, PurposePasswordReset, passwordResetTTL)
		if err != nil {
			return err
		}
		return s.queueMail(ctx, q, PurposePasswordReset, user.Email, passwordResetEmail, emailContent{
			Instance: s.instanceName(ctx, q), Username: user.Username,
			Link: baseURL + "/reset-password?token=" + token, Expires: humanDuration(passwordResetTTL),
		}, expires)
	})
}

// ResetPassword sets a new password with a reset token. Every session and personal access
// token of the account is revoked, and the email address counts as verified.
func (s *Service) ResetPassword(ctx context.Context, token, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(q *store.Queries) error {
		tok, err := q.ConsumeEmailToken(ctx, store.ConsumeEmailTokenParams{TokenHash: auth.HashAPIToken(token), Purpose: PurposePasswordReset})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Invalid("this password reset link is invalid, already used or expired; request a new one")
		}
		if err != nil {
			return err
		}
		user, err := q.GetUserByID(ctx, tok.UserID)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Invalid("this password reset link is invalid, already used or expired; request a new one")
		}
		if err != nil {
			return err
		}
		if err := q.UpdateUserPassword(ctx, store.UpdateUserPasswordParams{ID: user.ID, PasswordHash: hash}); err != nil {
			return err
		}
		if err := q.InvalidateEmailTokens(ctx, store.InvalidateEmailTokensParams{UserID: user.ID, Purpose: PurposePasswordReset}); err != nil {
			return err
		}
		if _, err := q.MarkEmailVerified(ctx, store.MarkEmailVerifiedParams{ID: user.ID, Email: tok.Email}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		s.emitSessionsEnded(ctx, q, user.ID, nil, nil)
		if _, err := q.RevokePersonalTokens(ctx, user.ID); err != nil {
			return err
		}
		if err := q.RevokeAllUserSessions(ctx, user.ID); err != nil {
			return err
		}
		s.log.Info("password reset by email", "user", user.ID)
		return nil
	})
}

// SendVerificationEmail emails the caller a link confirming their address.
func (s *Service) SendVerificationEmail(ctx context.Context, p *Principal, baseURL string) error {
	if p.User.IsBot {
		return apperr.Forbidden("bots have no email address")
	}
	if p.User.EmailVerifiedAt != nil {
		return apperr.Conflict("your email address is already verified")
	}
	if err := s.requireMail(); err != nil {
		return err
	}
	return s.tx(ctx, func(q *store.Queries) error {
		recent, err := recentlySent(ctx, q, p.User.ID, PurposeEmailVerification)
		if err != nil {
			return err
		}
		if recent {
			return apperr.Conflict("a verification email was sent less than a minute ago; check your inbox and spam folder")
		}
		return s.queueVerification(ctx, q, p.User, baseURL)
	})
}

func (s *Service) queueVerification(ctx context.Context, q *store.Queries, user store.User, baseURL string) error {
	token, expires, err := issueEmailToken(ctx, q, user, PurposeEmailVerification, emailVerificationTTL)
	if err != nil {
		return err
	}
	return s.queueMail(ctx, q, PurposeEmailVerification, user.Email, verificationEmail, emailContent{
		Instance: s.instanceName(ctx, q), Username: user.Username,
		Link: baseURL + "/verify-email?token=" + token, Expires: humanDuration(emailVerificationTTL),
	}, expires)
}

// VerifyEmail confirms an email address with a token from a verification email.
func (s *Service) VerifyEmail(ctx context.Context, token string) (store.User, error) {
	var user store.User
	err := s.tx(ctx, func(q *store.Queries) error {
		tok, err := q.ConsumeEmailToken(ctx, store.ConsumeEmailTokenParams{TokenHash: auth.HashAPIToken(token), Purpose: PurposeEmailVerification})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Invalid("this verification link is invalid, already used or expired; request a new one")
		}
		if err != nil {
			return err
		}
		user, err = q.MarkEmailVerified(ctx, store.MarkEmailVerifiedParams{ID: tok.UserID, Email: tok.Email})
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.Invalid("this link was sent to an address the account no longer uses")
		}
		return err
	})
	return user, err
}

// RunMailDelivery sends queued email until ctx is cancelled. Every replica runs it; row
// locks keep messages from being sent twice.
func (s *Service) RunMailDelivery(ctx context.Context) {
	poll := time.NewTicker(mailPoll)
	defer poll.Stop()
	for {
		for s.deliverMailBatch(ctx) == mailBatch && ctx.Err() == nil {
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		case <-s.mailWake:
		}
	}
}

func (s *Service) deliverMailBatch(ctx context.Context) int {
	sender := s.mailer()
	if sender == nil {
		return 0
	}
	claimed, err := s.q.ClaimMail(ctx, store.ClaimMailParams{LockSeconds: int32(mailLock / time.Second), Lim: mailBatch})
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("claiming queued email", "error", err)
		}
		return 0
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, m := range claimed {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			s.sendQueued(ctx, sender, m)
		})
	}
	wg.Wait()
	return len(claimed)
}

func (s *Service) sendQueued(ctx context.Context, sender mail.Sender, m store.MailOutbox) {
	sendCtx, cancel := context.WithTimeout(ctx, mailLock-30*time.Second)
	err := sender.Send(sendCtx, mail.Message{To: m.ToAddress, Subject: m.Subject, Text: m.TextBody, HTML: m.HtmlBody})
	cancel()
	if ctx.Err() != nil {
		return
	}
	params := store.FinishMailParams{ID: m.ID, Status: "sent", NextAttemptAt: m.NextAttemptAt}
	if err != nil {
		params.LastError = truncate(err.Error(), 1000)
		if m.Attempts >= MaxMailAttempts {
			params.Status = "failed"
			s.log.Error("giving up on email", "id", m.ID, "kind", m.Kind, "attempts", m.Attempts, "error", err)
		} else {
			params.Status = "pending"
			params.NextAttemptAt = time.Now().Add(mailBackoff[min(int(m.Attempts), len(mailBackoff))-1])
			s.log.Warn("sending email failed; will retry", "id", m.ID, "kind", m.Kind, "attempt", m.Attempts, "error", err)
		}
	}
	if err := s.q.FinishMail(ctx, params); err != nil && ctx.Err() == nil {
		s.log.Warn("recording email delivery", "id", m.ID, "error", err)
	}
}

// RunMaintenance periodically prunes expired email tokens, old outbox rows and uploads
// nothing references any more.
func (s *Service) RunMaintenance(ctx context.Context) {
	t := time.NewTicker(maintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.Maintain(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("maintenance", "error", err)
		}
	}
}

var maintenanceInterval = 10 * time.Minute

// Maintain runs one maintenance pass.
func (s *Service) Maintain(ctx context.Context) error {
	if _, err := s.q.ExpireMail(ctx); err != nil {
		return err
	}
	week := time.Now().Add(-7 * 24 * time.Hour)
	if _, err := s.q.DeleteOldMail(ctx, &week); err != nil {
		return err
	}
	if _, err := s.q.DeleteExpiredEmailTokens(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		return err
	}
	return s.pruneUploads(ctx, time.Now().Add(-uploadGrace))
}
