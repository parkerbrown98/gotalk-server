package storage

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/parkerbrown98/gotalk-server/internal/sigv4"
)

const (
	defaultS3Region = "us-east-1"
	s3MaxKeys       = "1000"
)

func init() {
	Register("s3", openS3)
}

type s3Backend struct {
	endpoint *url.URL
	region   string
	bucket   string
	prefix   string
	pathMode bool
	client   *http.Client
	signer   sigv4.Signer
}

func openS3(s Settings) (Backend, error) {
	if s.S3Bucket == "" {
		return nil, errors.New("storage.s3_bucket is required for the s3 driver")
	}
	if s.S3AccessKeyID == "" {
		return nil, errors.New("storage.s3_access_key_id is required for the s3 driver")
	}
	if s.S3SecretAccessKey == "" {
		return nil, errors.New("storage.s3_secret_access_key is required for the s3 driver")
	}
	region := s.S3Region
	if region == "" {
		region = defaultS3Region
	}
	endpoint := s.S3Endpoint
	if endpoint == "" {
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing storage.s3_endpoint: %w", err)
	}
	if !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("storage.s3_endpoint must be an absolute http or https URL")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("storage.s3_endpoint must not include a path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("storage.s3_endpoint must not include a query string or fragment")
	}
	u.Path = ""
	u.RawPath = ""

	prefix, err := normalizeS3Prefix(s.S3Prefix)
	if err != nil {
		return nil, err
	}

	return &s3Backend{
		endpoint: u,
		region:   region,
		bucket:   s.S3Bucket,
		prefix:   prefix,
		pathMode: s.S3ForcePathStyle,
		client:   newS3HTTPClient(),
		signer: sigv4.Signer{
			Credentials: sigv4.Credentials{AccessKeyID: s.S3AccessKeyID, SecretAccessKey: s.S3SecretAccessKey},
			Region:      region,
			Service:     "s3",
		},
	}, nil
}

func (b *s3Backend) Driver() string {
	return "s3"
}

func (b *s3Backend) Describe() string {
	desc := "s3 bucket " + b.bucket + " at " + b.endpoint.String()
	if b.prefix != "" {
		desc += " prefix " + b.prefix
	}
	return desc
}

func (b *s3Backend) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	if contentType == "" {
		contentType = contentTypeForKey(key)
	}
	req, err := b.newObjectRequest(ctx, http.MethodPut, key, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	b.signer.Sign(req, sigv4.UnsignedPayload, time.Now())
	res, err := b.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return s3ResponseError(res, http.MethodPut, key)
	}
	return nil
}

func (b *s3Backend) Get(ctx context.Context, key string) (io.ReadCloser, Object, error) {
	if err := CheckKey(key); err != nil {
		return nil, Object{}, err
	}
	req, err := b.newObjectRequest(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, Object{}, err
	}
	b.signer.Sign(req, sigv4.UnsignedPayload, time.Now())
	res, err := b.do(req)
	if err != nil {
		return nil, Object{}, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		defer func() { _ = res.Body.Close() }()
		return nil, Object{}, s3ResponseError(res, http.MethodGet, key)
	}
	return res.Body, b.objectFromHeaders(key, res.Header, res.ContentLength), nil
}

func (b *s3Backend) Stat(ctx context.Context, key string) (Object, error) {
	if err := CheckKey(key); err != nil {
		return Object{}, err
	}
	req, err := b.newObjectRequest(ctx, http.MethodHead, key, nil)
	if err != nil {
		return Object{}, err
	}
	b.signer.Sign(req, sigv4.UnsignedPayload, time.Now())
	res, err := b.do(req)
	if err != nil {
		return Object{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return Object{}, s3ResponseError(res, http.MethodHead, key)
	}
	return b.objectFromHeaders(key, res.Header, res.ContentLength), nil
}

func (b *s3Backend) Delete(ctx context.Context, key string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	req, err := b.newObjectRequest(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	b.signer.Sign(req, sigv4.UnsignedPayload, time.Now())
	res, err := b.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusNotFound {
		return nil
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return s3ResponseError(res, http.MethodDelete, key)
	}
	return nil
}

func (b *s3Backend) List(ctx context.Context, prefix string, fn func(Object) error) error {
	if err := checkListPrefix(prefix); err != nil {
		return err
	}
	token := ""
	for {
		req, err := b.newListRequest(ctx, prefix, token)
		if err != nil {
			return err
		}
		res, err := b.do(req)
		if err != nil {
			return err
		}
		if res.StatusCode < 200 || res.StatusCode > 299 {
			err = s3ResponseError(res, http.MethodGet, "list "+prefix)
			_ = res.Body.Close()
			return err
		}
		var out s3ListBucketResult
		err = xml.NewDecoder(res.Body).Decode(&out)
		closeErr := res.Body.Close()
		if err != nil {
			return fmt.Errorf("s3: decoding list response: %w", err)
		}
		if closeErr != nil {
			return fmt.Errorf("s3: closing list response: %w", closeErr)
		}
		for _, item := range out.Contents {
			if !strings.HasPrefix(item.Key, b.prefix) {
				continue
			}
			key := strings.TrimPrefix(item.Key, b.prefix)
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if err := fn(Object{
				Key:         key,
				Size:        item.Size,
				ContentType: contentTypeForKey(key),
				ModTime:     item.LastModified.Time,
			}); err != nil {
				return err
			}
		}
		if !out.IsTruncated || out.NextContinuationToken == "" {
			return nil
		}
		token = out.NextContinuationToken
	}
}

func (b *s3Backend) newObjectRequest(ctx context.Context, method, key string, body io.Reader) (*http.Request, error) {
	u := b.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("creating s3 request: %w", err)
	}
	return req, nil
}

func (b *s3Backend) newListRequest(ctx context.Context, prefix, token string) (*http.Request, error) {
	u := b.bucketURL()
	q := u.Query()
	q.Set("list-type", "2")
	q.Set("max-keys", s3MaxKeys)
	q.Set("prefix", b.prefix+prefix)
	if token != "" {
		q.Set("continuation-token", token)
	}
	u.RawQuery = sigv4.CanonicalQuery(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("creating s3 list request: %w", err)
	}
	b.signer.Sign(req, sigv4.UnsignedPayload, time.Now())
	return req, nil
}

func (b *s3Backend) do(req *http.Request) (*http.Response, error) {
	res, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3: %s %s: %w", req.Method, req.URL.Path, err)
	}
	return res, nil
}

func (b *s3Backend) objectURL(key string) *url.URL {
	return b.urlForKey(b.prefix + key)
}

func (b *s3Backend) bucketURL() *url.URL {
	u := *b.endpoint
	if b.pathMode {
		u.Path, u.RawPath = escapedPath(b.bucket)
		return &u
	}
	u.Host = b.bucket + "." + u.Host
	u.Path = "/"
	u.RawPath = ""
	return &u
}

func (b *s3Backend) urlForKey(fullKey string) *url.URL {
	u := *b.endpoint
	if b.pathMode {
		u.Path, u.RawPath = escapedPath(b.bucket, fullKey)
		return &u
	}
	u.Host = b.bucket + "." + u.Host
	u.Path, u.RawPath = escapedPath(fullKey)
	return &u
}

func escapedPath(parts ...string) (string, string) {
	var plain, escaped strings.Builder
	for _, part := range parts {
		plain.WriteByte('/')
		escaped.WriteByte('/')
		segs := strings.Split(part, "/")
		for i, seg := range segs {
			if i > 0 {
				plain.WriteByte('/')
				escaped.WriteByte('/')
			}
			plain.WriteString(seg)
			escaped.WriteString(sigv4.Escape(seg))
		}
	}
	if plain.Len() == 0 {
		return "/", ""
	}
	return plain.String(), escaped.String()
}

func (b *s3Backend) objectFromHeaders(key string, h http.Header, size int64) Object {
	contentType := h.Get("Content-Type")
	if contentType == "" {
		contentType = contentTypeForKey(key)
	}
	var mod time.Time
	if lastModified := h.Get("Last-Modified"); lastModified != "" {
		mod, _ = http.ParseTime(lastModified)
	}
	return Object{Key: key, Size: size, ContentType: contentType, ModTime: mod}
}

func newS3HTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
		},
	}
}

func normalizeS3Prefix(prefix string) (string, error) {
	prefix = strings.TrimLeft(prefix, "/")
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return "", nil
	}
	if !ValidKey(prefix) {
		return "", fmt.Errorf("storage: invalid s3_prefix %q", prefix)
	}
	return prefix + "/", nil
}

type s3ListBucketResult struct {
	IsTruncated           bool              `xml:"IsTruncated"`
	NextContinuationToken string            `xml:"NextContinuationToken"`
	Contents              []s3ObjectContent `xml:"Contents"`
}

type s3ObjectContent struct {
	Key          string `xml:"Key"`
	LastModified s3Time `xml:"LastModified"`
	Size         int64  `xml:"Size"`
}

type s3Time struct {
	time.Time
}

func (t *s3Time) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, string(text))
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

type s3ErrorPayload struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

type s3Error struct {
	method   string
	resource string
	code     string
	message  string
	status   int
	notFound bool
}

func (e *s3Error) Error() string {
	code := e.code
	if code == "" {
		code = http.StatusText(e.status)
	}
	message := e.message
	if message == "" {
		message = http.StatusText(e.status)
	}
	return fmt.Sprintf("s3: %s %s: %s: %s (HTTP %d)", e.method, e.resource, code, message, e.status)
}

func (e *s3Error) Unwrap() error {
	if e.notFound {
		return ErrNotFound
	}
	return nil
}

func s3ResponseError(res *http.Response, method, resource string) error {
	payload := s3ErrorPayload{}
	if res.Body != nil {
		limited := io.LimitReader(res.Body, 64*1024)
		_ = xml.NewDecoder(limited).Decode(&payload)
	}
	notFound := res.StatusCode == http.StatusNotFound || payload.Code == "NoSuchKey" || payload.Code == "NoSuchBucket"
	return &s3Error{
		method:   method,
		resource: resource,
		code:     payload.Code,
		message:  payload.Message,
		status:   res.StatusCode,
		notFound: notFound,
	}
}
