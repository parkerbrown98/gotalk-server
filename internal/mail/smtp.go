package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

const (
	smtpTLSStartTLS = "starttls"
	smtpTLSImplicit = "tls"
	smtpTLSNone     = "none"
)

var smtpTLSConfigHook func(*tls.Config)

type smtpSender struct {
	host     string
	port     int
	tlsMode  string
	username string
	password string
	from     *mail.Address
}

func init() {
	Register("smtp", newSMTPSender)
}

func newSMTPSender(s Settings, _ Env) (Sender, error) {
	if err := requireSetting(s.SMTPHost, "smtp_host", "smtp"); err != nil {
		return nil, err
	}
	from, err := requireFrom(s, "smtp")
	if err != nil {
		return nil, err
	}
	mode := strings.ToLower(strings.TrimSpace(s.SMTPTLS))
	if mode == "" {
		mode = smtpTLSStartTLS
	}
	switch mode {
	case smtpTLSStartTLS, smtpTLSImplicit, smtpTLSNone:
	default:
		return nil, fmt.Errorf("mail.smtp_tls must be starttls, tls, none or empty for the smtp driver")
	}
	port := s.SMTPPort
	if port == 0 {
		port = defaultSMTPPort(mode)
	}
	return &smtpSender{
		host:     s.SMTPHost,
		port:     port,
		tlsMode:  mode,
		username: s.SMTPUsername,
		password: s.SMTPPassword,
		from:     from,
	}, nil
}

func (s *smtpSender) Driver() string {
	return "smtp"
}

func (s *smtpSender) Describe() string {
	return "smtp " + net.JoinHostPort(s.host, strconv.Itoa(s.port)) + " (" + s.tlsMode + ")"
}

func (s *smtpSender) Send(ctx context.Context, msg Message) error {
	to, err := validateMessage("smtp", msg)
	if err != nil {
		return err
	}
	client, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := s.authenticate(client); err != nil {
		return err
	}
	if err := client.Mail(s.from.Address); err != nil {
		return fmt.Errorf("smtp: MAIL FROM failed: %w", err)
	}
	if err := client.Rcpt(to.Address); err != nil {
		return fmt.Errorf("smtp: RCPT TO failed: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA failed: %w", err)
	}
	raw, err := buildMIMEMessage(s.from, to, msg)
	if err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp: build message: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: finish message: %w", err)
	}
	// The server accepted the message; a failed QUIT must not cause a resend.
	_ = client.Quit()
	return nil
}

func (s *smtpSender) Check(ctx context.Context) error {
	client, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := s.authenticate(client); err != nil {
		return err
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("smtp: QUIT failed: %w", err)
	}
	return nil
}

func (s *smtpSender) connect(ctx context.Context) (*smtp.Client, error) {
	address := net.JoinHostPort(s.host, strconv.Itoa(s.port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	cfg := &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}
	if smtpTLSConfigHook != nil {
		smtpTLSConfigHook(cfg)
	}

	var (
		conn net.Conn
		err  error
	)
	if s.tlsMode == smtpTLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: cfg}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, fmt.Errorf("smtp: connect to %s: %w", address, err)
	}
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("smtp: set connection deadline: %w", err)
	}
	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("smtp: read greeting: %w", err)
	}
	if err := client.Hello("localhost"); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("smtp: EHLO failed: %w", err)
	}
	if s.tlsMode == smtpTLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			_ = client.Close()
			return nil, fmt.Errorf("smtp: STARTTLS is required but the server does not advertise STARTTLS")
		}
		if err := client.StartTLS(cfg); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("smtp: STARTTLS failed: %w", err)
		}
	}
	return client, nil
}

func (s *smtpSender) authenticate(client *smtp.Client) error {
	if s.username == "" {
		return nil
	}
	_, authLine := client.Extension("AUTH")
	mechanisms := strings.Fields(strings.ToUpper(authLine))
	var auth smtp.Auth
	switch {
	case containsMechanism(mechanisms, "PLAIN"):
		auth = smtp.PlainAuth("", s.username, s.password, s.host)
	case containsMechanism(mechanisms, "LOGIN"):
		auth = &loginAuth{username: s.username, password: s.password}
	case containsMechanism(mechanisms, "CRAM-MD5"):
		auth = smtp.CRAMMD5Auth(s.username, s.password)
	default:
		return fmt.Errorf("smtp: AUTH is required but the server does not advertise PLAIN, LOGIN or CRAM-MD5")
	}
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("smtp: AUTH failed: %w", err)
	}
	return nil
}

func defaultSMTPPort(mode string) int {
	switch mode {
	case smtpTLSImplicit:
		return 465
	case smtpTLSNone:
		return 25
	default:
		return 587
	}
}

func containsMechanism(mechanisms []string, want string) bool {
	for _, mech := range mechanisms {
		if mech == want {
			return true
		}
	}
	return false
}

type loginAuth struct {
	username string
	password string
	step     int
}

func (a *loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	a.step = 0
	return "LOGIN", []byte(a.username), nil
}

func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	if a.step == 1 {
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected extra LOGIN challenge")
}
