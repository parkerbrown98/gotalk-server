package mail

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildMIMEMessage(t *testing.T) {
	from, err := mail.ParseAddress("Gotalk <noreply@example.com>")
	require.NoError(t, err)
	to, err := mail.ParseAddress("User <user@example.net>")
	require.NoError(t, err)
	random := bytes.NewReader(bytes.Repeat([]byte{0x12}, 28))

	raw, err := buildMIMEMessageAt(from, to, Message{
		To:      to.String(),
		Subject: "Hello ☃",
		Text:    "hello = text\nsecond line",
		HTML:    "<strong>hello</strong>",
	}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), random)
	require.NoError(t, err)
	rawText := string(raw)
	requireNoBareLF(t, rawText)
	require.Contains(t, rawText, "Subject: =?UTF-8?q?Hello_=E2=98=83?=")
	require.Contains(t, rawText, "Content-Transfer-Encoding: quoted-printable")

	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	subject, err := (&mime.WordDecoder{}).DecodeHeader(parsed.Header.Get("Subject"))
	require.NoError(t, err)
	require.Equal(t, "Hello ☃", subject)
	require.Equal(t, "<12121212121212121212121212121212@example.com>", parsed.Header.Get("Message-ID"))

	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/alternative", mediaType)
	reader := multipart.NewReader(parsed.Body, params["boundary"])

	part, err := reader.NextPart()
	require.NoError(t, err)
	require.Equal(t, "text/plain; charset=UTF-8", part.Header.Get("Content-Type"))
	textBody, err := io.ReadAll(quotedprintable.NewReader(part))
	require.NoError(t, err)
	require.Equal(t, "hello = text\r\nsecond line", string(textBody))

	part, err = reader.NextPart()
	require.NoError(t, err)
	require.Equal(t, "text/html; charset=UTF-8", part.Header.Get("Content-Type"))
	htmlBody, err := io.ReadAll(quotedprintable.NewReader(part))
	require.NoError(t, err)
	require.Equal(t, "<strong>hello</strong>", string(htmlBody))

	_, err = reader.NextPart()
	require.ErrorIs(t, err, io.EOF)
}

func TestValidateMessageRejectsHeaderInjectionAndBadTo(t *testing.T) {
	_, err := validateMessage("test", Message{To: "user@example.com\r\nBcc: x@example.com", Subject: "ok"})
	require.ErrorContains(t, err, "test")
	require.ErrorContains(t, err, "to contains a newline")

	_, err = validateMessage("test", Message{To: "user@example.com", Subject: "hi\nBcc: x@example.com"})
	require.ErrorContains(t, err, "subject contains a newline")

	_, err = validateMessage("test", Message{To: "not an address", Subject: "ok"})
	require.ErrorContains(t, err, "not a valid address")
}

func requireNoBareLF(t *testing.T, s string) {
	t.Helper()
	for i, c := range s {
		if c == '\n' {
			require.NotZero(t, i, "message starts with LF")
			require.Equal(t, byte('\r'), s[i-1], "found bare LF in message")
		}
	}
}
