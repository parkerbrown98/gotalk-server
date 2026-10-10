package mail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDriversAndFactoryValidation(t *testing.T) {
	require.Equal(t, []string{"log", "mailgun", "postmark", "resend", "sendgrid", "ses", "smtp"}, Drivers())

	sender, err := Open(Settings{}, Env{})
	require.NoError(t, err)
	require.Nil(t, sender)

	for _, tc := range []struct {
		name     string
		settings Settings
		wantErr  string
	}{
		{name: "sendgrid key", settings: Settings{Driver: "sendgrid", From: testFrom}, wantErr: "mail.api_key is required for the sendgrid driver"},
		{name: "mailgun domain", settings: Settings{Driver: "mailgun", From: testFrom, APIKey: "key"}, wantErr: "mail.domain is required for the mailgun driver"},
		{name: "smtp host", settings: Settings{Driver: "smtp", From: testFrom}, wantErr: "mail.smtp_host is required for the smtp driver"},
		{name: "ses region", settings: Settings{Driver: "ses", From: testFrom, AccessKeyID: "ak", SecretAccessKey: "sk"}, wantErr: "mail.region is required for the ses driver"},
		{name: "from", settings: Settings{Driver: "resend", APIKey: "key"}, wantErr: "mail.from is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(tc.settings, Env{})
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestSendgridSendAndCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer sg-key", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v3/mail/send":
			require.Equal(t, http.MethodPost, r.Method)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "Subject", body["subject"])
			w.WriteHeader(http.StatusAccepted)
		case "/v3/scopes":
			require.Equal(t, http.MethodGet, r.Method)
			_, _ = w.Write([]byte(`{"scopes":["mail.send"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	sender := openTestSender(t, Settings{Driver: "sendgrid", From: testFrom, APIKey: "sg-key", APIURL: server.URL})
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	require.NoError(t, sender.Check(context.Background()))
}

func TestSendgridCheckMissingScopeAndErrorMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/scopes":
			_, _ = w.Write([]byte(`{"scopes":["marketing.send"]}`))
		case "/v3/mail/send":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"bad request"}]}`))
		}
	}))
	defer server.Close()
	sender := openTestSender(t, Settings{Driver: "sendgrid", From: testFrom, APIKey: "sg-secret", APIURL: server.URL})

	require.ErrorContains(t, sender.Check(context.Background()), "missing mail.send scope")
	err := sender.Send(context.Background(), testMessage())
	require.ErrorContains(t, err, "sendgrid: provider returned HTTP 400: bad request")
	require.NotContains(t, err.Error(), "sg-secret")
}

func TestMailgunSendAndCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		require.Equal(t, "api", user)
		require.Equal(t, "mg-key", pass)
		switch r.URL.Path {
		case "/v3/example.com/messages":
			require.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, r.ParseForm())
			require.Equal(t, `"Gotalk" <from@example.com>`, r.Form.Get("from"))
			require.Equal(t, "to@example.com", r.Form.Get("to"))
			require.Equal(t, "Subject", r.Form.Get("subject"))
		case "/v3/domains/example.com":
			require.Equal(t, http.MethodGet, r.Method)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	sender := openTestSender(t, Settings{Driver: "mailgun", From: testFrom, APIKey: "mg-key", Domain: "example.com", APIURL: server.URL + "/"})
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	require.NoError(t, sender.Check(context.Background()))
}

func TestPostmarkSendCheckAndErrorCode(t *testing.T) {
	failSend := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "pm-key", r.Header.Get("X-Postmark-Server-Token"))
		switch r.URL.Path {
		case "/email":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "outbound", body["MessageStream"])
			if failSend {
				_, _ = w.Write([]byte(`{"ErrorCode":300,"Message":"Inactive recipient"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ErrorCode":0,"Message":"OK"}`))
		case "/server":
			_, _ = w.Write([]byte(`{"Name":"test"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	sender := openTestSender(t, Settings{Driver: "postmark", From: testFrom, APIKey: "pm-key", APIURL: server.URL})
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	require.NoError(t, sender.Check(context.Background()))
	failSend = true
	require.ErrorContains(t, sender.Send(context.Background(), testMessage()), "postmark: provider returned HTTP 200: Inactive recipient")
}

func TestResendSendAndRestrictedKeyCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer rs-key", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/emails":
			require.Equal(t, http.MethodPost, r.Method)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, []any{"to@example.com"}, body["to"])
			w.WriteHeader(http.StatusCreated)
		case "/domains":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"name":"restricted_api_key","message":"restricted"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	sender := openTestSender(t, Settings{Driver: "resend", From: testFrom, APIKey: "rs-key", APIURL: server.URL})
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	require.NoError(t, sender.Check(context.Background()))
}

func TestSESSignsSendAndCheck(t *testing.T) {
	const region = "us-east-2"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		require.True(t, strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKID/"), auth)
		require.Contains(t, auth, "/"+region+"/ses/aws4_request")
		switch r.URL.Path {
		case "/v2/email/outbound-emails":
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "application/json", r.Header.Get("Content-Type"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, `"Gotalk" <from@example.com>`, body["FromEmailAddress"])
		case "/v2/email/account":
			require.Equal(t, http.MethodGet, r.Method)
			_, _ = w.Write([]byte(`{"ProductionAccessEnabled":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	sender := openTestSender(t, Settings{
		Driver:          "ses",
		From:            testFrom,
		Region:          region,
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
		APIURL:          server.URL,
	})
	require.NoError(t, sender.Send(context.Background(), testMessage()))
	require.NoError(t, sender.Check(context.Background()))
}

func openTestSender(t *testing.T, settings Settings) Sender {
	t.Helper()
	sender, err := Open(settings, Env{})
	require.NoError(t, err)
	require.NotNil(t, sender)
	return sender
}

func testMessage() Message {
	return Message{To: "to@example.com", Subject: "Subject", Text: "Text", HTML: "<b>Text</b>"}
}

const testFrom = "Gotalk <from@example.com>"
