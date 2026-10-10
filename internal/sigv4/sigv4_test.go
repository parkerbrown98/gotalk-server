package sigv4

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Test vectors from the AWS documentation ("Signature Version 4 signing process" and
// "Authenticating Requests: Using Query Parameters").

func TestSignMatchesAWSExample(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	s := Signer{
		Credentials: Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"},
		Region:      "us-east-1", Service: "iam",
	}
	s.Sign(req, EmptyPayloadHash, time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	require.Equal(t, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, "+
		"SignedHeaders=content-type;host;x-amz-date, "+
		"Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7", req.Header.Get("Authorization"))
	require.Equal(t, "20150830T123600Z", req.Header.Get("X-Amz-Date"))
	require.Empty(t, req.Header.Get("X-Amz-Content-Sha256"), "only S3 needs the payload hash header")
}

func TestPresignMatchesAWSExample(t *testing.T) {
	u, err := url.Parse("https://examplebucket.s3.amazonaws.com/test.txt")
	require.NoError(t, err)
	s := Signer{
		Credentials: Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		Region:      "us-east-1", Service: "s3",
	}
	out := s.Presign(http.MethodGet, u, 24*time.Hour, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	require.Equal(t, "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404", out.Query().Get("X-Amz-Signature"))
	require.Equal(t, "AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request", out.Query().Get("X-Amz-Credential"))
	require.True(t, strings.HasPrefix(out.String(), "https://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm="))
}

func TestSignS3AddsPayloadHash(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, "http://localhost:9000/bucket/a%20b.txt", strings.NewReader("hi"))
	require.NoError(t, err)
	s := Signer{Credentials: Credentials{AccessKeyID: "k", SecretAccessKey: "s", SessionToken: "tok"}, Region: "us-east-1", Service: "s3"}
	s.Sign(req, UnsignedPayload, time.Now())
	require.Equal(t, UnsignedPayload, req.Header.Get("X-Amz-Content-Sha256"))
	require.Equal(t, "tok", req.Header.Get("X-Amz-Security-Token"))
	require.Contains(t, req.Header.Get("Authorization"), "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token,")
}

func TestEscapingAndCanonicalForms(t *testing.T) {
	require.Equal(t, "a%20b%2Bc~-_.%2F", Escape("a b+c~-_./"))
	u, _ := url.Parse("http://h/bucket/dir/file name.txt")
	require.Equal(t, "/bucket/dir/file%20name.txt", CanonicalPath(u))
	require.Equal(t, "/", CanonicalPath(&url.URL{}))
	require.Equal(t, "a=1&a=2&b=x%20y&list-type=2", CanonicalQuery(url.Values{"b": {"x y"}, "a": {"2", "1"}, "list-type": {"2"}}))
	require.Equal(t, "2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae", HashPayload([]byte("foo")))
}
