package mail

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSMTPSendPlainAuthNoTLS(t *testing.T) {
	server := newFakeSMTP(t, []string{"PLAIN"})
	defer server.Close()

	sender := openTestSender(t, Settings{
		Driver:       "smtp",
		From:         testFrom,
		SMTPHost:     server.Host(),
		SMTPPort:     server.Port(),
		SMTPTLS:      "none",
		SMTPUsername: "user",
		SMTPPassword: "pass",
	})
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	require.Contains(t, server.Auths(), "PLAIN")
	require.Contains(t, server.Message(), "Subject: Subject\r\n")
	require.Contains(t, server.Message(), "Content-Type: multipart/alternative;")
}

func TestSMTPLoginFallback(t *testing.T) {
	server := newFakeSMTP(t, []string{"LOGIN"})
	defer server.Close()

	sender := openTestSender(t, Settings{
		Driver:       "smtp",
		From:         testFrom,
		SMTPHost:     server.Host(),
		SMTPPort:     server.Port(),
		SMTPTLS:      "none",
		SMTPUsername: "user",
		SMTPPassword: "pass",
	})
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	require.Contains(t, server.Auths(), "LOGIN")
}

func TestSMTPCheck(t *testing.T) {
	server := newFakeSMTP(t, []string{"PLAIN"})
	defer server.Close()

	sender := openTestSender(t, Settings{
		Driver:       "smtp",
		From:         testFrom,
		SMTPHost:     server.Host(),
		SMTPPort:     server.Port(),
		SMTPTLS:      "none",
		SMTPUsername: "user",
		SMTPPassword: "pass",
	})
	require.NoError(t, sender.Check(context.Background()))
	require.Empty(t, server.Message())
}

func TestSMTPStartTLSRequiredButNotOffered(t *testing.T) {
	server := newFakeSMTP(t, nil)
	defer server.Close()

	sender := openTestSender(t, Settings{
		Driver:   "smtp",
		From:     testFrom,
		SMTPHost: server.Host(),
		SMTPPort: server.Port(),
	})
	err := sender.Check(context.Background())
	require.ErrorContains(t, err, "STARTTLS is required")
}

type fakeSMTP struct {
	t          *testing.T
	listener   net.Listener
	mechanisms []string
	wg         sync.WaitGroup
	mu         sync.Mutex
	auths      []string
	message    string
}

func newFakeSMTP(t *testing.T, mechanisms []string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &fakeSMTP{t: t, listener: ln, mechanisms: mechanisms}
	server.wg.Add(1)
	go server.serve()
	return server
}

func (s *fakeSMTP) Host() string {
	host, _, err := net.SplitHostPort(s.listener.Addr().String())
	require.NoError(s.t, err)
	return host
}

func (s *fakeSMTP) Port() int {
	_, port, err := net.SplitHostPort(s.listener.Addr().String())
	require.NoError(s.t, err)
	var out int
	_, err = fmt.Sscanf(port, "%d", &out)
	require.NoError(s.t, err)
	return out
}

func (s *fakeSMTP) Close() {
	_ = s.listener.Close()
	s.wg.Wait()
}

func (s *fakeSMTP) Auths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}

func (s *fakeSMTP) Message() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.message
}

func (s *fakeSMTP) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	writeSMTP(w, "220 localhost ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			writeSMTPRaw(w, "250-localhost\r\n")
			if len(s.mechanisms) > 0 {
				writeSMTP(w, "250 AUTH "+strings.Join(s.mechanisms, " "))
			} else {
				writeSMTP(w, "250 OK")
			}
		case strings.HasPrefix(upper, "AUTH PLAIN"):
			s.recordAuth("PLAIN")
			writeSMTP(w, "235 2.7.0 Authentication successful")
		case strings.HasPrefix(upper, "AUTH LOGIN"):
			s.recordAuth("LOGIN")
			writeSMTP(w, "334 UGFzc3dvcmQ6")
			passLine, err := r.ReadString('\n')
			if err != nil {
				return
			}
			decoded, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(passLine))
			if string(decoded) != "pass" {
				writeSMTP(w, "535 invalid")
				continue
			}
			writeSMTP(w, "235 2.7.0 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			writeSMTP(w, "250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			writeSMTP(w, "250 OK")
		case upper == "DATA":
			writeSMTP(w, "354 End data with <CR><LF>.<CR><LF>")
			var b strings.Builder
			for {
				dataLine, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dataLine == ".\r\n" {
					break
				}
				b.WriteString(dataLine)
			}
			s.mu.Lock()
			s.message = b.String()
			s.mu.Unlock()
			writeSMTP(w, "250 queued")
		case upper == "QUIT":
			writeSMTP(w, "221 bye")
			return
		default:
			writeSMTP(w, "250 OK")
		}
	}
}

func (s *fakeSMTP) recordAuth(mech string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auths = append(s.auths, mech)
}

func writeSMTP(w *bufio.Writer, line string) {
	writeSMTPRaw(w, line+"\r\n")
}

func writeSMTPRaw(w *bufio.Writer, text string) {
	_, _ = w.WriteString(text)
	_ = w.Flush()
}
