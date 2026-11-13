package redact

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/gif"
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
// WebP image: a header its format defines, with a positive size, and the
// format's end marker as its last bytes (for WebP, a RIFF size that covers
// exactly the file and a known first chunk). PNG and GIF headers are read by
// their decoders, a JPEG's by its markers (see isJPEGHeader). A magic number alone let
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
		return bytes.HasSuffix(raw, jpegEnd) && isJPEGHeader(raw)
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

// isJPEGHeader reports whether raw, which starts with a JPEG start-of-image
// marker, holds a frame header before its first scan: a run of marker
// segments, each a 0xFF byte, a marker byte and a length that fits in raw,
// with a start-of-frame segment (SOF0 to SOF15, but not DHT, JPG or DAC) of
// a positive width before the start-of-scan segment. The frame and scan
// headers must each be as long as their component counts make them, so a
// header cut short is no image. It reads the markers only, so every JPEG
// coding process passes, including the lossless, arithmetic-coded and 12-bit
// ones that Go's decoder does not read. A height of 0 is valid (a DNL segment
// sets it later).
func isJPEGHeader(raw []byte) bool {
	frame := false
	for i := len(jpegStart) - 1; i+2 <= len(raw); {
		if raw[i] != 0xFF {
			return false
		}
		marker := raw[i+1]
		switch {
		case marker == 0xFF: // a fill byte
			i++
			continue
		case marker == 0x01 || marker >= 0xD0 && marker <= 0xD7: // TEM, RSTn: no length
			i += 2
			continue
		case marker == 0xD8 || marker == 0xD9 || marker == 0x00: // SOI, EOI or a stuffed byte before any scan
			return false
		}
		if i+4 > len(raw) {
			return false
		}
		length := int(raw[i+2])<<8 | int(raw[i+3])
		if length < 2 || i+2+length > len(raw) {
			return false
		}
		switch {
		case marker == 0xDA: // SOS
			// The component count, then two bytes per component and three
			// bytes that close the header.
			return frame && length >= 3 && raw[i+4] != 0 && length == 6+2*int(raw[i+4])
		case marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC:
			// Precision, then height and width, then the component count and
			// three bytes per component.
			if length < 8 || raw[i+7]|raw[i+8] == 0 || raw[i+9] == 0 || length != 8+3*int(raw[i+9]) {
				return false
			}
			frame = true
		}
		i += 2 + length
	}
	return false
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
