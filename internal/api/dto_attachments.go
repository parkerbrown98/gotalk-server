package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/service"
	"github.com/parkerbrown98/gotalk-server/internal/store"
)

const tagAttachments = "Attachments"

// Attachment is a file attached to a message or post.
type Attachment struct {
	ID          string `json:"id" format:"uuid"`
	URL         string `json:"url"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size" doc:"Bytes"`
	Width       *int32 `json:"width" doc:"Images only. Files without dimensions are served as downloads"`
	Height      *int32 `json:"height"`
}

func toAttachment(up store.Upload) Attachment {
	a := Attachment{ID: up.ID.String(), URL: up.Url, Filename: up.Filename, ContentType: up.ContentType, Size: up.SizeBytes}
	if service.IsImage(up) {
		w, h := up.Width, up.Height
		a.Width, a.Height = &w, &h
	}
	return a
}

// EmbedImage is a link preview's image, copied to this instance's storage.
type EmbedImage struct {
	URL    string `json:"url"`
	Width  int32  `json:"width"`
	Height int32  `json:"height"`
}

// Embed is a preview of a link in a message or post, fetched by the server.
type Embed struct {
	URL         string      `json:"url" doc:"The link as written in the content"`
	Kind        string      `json:"kind" enum:"link,image" doc:"image: the link points straight at an image"`
	ResolvedURL string      `json:"resolved_url" doc:"Where the link led after redirects"`
	SiteName    string      `json:"site_name"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Color       *string     `json:"color" doc:"The site's theme color (#rrggbb)"`
	Image       *EmbedImage `json:"image"`
	LargeImage  bool        `json:"large_image" doc:"The site prefers a large image over a thumbnail"`
}

func toEmbed(p service.LinkPreview) Embed {
	e := Embed{
		URL: p.Url, Kind: p.Kind, ResolvedURL: p.FinalUrl, SiteName: p.SiteName, Title: p.Title,
		Description: p.Description, LargeImage: p.LargeImage || p.Kind == "image",
	}
	if p.ThemeColor != "" {
		c := p.ThemeColor
		e.Color = &c
	}
	if p.ImageUrl != nil && p.ImageWidth != nil && p.ImageHeight != nil {
		e.Image = &EmbedImage{URL: *p.ImageUrl, Width: *p.ImageWidth, Height: *p.ImageHeight}
	}
	return e
}

// parseAttachmentIDs validates attachment_ids from a request body.
func parseAttachmentIDs(raw []string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, len(raw))
	for i, r := range raw {
		id, err := parseID("attachment_ids", r)
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return ids, nil
}

type AttachmentUpload struct {
	Filename    string `query:"filename" maxLength:"255" doc:"The file's name, shown to readers and used for downloads"`
	ContentType string `header:"Content-Type"`
	RawBody     []byte `contentType:"application/octet-stream"`
}

func (s *Server) registerAttachments() {
	op := withStatus(withAuth(operation("upload-attachment", http.MethodPost, "/attachments",
		"Upload a file to attach to a message or post", tagAttachments)), http.StatusCreated)
	op.Description = "Send the file as the raw request body with its Content-Type, and its name in `filename`. " +
		"PNG, JPEG, GIF and WebP images have metadata such as EXIF location removed and are shown inline; " +
		"other files are kept as sent and served as downloads. Pass the returned ID in `attachment_ids` " +
		"when sending a message or post within an hour, after which unused uploads are deleted. " +
		"The size limit is `limits.upload_size` in GET /instance."
	op.MaxBodyBytes = s.Config.Uploads.MaxSize
	op.Errors = append(op.Errors, http.StatusRequestEntityTooLarge, http.StatusServiceUnavailable)
	huma.Register(s.api, withContentRateLimit(op),
		handle(s, func(ctx context.Context, in *AttachmentUpload) (*Body[Attachment], error) {
			up, err := s.Service.UploadAttachment(ctx, mustPrincipal(ctx), in.RawBody, in.Filename, in.ContentType, baseURLFrom(ctx))
			if err != nil {
				return nil, err
			}
			return ok(toAttachment(up))
		}))
}
