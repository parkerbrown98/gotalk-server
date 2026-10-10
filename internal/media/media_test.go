package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"
)

func testImage(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, 0, color.RGBA{R: 200, A: 255})
	}
	return img
}

func TestJPEGLosesEXIF(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, testImage(40, 30), nil))
	exif := append([]byte("Exif\x00\x00"), []byte("GPS 52.37N 4.89E")...)
	seg := append([]byte{0xFF, 0xE1, 0, 0}, exif...)
	binary.BigEndian.PutUint16(seg[2:], uint16(len(exif)+2))
	withExif := append(append([]byte{0xFF, 0xD8}, seg...), buf.Bytes()[2:]...)

	info, out, err := Process(withExif)
	require.NoError(t, err)
	require.Equal(t, Info{ContentType: "image/jpeg", Ext: "jpg", Width: 40, Height: 30}, info)
	require.NotContains(t, string(out), "GPS")
	require.Contains(t, string(withExif), "GPS")
	_, err = jpeg.Decode(bytes.NewReader(out))
	require.NoError(t, err, "stripped JPEG still decodes")
}

func pngChunk(typ string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, typ...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

func TestPNGLosesTextChunks(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, testImage(16, 8)))
	raw := buf.Bytes()
	iend := len(raw) - 12
	withText := append(append(append([]byte{}, raw[:iend]...), pngChunk("tEXt", []byte("Comment\x00taken at home"))...), raw[iend:]...)

	info, out, err := Process(withText)
	require.NoError(t, err)
	require.Equal(t, 16, info.Width)
	require.Equal(t, "png", info.Ext)
	require.NotContains(t, string(out), "taken at home")
	_, err = png.Decode(bytes.NewReader(out))
	require.NoError(t, err)

	// A corrupted checksum is rejected rather than passed through.
	bad := bytes.Clone(withText)
	bad[len(bad)-20] ^= 0xFF
	_, _, err = Process(bad)
	require.Error(t, err)
}

func webp(chunks ...[]byte) []byte {
	body := []byte("WEBP")
	for _, c := range chunks {
		body = append(body, c...)
	}
	out := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	return append(out, body...)
}

func riff(fourCC string, payload []byte) []byte {
	out := append([]byte(fourCC), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...)
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func TestWebPLosesEXIFAndXMP(t *testing.T) {
	vp8x := make([]byte, 10)
	vp8x[0] = 0x08 | 0x04     // EXIF and XMP flags
	vp8x[4], vp8x[7] = 99, 49 // 100×50 canvas
	bits := uint32(99) | uint32(49)<<14
	vp8l := append([]byte{0x2f}, binary.LittleEndian.AppendUint32(nil, bits)...)
	in := webp(riff("VP8X", vp8x), riff("VP8L", vp8l), riff("EXIF", []byte("GPS secret")), riff("XMP ", []byte("<x:xmpmeta/>")))

	info, out, err := Process(in)
	require.NoError(t, err)
	require.Equal(t, Info{ContentType: "image/webp", Ext: "webp", Width: 100, Height: 50}, info)
	require.NotContains(t, string(out), "GPS secret")
	require.NotContains(t, string(out), "xmpmeta")
	chunks, err := webpChunks(out)
	require.NoError(t, err)
	require.Len(t, chunks, 2)
	require.Zero(t, chunks[0].payload[0]&0x0C, "metadata flags are cleared")

	// A simple lossless file without VP8X.
	info, _, err = Process(webp(riff("VP8L", vp8l)))
	require.NoError(t, err)
	require.Equal(t, 100, info.Width)
	require.Equal(t, 50, info.Height)
}

func TestRejectsNonImagesAndHugeImages(t *testing.T) {
	_, _, err := Process([]byte("<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>"))
	require.ErrorIs(t, err, ErrUnsupported)
	_, _, err = Process([]byte("GIF89a"))
	require.Error(t, err, "truncated GIF")

	vp8x := make([]byte, 10)
	big := MaxSide // width-1 = MaxSide means MaxSide+1 pixels
	vp8x[4], vp8x[5] = byte(big), byte(big>>8)
	_, _, err = Process(webp(riff("VP8X", vp8x)))
	require.ErrorContains(t, err, "at most")
}
