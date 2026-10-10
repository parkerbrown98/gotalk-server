package mail

import (
	"context"
	"net/mail"
)

const logDefaultFrom = "Gotalk <noreply@localhost>"

type logSender struct {
	env  Env
	from *mail.Address
}

func init() {
	Register("log", newLogSender)
}

func newLogSender(s Settings, env Env) (Sender, error) {
	fromText := s.From
	if fromText == "" {
		fromText = logDefaultFrom
	}
	from, err := mail.ParseAddress(fromText)
	if err != nil {
		return nil, err
	}
	return &logSender{env: env, from: from}, nil
}

func (s *logSender) Driver() string {
	return "log"
}

func (s *logSender) Describe() string {
	return "log (emails are written to the server log, not sent)"
}

func (s *logSender) Send(_ context.Context, msg Message) error {
	if _, err := validateMessage("log", msg); err != nil {
		return err
	}
	s.env.Logger.Info("email (log driver; not sent)", "from", s.from.String(), "to", msg.To, "subject", msg.Subject, "text", msg.Text)
	return nil
}

func (s *logSender) Check(_ context.Context) error {
	return nil
}
