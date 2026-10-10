package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

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
)

var uploadDirs = map[string]string{
	UploadAvatar:       "avatars",
	UploadPlaceIcon:    "place-icons",
	UploadPlaceBanner:  "place-banners",
	UploadInstanceIcon: "instance",
}

// MediaPrefixes are the storage key prefixes served under /media/ and included in backups
// as media.
func MediaPrefixes() []string {
	return []string{"avatars/", "place-icons/", "place-banners/", "instance/"}
}

// uploadGrace protects new uploads from the cleanup sweep until they are attached.
const uploadGrace = time.Hour

// MaxUploadSize is the configured upload limit in bytes.
func (s *Service) MaxUploadSize() int64 { return s.cfg.Uploads.MaxSize }

// storeImage validates an image, strips its metadata, and stores it. The returned URL is
// what entities reference; the upload is deleted by maintenance if nothing ever does.
func (s *Service) storeImage(ctx context.Context, p *Principal, purpose string, data []byte, baseURL string) (string, error) {
	p0 := s.Providers()
	b := p0.Storage
	if b == nil {
		return "", apperr.Unavailable("file uploads are unavailable: storage is not configured correctly")
	}
	if int64(len(data)) > s.cfg.Uploads.MaxSize {
		return "", apperr.Invalid("the file is larger than the %d byte limit", s.cfg.Uploads.MaxSize)
	}
	info, clean, err := media.Process(data)
	if err != nil {
		return "", apperr.Invalid("%s", err.Error())
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	key := uploadDirs[purpose] + "/" + id.String() + "." + info.Ext
	if err := b.Put(ctx, key, bytes.NewReader(clean), int64(len(clean)), info.ContentType); err != nil {
		s.log.Error("storing upload", "key", key, "storage", b.Describe(), "error", err)
		return "", apperr.Unavailable("the file could not be stored; try again later")
	}
	url := mediaURL(p0.StorageSettings.PublicURL, baseURL, key)
	uploader := p.User.ID
	if _, err := s.q.CreateUpload(ctx, store.CreateUploadParams{
		ID: id, StorageKey: key, Url: url, Purpose: purpose, UploaderID: &uploader, ContentType: info.ContentType,
		SizeBytes: int64(len(clean)), Width: int32(info.Width), Height: int32(info.Height), //nolint:gosec // bounded by media.MaxSide
	}); err != nil {
		_ = b.Delete(context.WithoutCancel(ctx), key)
		return "", err
	}
	return url, nil
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

// OpenMedia opens an uploaded file for serving. Only files recorded as uploads are
// served, so backups and other objects in the bucket stay private.
func (s *Service) OpenMedia(ctx context.Context, key string) (io.ReadCloser, storage.Object, error) {
	if !storage.ValidKey(key) {
		return nil, storage.Object{}, apperr.NotFound("file not found")
	}
	up, err := s.q.GetUploadByKey(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, storage.Object{}, apperr.NotFound("file not found")
	}
	if err != nil {
		return nil, storage.Object{}, err
	}
	b := s.storageBackend()
	if b == nil {
		return nil, storage.Object{}, apperr.Unavailable("storage is not configured")
	}
	rc, obj, err := b.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, storage.Object{}, apperr.NotFound("file not found")
	}
	if err != nil {
		return nil, storage.Object{}, err
	}
	obj.ContentType = up.ContentType
	return rc, obj, nil
}
