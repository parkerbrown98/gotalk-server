package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"
)

const crlf = "\r\n"

func validateMessage(provider string, msg Message) (*mail.Address, error) {
	if strings.ContainsAny(msg.To, "\r\n") {
		return nil, fmt.Errorf("%s: message to contains a newline", provider)
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		return nil, fmt.Errorf("%s: message subject contains a newline", provider)
	}
	to, err := mail.ParseAddress(msg.To)
	if err != nil {
		return nil, fmt.Errorf("%s: message to %q is not a valid address: %w", provider, msg.To, err)
	}
	return to, nil
}

func buildMIMEMessage(from, to *mail.Address, msg Message) ([]byte, error) {
	return buildMIMEMessageAt(from, to, msg, time.Now(), rand.Reader)
}

func buildMIMEMessageAt(from, to *mail.Address, msg Message, now time.Time, random io.Reader) ([]byte, error) {
	messageID, err := newMessageID(from.Address, random)
	if err != nil {
		return nil, err
	}
	boundary, err := randomBoundary(random)
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	writeHeader(&b, "From", from.String())
	writeHeader(&b, "To", to.String())
	writeHeader(&b, "Subject", encodeHeader(msg.Subject))
	writeHeader(&b, "Date", now.Format(time.RFC1123Z))
	writeHeader(&b, "Message-ID", messageID)
	writeHeader(&b, "MIME-Version", "1.0")
	writeHeader(&b, "Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString(crlf)

	writePart(&b, boundary, "text/plain", msg.Text)
	if msg.HTML != "" {
		writePart(&b, boundary, "text/html", msg.HTML)
	}
	b.WriteString("--" + boundary + "--" + crlf)
	return b.Bytes(), nil
}

func writeHeader(b *bytes.Buffer, name, value string) {
	b.WriteString(name)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString(crlf)
}

func writePart(b *bytes.Buffer, boundary, contentType, body string) {
	b.WriteString("--" + boundary + crlf)
	writeHeader(b, "Content-Type", contentType+"; charset=UTF-8")
	writeHeader(b, "Content-Transfer-Encoding", "quoted-printable")
	b.WriteString(crlf)
	qp := quotedprintable.NewWriter(b)
	_, _ = qp.Write([]byte(body))
	_ = qp.Close()
	b.WriteString(crlf)
}

func encodeHeader(value string) string {
	if value == "" || utf8.RuneCountInString(value) == len(value) {
		return value
	}
	return mime.QEncoding.Encode("UTF-8", value)
}

func newMessageID(from string, random io.Reader) (string, error) {
	at := strings.LastIndexByte(from, '@')
	if at < 0 || at == len(from)-1 {
		return "", fmt.Errorf("mail: from address %q has no domain", from)
	}
	buf := make([]byte, 16)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("mail: generate message id: %w", err)
	}
	return "<" + hex.EncodeToString(buf) + "@" + from[at+1:] + ">", nil
}

func randomBoundary(random io.Reader) (string, error) {
	buf := make([]byte, 12)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("mail: generate mime boundary: %w", err)
	}
	return "gotalk-" + hex.EncodeToString(buf), nil
}
