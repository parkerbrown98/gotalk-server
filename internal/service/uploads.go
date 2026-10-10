package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/media"
	"github.com/parkerbrown98/gotalk-server/internal/permissions"
	"github.com/parkerbrown98/gotalk-server/internal/storage"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

// Upload purposes and the storage directory each one uses.
const (
	UploadAvatar       = "avatar"
	UploadPlaceIcon    = "place_icon"
	UploadPlaceBanner  = "place_banner"
	UploadInstanceIcon = "instance_icon"
	UploadAttachment   = "attachment"
	UploadEmbed        = "embed"
)

var uploadDirs = map[string]string{
	UploadAvatar:       "avatars",
	UploadPlaceIcon:    "place-icons",
	UploadPlaceBanner:  "place-banners",
	UploadInstanceIcon: "instance",
	UploadAttachment:   "attachments",
	UploadEmbed:        "embeds",
}

// MediaPrefixes are the storage key prefixes served under /media/ and included in backups
// as media.
func MediaPrefixes() []string {
	return []string{"avatars/", "place-icons/", "place-banners/", "instance/", "attachments/", "embeds/"}
}

// uploadGrace protects new uploads from the cleanup sweep until they are attached.
const uploadGrace = time.Hour

// MaxUploadSize is the configured upload limit in bytes.
func (s *Service) MaxUploadSize() int64 { return s.cfg.Uploads.MaxSize }

// storeImage validates an image, strips its metadata, and stores it. The returned URL is
// what entities reference; the upload is deleted by maintenance if nothing ever does.
func (s *Service) storeImage(ctx context.Context, p *Principal, purpose string, data []byte, baseURL string) (string, error) {
	up, err := s.storeUpload(ctx, &p.User.ID, uploadInput{purpose: purpose, data: data}, baseURL)
	return up.Url, err
}

// uploadInput is a file to store. Only attachments may be something other than an image;
// filename and contentType (as declared by the client) only matter for them.
type uploadInput struct {
	purpose     string
	data        []byte
	filename    string
	contentType string
}

// storeUpload stores a file and records it. Images (PNG, JPEG, GIF, WebP) are validated and
// stripped of metadata; other files are only accepted as attachments and kept as sent.
func (s *Service) storeUpload(ctx context.Context, uploader *uuid.UUID, in uploadInput, baseURL string) (store.Upload, error) {
	p0 := s.Providers()
	b := p0.Storage
	if b == nil {
		return store.Upload{}, apperr.Unavailable("file uploads are unavailable: storage is not configured correctly")
	}
	if int64(len(in.data)) > s.cfg.Uploads.MaxSize {
		return store.Upload{}, apperr.Invalid("the file is larger than the %d byte limit", s.cfg.Uploads.MaxSize)
	}
	if len(in.data) == 0 {
		return store.Upload{}, apperr.Invalid("the file is empty")
	}
	info, clean, err := media.Process(in.data)
	switch {
	case errors.Is(err, media.ErrUnsupported) && in.purpose == UploadAttachment:
		clean = in.data
		info = media.Info{ContentType: fileContentType(in.contentType, in.data), Ext: fileExt(in.filename)}
	case err != nil:
		return store.Upload{}, apperr.Invalid("%s", err.Error())
	}
	filename := ""
	if in.purpose == UploadAttachment {
		filename = cleanFilename(in.filename, info)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return store.Upload{}, err
	}
	key := uploadDirs[in.purpose] + "/" + id.String() + "." + info.Ext
	if err := b.Put(ctx, key, bytes.NewReader(clean), int64(len(clean)), info.ContentType); err != nil {
		s.log.Error("storing upload", "key", key, "storage", b.Describe(), "error", err)
		return store.Upload{}, apperr.Unavailable("the file could not be stored; try again later")
	}
	up, err := s.q.CreateUpload(ctx, store.CreateUploadParams{
		ID: id, StorageKey: key, Url: mediaURL(p0.StorageSettings.PublicURL, baseURL, key), Purpose: in.purpose,
		UploaderID: uploader, ContentType: info.ContentType, SizeBytes: int64(len(clean)),
		Width: int32(info.Width), Height: int32(info.Height), //nolint:gosec // bounded by media.MaxSide
		Filename: filename,
	})
	if err != nil {
		_ = b.Delete(context.WithoutCancel(ctx), key)
		return store.Upload{}, err
	}
	return up, nil
}

// IsImage reports whether an upload is an image clients can show inline.
func IsImage(up store.Upload) bool { return up.Width > 0 && up.Height > 0 }

// fileContentType picks the type recorded for a non-image attachment: the client's, if it
// is well formed, or else a sniffed one. Files are always served as downloads in a
// sandbox, so the type is only a hint.
func fileContentType(declared string, data []byte) string {
	if mt, _, err := mime.ParseMediaType(declared); err == nil && strings.Contains(mt, "/") && len(mt) <= 127 {
		return mt
	}
	mt, _, _ := mime.ParseMediaType(http.DetectContentType(data))
	return mt
}

var fileExtPattern = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

func fileExt(filename string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(filename), "."))
	if fileExtPattern.MatchString(ext) {
		return ext
	}
	return "bin"
}

const maxFilenameLen = 200

// cleanFilename keeps the last path element of a client-supplied name, without control
// characters, quotes or anything over maxFilenameLen characters (the extension is kept).
func cleanFilename(name string, info media.Info) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' || r == '/' || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "." || name == ".." {
		name = ""
	}
	if name == "" {
		if info.Width > 0 {
			return "image." + info.Ext
		}
		return "file"
	}
	if r := []rune(name); len(r) > maxFilenameLen {
		ext := []rune(path.Ext(name))
		if len(ext) > 16 {
			ext = nil
		}
		name = string(r[:maxFilenameLen-len(ext)]) + string(ext)
	}
	return name
}

func mediaURL(publicURL, baseURL, key string) string {
	if publicURL != "" {
		return strings.TrimRight(publicURL, "/") + "/" + key
	}
	return baseURL + "/media/" + key
}

// releaseUpload deletes the upload behind url right away if it is ours and nothing uses
// it any more. Failures are left to the maintenance sweep.
func (s *Service) releaseUpload(ctx context.Context, url *string) {
	if url == nil || *url == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	up, err := s.q.GetUploadByURL(ctx, *url)
	if err != nil {
		return
	}
	if used, err := s.q.UploadReferenced(ctx, *url); err != nil || used {
		return
	}
	s.deleteUpload(ctx, up)
}

func (s *Service) deleteUpload(ctx context.Context, up store.Upload) {
	b := s.storageBackend()
	if b == nil {
		return
	}
	if err := b.Delete(ctx, up.StorageKey); err != nil {
		s.log.Warn("deleting unused upload", "key", up.StorageKey, "error", err)
		return
	}
	if err := s.q.DeleteUpload(ctx, up.ID); err != nil {
		s.log.Warn("forgetting deleted upload", "key", up.StorageKey, "error", err)
	}
}

// pruneUploads deletes uploads created before `before` that nothing references.
func (s *Service) pruneUploads(ctx context.Context, before time.Time) error {
	if s.storageBackend() == nil {
		return nil
	}
	for {
		ups, err := s.q.ListUnreferencedUploads(ctx, store.ListUnreferencedUploadsParams{Before: before, Lim: 100})
		if err != nil || len(ups) == 0 {
			return err
		}
		for _, up := range ups {
			s.deleteUpload(ctx, up)
		}
		if len(ups) < 100 {
			return nil
		}
		// Stop if deletions keep failing rather than spinning on the same rows.
		if left, err := s.q.ListUnreferencedUploads(ctx, store.ListUnreferencedUploadsParams{Before: before, Lim: 1}); err != nil ||
			(len(left) > 0 && left[0].ID == ups[0].ID) {
			return err
		}
	}
}

// SetAvatar stores an uploaded image as the caller's avatar.
func (s *Service) SetAvatar(ctx context.Context, p *Principal, data []byte, baseURL string) (store.User, error) {
	url, err := s.storeImage(ctx, p, UploadAvatar, data, baseURL)
	if err != nil {
		return store.User{}, err
	}
	return s.replaceAvatar(ctx, p, url)
}

// ClearAvatar removes the caller's avatar.
func (s *Service) ClearAvatar(ctx context.Context, p *Principal) (store.User, error) {
	return s.replaceAvatar(ctx, p, "")
}

func (s *Service) replaceAvatar(ctx context.Context, p *Principal, url string) (store.User, error) {
	prev := p.User.AvatarUrl
	user, err := s.UpdateProfile(ctx, p, ProfileUpdate{AvatarURL: &url})
	if err != nil {
		return user, err
	}
	s.releaseUpload(ctx, prev)
	return user, nil
}

// SetPlaceImage stores an uploaded image as a place's icon or banner (MANAGE_PLACE).
// Empty data removes the image.
func (s *Service) SetPlaceImage(ctx context.Context, p *Principal, ref, purpose string, data []byte, baseURL string) (PlaceView, error) {
	place, _, err := s.requirePermission(ctx, s.q, p, ref, permissions.ManagePlace)
	if err != nil {
		return PlaceView{}, err
	}
	url := ""
	if data != nil {
		if url, err = s.storeImage(ctx, p, purpose, data, baseURL); err != nil {
			return PlaceView{}, err
		}
	}
	in, prev := PlaceUpdate{IconURL: &url}, place.IconUrl
	if purpose == UploadPlaceBanner {
		in, prev = PlaceUpdate{BannerURL: &url}, place.BannerUrl
	}
	view, err := s.UpdatePlace(ctx, p, place.ID.String(), in)
	if err != nil {
		return view, err
	}
	s.releaseUpload(ctx, prev)
	return view, nil
}

// SetInstanceIcon stores an uploaded image as the instance icon (instance admins only).
// Empty data removes the icon.
func (s *Service) SetInstanceIcon(ctx context.Context, p *Principal, data []byte, baseURL string) (store.InstanceSetting, error) {
	if !p.User.IsInstanceAdmin {
		return store.InstanceSetting{}, apperr.Forbidden("only instance administrators can change instance settings")
	}
	settings, err := s.q.GetInstanceSettings(ctx)
	if err != nil {
		return settings, err
	}
	url := ""
	if data != nil {
		if url, err = s.storeImage(ctx, p, UploadInstanceIcon, data, baseURL); err != nil {
			return settings, err
		}
	}
	updated, err := s.UpdateInstance(ctx, p, InstanceUpdate{IconURL: &url})
	if err != nil {
		return updated, err
	}
	s.releaseUpload(ctx, settings.IconUrl)
	return updated, nil
}

// MediaFile is an uploaded file opened for serving.
type MediaFile struct {
	storage.Object
	// Filename is the attachment's name; empty for other uploads.
	Filename string
	// Download is set for files that must not be shown inline (non-image attachments).
	Download bool
}

// OpenMedia opens an uploaded file for serving. Only files recorded as uploads are
// served, so backups and other objects in the bucket stay private.
func (s *Service) OpenMedia(ctx context.Context, key string) (io.ReadCloser, MediaFile, error) {
	if !storage.ValidKey(key) {
		return nil, MediaFile{}, apperr.NotFound("file not found")
	}
	up, err := s.q.GetUploadByKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, MediaFile{}, apperr.NotFound("file not found")
	}
	if err != nil {
		return nil, MediaFile{}, err
	}
	b := s.storageBackend()
	if b == nil {
		return nil, MediaFile{}, apperr.Unavailable("storage is not configured")
	}
	rc, obj, err := b.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, MediaFile{}, apperr.NotFound("file not found")
	}
	if err != nil {
		return nil, MediaFile{}, err
	}
	obj.ContentType = up.ContentType
	return rc, MediaFile{Object: obj, Filename: up.Filename, Download: up.Purpose == UploadAttachment && !IsImage(up)}, nil
}
