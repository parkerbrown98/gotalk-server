// Package unfurl fetches link previews: OpenGraph, Twitter card and HTML metadata for web
// pages, and the bytes of directly linked or preview images. Requests only reach public
// addresses (checked after DNS resolution, on every redirect) unless AllowPrivate is set,
// so users cannot make the server probe its own network.
package unfurl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// Kinds of preview.
const (
	KindLink  = "link"
	KindImage = "image"
)

const (
	maxHTMLBytes   = 1 << 20
	maxRedirects   = 5
	maxTitle       = 256
	maxDesc        = 500
	maxSiteName    = 100
	requestTimeout = 10 * time.Second
)

// ErrBlocked is returned when a URL resolves to an address previews may not fetch.
var ErrBlocked = errors.New("address not allowed")

// Result is what a URL previews as. Image holds the raw bytes of the linked image (KindImage)
// or of the page's preview image, if one could be fetched.
type Result struct {
	Kind        string
	FinalURL    string
	SiteName    string
	Title       string
	Description string
	ThemeColor  string
	LargeImage  bool
	Image       []byte
}

// Client fetches previews. Create one with New.
type Client struct {
	http     *http.Client
	maxImage int64
	agent    string
}

// Options configures a Client.
type Options struct {
	// AllowPrivate permits loopback, private and other non-public addresses. Only for
	// development and tests.
	AllowPrivate bool
	// MaxImage is the largest image downloaded, in bytes.
	MaxImage int64
	// UserAgent identifies the fetcher to sites.
	UserAgent string
}

// New returns a Client.
func New(o Options) *Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	if !o.AllowPrivate {
		dialer.Control = guard
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
	}
	agent := o.UserAgent
	if agent == "" {
		agent = "Mozilla/5.0 (compatible; GotalkBot/1.0; link previews)"
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return errors.New("too many redirects")
				}
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return errors.New("redirect to a non-web URL")
				}
				return nil
			},
		},
		maxImage: o.MaxImage,
		agent:    agent,
	}
}

// guard rejects connections to addresses that are not publicly routable. It runs after
// DNS resolution, so a hostname cannot point the fetcher at an internal service.
func guard(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	if !Public(ip) {
		return ErrBlocked
	}
	return nil
}

var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach private IPv4 ranges
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), // 6to4 embeds arbitrary IPv4 addresses
}

// Public reports whether ip is a globally routable unicast address.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// Fetch previews rawURL.
func (c *Client) Fetch(ctx context.Context, rawURL string) (Result, error) {
	resp, err := c.get(ctx, rawURL, "text/html,application/xhtml+xml;q=0.9,image/*;q=0.8,*/*;q=0.5")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	final := resp.Request.URL
	ct, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case strings.HasPrefix(ct, "image/"):
		data, err := c.readImage(resp)
		if err != nil {
			return Result{}, err
		}
		return Result{Kind: KindImage, FinalURL: final.String(), SiteName: siteName(final), Image: data}, nil
	case ct == "text/html" || ct == "application/xhtml+xml":
	default:
		return Result{}, fmt.Errorf("unsupported content type %q", ct)
	}

	body := io.LimitReader(resp.Body, maxHTMLBytes)
	if r, err := charset.NewReader(body, mime.FormatMediaType(ct, params)); err == nil {
		body = r
	}
	m := parseMeta(body)
	res := Result{Kind: KindLink, FinalURL: final.String()}
	res.Title = clip(first(m["og:title"], m["twitter:title"], m["title"]), maxTitle)
	res.Description = clip(first(m["og:description"], m["twitter:description"], m["description"]), maxDesc)
	res.SiteName = clip(first(m["og:site_name"], m["application-name"]), maxSiteName)
	if res.SiteName == "" {
		res.SiteName = siteName(final)
	}
	if color := strings.TrimSpace(m["theme-color"]); hexColor.MatchString(color) {
		res.ThemeColor = strings.ToLower(color)
	}
	res.LargeImage = strings.EqualFold(strings.TrimSpace(m["twitter:card"]), "summary_large_image")
	if img := first(m["og:image:secure_url"], m["og:image"], m["og:image:url"], m["twitter:image"], m["twitter:image:src"]); img != "" {
		if u, err := final.Parse(img); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			res.Image, _ = c.fetchImage(ctx, u.String())
		}
	}
	if res.Title == "" && res.Image == nil {
		return Result{}, errors.New("the page has no title or preview image")
	}
	return res, nil
}

func (c *Client) get(ctx context.Context, rawURL, accept string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("not a web URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "en;q=0.9, *;q=0.5")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp, nil
}

func (c *Client) fetchImage(ctx context.Context, rawURL string) ([]byte, error) {
	resp, err := c.get(ctx, rawURL, "image/*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); !strings.HasPrefix(ct, "image/") {
		return nil, fmt.Errorf("unexpected content type %q", ct)
	}
	return c.readImage(resp)
}

func (c *Client) readImage(resp *http.Response) ([]byte, error) {
	if resp.ContentLength > c.maxImage {
		return nil, errors.New("image too large")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxImage+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > c.maxImage {
		return nil, errors.New("image too large")
	}
	return data, nil
}

var hexColor = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// parseMeta collects <meta> values and the <title> from a document's head. Keys are
// lowercased; the first value of each wins.
func parseMeta(r io.Reader) map[string]string {
	out := map[string]string{}
	z := html.NewTokenizer(r)
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "body":
				return out
			case "title":
				inTitle = true
			case "meta":
				var key, content string
				for more := hasAttr; more; {
					var k, v []byte
					k, v, more = z.TagAttr()
					switch string(k) {
					case "property", "name", "itemprop":
						if key == "" {
							key = strings.ToLower(strings.TrimSpace(string(v)))
						}
					case "content":
						content = string(v)
					}
				}
				if _, seen := out[key]; key != "" && content != "" && !seen {
					out[key] = content
				}
			}
		case html.TextToken:
			if _, seen := out["title"]; inTitle && !seen {
				out["title"] = string(z.Text())
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "title":
				inTitle = false
			case "head":
				return out
			}
		}
	}
}

func first(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// clip collapses whitespace, drops invalid UTF-8 and shortens s to n characters.
func clip(s string, n int) string {
	s = strings.ToValidUTF8(strings.Join(strings.Fields(s), " "), "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n-1])) + "…"
}

func siteName(u *url.URL) string {
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}
