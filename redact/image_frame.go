package redact

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
)

var (
	pngSignature = []byte("\x89PNG\r\n\x1a\n")
	// pngEnd is the IEND chunk: a zero length, its type and its fixed CRC.
	pngEnd    = []byte("\x00\x00\x00\x00IEND\xaeB`\x82")
	jpegStart = []byte{0xFF, 0xD8, 0xFF}
	jpegEnd   = []byte{0xFF, 0xD9}
	gif87a    = []byte("GIF87a")
	gif89a    = []byte("GIF89a")
	gifEnd    = []byte{0x3B}
	riffTag   = []byte("RIFF")
	webpTag   = []byte("WEBP")
	vp8Start  = []byte{0x9D, 0x01, 0x2A}
)

// isFramedImage reports whether raw is framed as one whole PNG, JPEG, GIF or
// WebP image: a header its format's decoder accepts, with a positive size,
// and the format's end marker as its last bytes (for WebP, a RIFF size that
// covers exactly the file and a known first chunk). A magic number alone let
// any bytes after it pass as an image, such as a token after
// data:image/png;base64,iVBORw0KGgo, and so would bytes appended after a real
// image. Bytes placed inside an image's own segments or chunks still pass:
// they have to be put there on purpose, and nothing here reads image content.
//
// The checks read the header and the last bytes only, so a large image costs
// no more than its base64 decode.
func isFramedImage(raw []byte) bool {
	switch {
	case bytes.HasPrefix(raw, pngSignature):
		return bytes.HasSuffix(raw, pngEnd) && decodesConfig(png.DecodeConfig, raw)
	case bytes.HasPrefix(raw, jpegStart):
		return bytes.HasSuffix(raw, jpegEnd) && decodesConfig(jpeg.DecodeConfig, raw)
	case bytes.HasPrefix(raw, gif87a), bytes.HasPrefix(raw, gif89a):
		return bytes.HasSuffix(raw, gifEnd) && decodesConfig(gif.DecodeConfig, raw)
	case len(raw) >= 12 && bytes.HasPrefix(raw, riffTag) && bytes.Equal(raw[8:12], webpTag):
		return isFramedWebP(raw)
	}
	return false
}

func decodesConfig(decode func(io.Reader) (image.Config, error), raw []byte) bool {
	cfg, err := decode(bytes.NewReader(raw))
	return err == nil && cfg.Width > 0 && cfg.Height > 0
}

// isFramedWebP reports whether raw is one RIFF WebP file: its RIFF size is the
// file's length less the 8-byte RIFF header, and its first chunk is a VP8,
// VP8L or VP8X chunk that fits in the file and starts as that chunk must.
func isFramedWebP(raw []byte) bool {
	const header = 20 // RIFF tag, RIFF size, WEBP tag, chunk tag, chunk size
	if len(raw) < header || int64(binary.LittleEndian.Uint32(raw[4:8])) != int64(len(raw))-8 {
		return false
	}
	size := int64(binary.LittleEndian.Uint32(raw[16:20]))
	if size > int64(len(raw))-header {
		return false
	}
	data := raw[header:] // at least size bytes
	switch string(raw[12:16]) {
	case "VP8 ":
		// A key frame: 3 bytes of frame tag, then the start code.
		return size >= 10 && bytes.Equal(data[3:6], vp8Start)
	case "VP8L":
		return size >= 5 && data[0] == 0x2F
	case "VP8X":
		return size >= 10
	}
	return false
}
