package service

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/store"
	"github.com/parkerbrown98/gotalk-server/internal/unfurl"
)

const (
	// MaxAttachments is how many files a message or post may carry.
	MaxAttachments = 10
	// maxEmbeds is how many links in one message or post get previews.
	maxEmbeds = 5
	// Previews are fetched again when someone posts a link whose preview is older than this.
	previewTTL       = 7 * 24 * time.Hour
	failedPreviewTTL = time.Hour
	// previewWorkers bounds concurrent preview fetches across the instance.
	previewWorkers = 4
)

type baseURLKey struct{}

// WithBaseURL records the instance's public base URL (as seen by the current request) for
// work that builds /media URLs after the request, such as storing link preview images.
func WithBaseURL(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, baseURLKey{}, base)
}

func baseURLOf(ctx context.Context) string {
	b, _ := ctx.Value(baseURLKey{}).(string)
	return b
}

// UploadAttachment stores a file for the caller to attach to a message or post. Images are
// stripped of metadata and shown inline; other files are kept as sent and served as
// downloads. Uploads not attached within an hour are deleted.
func (s *Service) UploadAttachment(ctx context.Context, p *Principal, data []byte, filename, contentType, baseURL string) (store.Upload, error) {
	return s.storeUpload(ctx, &p.User.ID, uploadInput{
		purpose: UploadAttachment, data: data, filename: filename, contentType: contentType,
	}, baseURL)
}

// claimAttachments checks that ids are unused attachments the caller uploaded, and returns
// them in the given order.
func (s *Service) claimAttachments(ctx context.Context, q *store.Queries, p *Principal, ids []uuid.UUID) ([]store.Upload, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxAttachments {
		return nil, apperr.Invalid("at most %d attachments are allowed", MaxAttachments)
	}
	if len(dedupe(slices.Clone(ids))) != len(ids) {
		return nil, apperr.Invalid("attachment_ids must not repeat")
	}
	rows, err := q.ListUploadsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]store.Upload, len(rows))
	for _, up := range rows {
		if up.Purpose == UploadAttachment && up.UploaderID != nil && *up.UploaderID == p.User.ID {
			byID[up.ID] = up
		}
	}
	out := make([]store.Upload, len(ids))
	for i, id := range ids {
		up, ok := byID[id]
		if !ok {
			return nil, apperr.Invalid("attachment %s not found; upload it with POST /attachments first", id)
		}
		out[i] = up
	}
	used, err := q.ListAttachedUploads(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(used) > 0 {
		return nil, apperr.Invalid("attachment %s is already in use", used[0])
	}
	return out, nil
}

func uploadIDs(ups []store.Upload) []uuid.UUID {
	ids := make([]uuid.UUID, len(ups))
	for i, up := range ups {
		ids[i] = up.ID
	}
	return ids
}

// LinkPreview is a cached preview for a link in a message or post.
type LinkPreview = store.ListLinkPreviewsRow

var (
	linkCodeFence = regexp.MustCompile("(?s)(^|\n)\\s{0,3}(```|~~~).*?(\n\\s{0,3}(```|~~~)[^\n]*|$)")
	linkCodeSpan  = regexp.MustCompile("`[^`\n]*`")
	linkPattern   = regexp.MustCompile(`(<)?(https?://[^\s<>"]+)`)
)

// linkURLs returns the first maxEmbeds distinct web links in Markdown content, skipping code
// and links wrapped in <angle brackets>, which is how authors opt out of a preview.
func linkURLs(content string) []string {
	if !strings.Contains(content, "http") {
		return nil
	}
	content = linkCodeFence.ReplaceAllString(content, "$1 ")
	content = linkCodeSpan.ReplaceAllString(content, " ")
	var out []string
	for _, m := range linkPattern.FindAllStringSubmatch(content, -1) {
		if m[1] == "<" {
			continue
		}
		raw := trimLink(m[2])
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || len(raw) > 2048 || slices.Contains(out, raw) {
			continue
		}
		out = append(out, raw)
		if len(out) == maxEmbeds {
			break
		}
	}
	return out
}

// trimLink drops trailing punctuation and Markdown syntax that is not part of a link, such
// as the ")" closing [text](url) or a sentence's full stop.
func trimLink(s string) string {
	for s != "" {
		last := s[len(s)-1]
		switch {
		case strings.IndexByte(".,:;!?'*_~]", last) >= 0:
			s = s[:len(s)-1]
		case last == ')' && strings.Count(s, "(") < strings.Count(s, ")"):
			s = s[:len(s)-1]
		default:
			return s
		}
	}
	return s
}

// linkPreviews looks up the cached previews for each content's links, in link order.
func (s *Service) linkPreviews(ctx context.Context, q *store.Queries, contents []string) ([][]LinkPreview, error) {
	out := make([][]LinkPreview, len(contents))
	links := make([][]string, len(contents))
	var all []string
	for i, c := range contents {
		links[i] = linkURLs(c)
		all = append(all, links[i]...)
	}
	if len(all) == 0 {
		return out, nil
	}
	rows, err := q.ListLinkPreviews(ctx, dedupeStrings(all))
	if err != nil {
		return nil, err
	}
	byURL := make(map[string]LinkPreview, len(rows))
	for _, r := range rows {
		if r.Status == "ok" {
			byURL[r.Url] = r
		}
	}
	for i, ls := range links {
		for _, l := range ls {
			if r, ok := byURL[l]; ok {
				out[i] = append(out[i], r)
			}
		}
	}
	return out, nil
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// linkResolver fetches previews in the background, one fetch per URL at a time.
type linkResolver struct {
	once     sync.Once
	client   *unfurl.Client
	sem      chan struct{}
	mu       sync.Mutex
	inflight map[string]chan struct{}
	jobs     sync.WaitGroup
	closed   bool
}

func (s *Service) previewClient() *unfurl.Client {
	r := &s.links
	r.once.Do(func() {
		if !s.cfg.Embeds.Enabled {
			return
		}
		r.client = unfurl.New(unfurl.Options{AllowPrivate: s.cfg.Embeds.AllowPrivateNetworks, MaxImage: s.cfg.Uploads.MaxSize})
		r.sem = make(chan struct{}, previewWorkers)
		r.inflight = map[string]chan struct{}{}
	})
	return r.client
}

// resolveLinks fetches previews for the links in content once the transaction q belongs to
// commits, then calls updated if any new preview became available.
func (s *Service) resolveLinks(ctx context.Context, q *store.Queries, content string, updated func(context.Context)) {
	urls := linkURLs(content)
	if len(urls) == 0 || s.previewClient() == nil {
		return
	}
	s.afterCommit(ctx, q, func(ctx context.Context) {
		r := &s.links
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		r.jobs.Add(1)
		r.mu.Unlock()
		go func() {
			defer r.jobs.Done()
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if s.fetchPreviews(ctx, urls) {
				updated(ctx)
			}
		}()
	})
}

// WaitLinkPreviews stops new preview fetches and waits for running ones; call it when
// shutting down.
func (s *Service) WaitLinkPreviews() {
	s.links.mu.Lock()
	s.links.closed = true
	s.links.mu.Unlock()
	s.links.jobs.Wait()
}

// fetchPreviews refreshes missing and stale previews and reports whether any changed.
func (s *Service) fetchPreviews(ctx context.Context, urls []string) bool {
	rows, err := s.q.ListLinkPreviews(ctx, urls)
	if err != nil {
		s.log.Warn("loading link previews", "error", err)
		return false
	}
	fresh := map[string]bool{}
	for _, r := range rows {
		ttl := previewTTL
		if r.Status != "ok" {
			ttl = failedPreviewTTL
		}
		fresh[r.Url] = time.Since(r.FetchedAt) < ttl
	}
	r := &s.links
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		changed bool
	)
	for _, u := range urls {
		if fresh[u] {
			continue
		}
		r.mu.Lock()
		wait, busy := r.inflight[u]
		done := make(chan struct{})
		if !busy {
			r.inflight[u] = done
		}
		r.mu.Unlock()
		wg.Go(func() {
			ok := false
			if busy {
				// Another message is fetching the same link; reuse its result.
				select {
				case <-wait:
					ok = true
				case <-ctx.Done():
				}
			} else {
				defer func() {
					r.mu.Lock()
					delete(r.inflight, u)
					r.mu.Unlock()
					close(done)
				}()
				select {
				case r.sem <- struct{}{}:
					ok = s.fetchPreview(ctx, u)
					<-r.sem
				case <-ctx.Done():
				}
			}
			if ok {
				mu.Lock()
				changed = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return changed
}

// fetchPreview fetches one link and stores the result, copying its preview image into
// storage. It reports whether a usable preview was stored.
func (s *Service) fetchPreview(ctx context.Context, link string) bool {
	res, err := s.previewClient().Fetch(ctx, link)
	params := store.UpsertLinkPreviewParams{Url: link, Status: "failed", Kind: unfurl.KindLink}
	if err != nil {
		s.log.Debug("fetching link preview", "url", link, "error", err)
	} else {
		params = store.UpsertLinkPreviewParams{
			Url: link, Status: "ok", Kind: res.Kind, FinalUrl: res.FinalURL, SiteName: res.SiteName,
			Title: res.Title, Description: res.Description, ThemeColor: res.ThemeColor, LargeImage: res.LargeImage,
		}
		if res.Image != nil {
			up, err := s.storeUpload(ctx, nil, uploadInput{purpose: UploadEmbed, data: res.Image}, baseURLOf(ctx))
			if err == nil {
				params.ImageUploadID = &up.ID
			} else {
				s.log.Debug("storing link preview image", "url", link, "error", err)
			}
		}
		// A direct image link is only worth showing if the image could be kept.
		if res.Kind == unfurl.KindImage && params.ImageUploadID == nil {
			params = store.UpsertLinkPreviewParams{Url: link, Status: "failed", Kind: unfurl.KindImage}
		}
	}
	if err := s.q.UpsertLinkPreview(ctx, params); err != nil {
		s.log.Warn("saving link preview", "url", link, "error", err)
		return false
	}
	return params.Status == "ok"
}
