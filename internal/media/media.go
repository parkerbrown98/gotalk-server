// Package media validates uploaded images and strips metadata (EXIF, XMP, text chunks)
// that can reveal where and when a photo was taken. It works losslessly on the encoded
// bytes, so no CGO image library is needed and pixel data is never decoded.
package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	_ "image/gif" // register decoders for DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"net/http"
)

// Info describes a validated image.
type Info struct {
	ContentType string
	Ext         string
	Width       int
	Height      int
}

// ErrUnsupported is returned for files that are not PNG, JPEG, GIF or WebP images.
var ErrUnsupported = errors.New("only PNG, JPEG, GIF and WebP images are supported")

// MaxSide is the largest accepted width or height in pixels.
const MaxSide = 8192

var exts = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}

// ContentTypes lists the accepted image types.
func ContentTypes() []string { return []string{"image/png", "image/jpeg", "image/gif", "image/webp"} }

// Process sniffs data, checks it is a supported image within MaxSide, and returns the
// image with metadata removed.
func Process(data []byte) (Info, []byte, error) {
	ct := http.DetectContentType(data)
	ext, ok := exts[ct]
	if !ok {
		return Info{}, nil, ErrUnsupported
	}
	var (
		out []byte
		err error
	)
	switch ct {
	case "image/jpeg":
		out, err = stripJPEG(data)
	case "image/png":
		out, err = stripPNG(data)
	case "image/webp":
		out, err = stripWebP(data)
	default:
		out = data
	}
	if err != nil {
		return Info{}, nil, fmt.Errorf("the image is corrupt: %w", err)
	}
	var w, h int
	if ct == "image/webp" {
		w, h, err = webpSize(out)
	} else {
		var cfg image.Config
		cfg, _, err = image.DecodeConfig(bytes.NewReader(out))
		w, h = cfg.Width, cfg.Height
	}
	if err != nil {
		return Info{}, nil, fmt.Errorf("the image is corrupt: %w", err)
	}
	if w < 1 || h < 1 || w > MaxSide || h > MaxSide {
		return Info{}, nil, fmt.Errorf("images must be at most %d×%d pixels (this one is %d×%d)", MaxSide, MaxSide, w, h)
	}
	return Info{ContentType: ct, Ext: ext, Width: w, Height: h}, out, nil
}

var errTruncated = errors.New("truncated")

// stripJPEG drops APP1 (EXIF, XMP), APP13 (IPTC) and COM segments before the image data.
func stripJPEG(data []byte) ([]byte, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, errors.New("missing JPEG header")
	}
	out := make([]byte, 0, len(data))
	out = append(out, 0xFF, 0xD8)
	i := 2
	for i < len(data) {
		if data[i] != 0xFF {
			return nil, errors.New("bad JPEG marker")
		}
		for i < len(data) && data[i] == 0xFF {
			i++ // fill bytes
		}
		if i >= len(data) {
			return nil, errTruncated
		}
		marker := data[i]
		i++
		switch {
		case marker == 0xD9: // EOI
			return append(out, 0xFF, marker), nil
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			out = append(out, 0xFF, marker)
			continue
		}
		if i+2 > len(data) {
			return nil, errTruncated
		}
		n := int(binary.BigEndian.Uint16(data[i:]))
		if n < 2 || i+n > len(data) {
			return nil, errTruncated
		}
		seg := data[i : i+n]
		if marker == 0xDA { // start of scan: the rest is entropy-coded data
			out = append(out, 0xFF, marker)
			return append(out, data[i:]...), nil
		}
		if marker != 0xE1 && marker != 0xED && marker != 0xFE {
			out = append(out, 0xFF, marker)
			out = append(out, seg...)
		}
		i += n
	}
	return nil, errTruncated
}

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// stripPNG drops the eXIf, tEXt, zTXt, iTXt and tIME chunks.
func stripPNG(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, pngSignature) {
		return nil, errors.New("missing PNG signature")
	}
	out := make([]byte, 0, len(data))
	out = append(out, pngSignature...)
	i := len(pngSignature)
	for i < len(data) {
		if i+8 > len(data) {
			return nil, errTruncated
		}
		n := int(binary.BigEndian.Uint32(data[i:]))
		typ := string(data[i+4 : i+8])
		end := i + 12 + n
		if n < 0 || end > len(data) || end < i {
			return nil, errTruncated
		}
		if crc32.ChecksumIEEE(data[i+4:i+8+n]) != binary.BigEndian.Uint32(data[i+8+n:]) {
			return nil, fmt.Errorf("bad checksum in %s chunk", typ)
		}
		switch typ {
		case "eXIf", "tEXt", "zTXt", "iTXt", "tIME":
		default:
			out = append(out, data[i:end]...)
		}
		i = end
		if typ == "IEND" {
			return out, nil
		}
	}
	return nil, errTruncated
}

type riffChunk struct {
	fourCC  string
	payload []byte
}

func webpChunks(data []byte) ([]riffChunk, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return nil, errors.New("missing WebP header")
	}
	size := int(binary.LittleEndian.Uint32(data[4:]))
	if size+8 > len(data) || size < 4 {
		return nil, errTruncated
	}
	body := data[12 : 8+size]
	var chunks []riffChunk
	for i := 0; i < len(body); {
		if i+8 > len(body) {
			return nil, errTruncated
		}
		n := int(binary.LittleEndian.Uint32(body[i+4:]))
		if n < 0 || i+8+n > len(body) {
			return nil, errTruncated
		}
		chunks = append(chunks, riffChunk{fourCC: string(body[i : i+4]), payload: body[i+8 : i+8+n]})
		i += 8 + n + n%2
	}
	if len(chunks) == 0 {
		return nil, errTruncated
	}
	return chunks, nil
}

// stripWebP drops EXIF and XMP chunks and clears their flags in the VP8X header.
func stripWebP(data []byte) ([]byte, error) {
	chunks, err := webpChunks(data)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	for _, c := range chunks {
		payload := c.payload
		switch c.fourCC {
		case "EXIF", "XMP ":
			continue
		case "VP8X":
			if len(payload) < 10 {
				return nil, errTruncated
			}
			payload = bytes.Clone(payload)
			payload[0] &^= 0x08 | 0x04 // EXIF and XMP present flags
		}
		body.WriteString(c.fourCC)
		_ = binary.Write(&body, binary.LittleEndian, uint32(len(payload))) //nolint:gosec // chunk sizes come from a 32-bit field
		body.Write(payload)
		if len(payload)%2 == 1 {
			body.WriteByte(0)
		}
	}
	out := make([]byte, 0, 12+body.Len())
	out = append(out, "RIFF"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(4+body.Len())) //nolint:gosec // bounded by the input size
	out = append(out, "WEBP"...)
	return append(out, body.Bytes()...), nil
}

func webpSize(data []byte) (int, int, error) {
	chunks, err := webpChunks(data)
	if err != nil {
		return 0, 0, err
	}
	c := chunks[0]
	p := c.payload
	switch c.fourCC {
	case "VP8X":
		if len(p) < 10 {
			return 0, 0, errTruncated
		}
		w := int(p[4]) | int(p[5])<<8 | int(p[6])<<16
		h := int(p[7]) | int(p[8])<<8 | int(p[9])<<16
		return w + 1, h + 1, nil
	case "VP8 ":
		if len(p) < 10 || p[3] != 0x9d || p[4] != 0x01 || p[5] != 0x2a {
			return 0, 0, errors.New("bad VP8 frame header")
		}
		return int(binary.LittleEndian.Uint16(p[6:]) & 0x3fff), int(binary.LittleEndian.Uint16(p[8:]) & 0x3fff), nil
	case "VP8L":
		if len(p) < 5 || p[0] != 0x2f {
			return 0, 0, errors.New("bad VP8L header")
		}
		bits := binary.LittleEndian.Uint32(p[1:])
		return int(bits&0x3fff) + 1, int((bits>>14)&0x3fff) + 1, nil
	}
	return 0, 0, fmt.Errorf("unexpected first WebP chunk %q", c.fourCC)
}
