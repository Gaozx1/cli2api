// Package imgconvert re-encodes chat image attachments into formats an upstream
// provider accepts.
//
// WorkBuddy rejects an image it cannot parse with code 11135 ("Image not
// recognized") and tells the caller to re-upload it as JPG or PNG. The gateway
// forwards the caller's image bytes verbatim, so an otherwise valid request
// fails on the image encoding alone. Re-encoding to PNG or JPEG here turns that
// into a working request instead of an error the caller has to act on.
//
// Only the encoding changes: the pixels are decoded and re-encoded, never
// resized, so the model still sees the same image.
package imgconvert

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"

	// Register the decoders image.Decode dispatches to. GIF is stdlib; WebP is
	// the common format phones and browsers produce that the upstream refuses.
	_ "golang.org/x/image/webp"
	_ "image/gif"
)

const (
	PNGMime  = "image/png"
	JPEGMime = "image/jpeg"

	// maxInputBytes caps what we will decode. A data URL past this is left
	// alone rather than decoded, so a pathologically large attachment cannot
	// allocate its way through the gateway.
	maxInputBytes = 24 << 20

	// jpegQuality balances fidelity against the base64 size the upstream body
	// has to carry. Quality 90 is visually near-lossless for photos.
	jpegQuality = 90
)

// acceptable reports whether an upstream-defined "JPG or PNG" rule is already
// satisfied, in which case the bytes are passed through untouched.
func acceptable(mime string) bool {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case PNGMime, JPEGMime, "image/jpg", "image/pjpeg":
		return true
	}
	return false
}

// NormalizeDataURL rewrites a base64 image data URL into PNG or JPEG when its
// current format is one the upstream rejects. It reports whether it changed the
// URL. A non-data URL (a remote http(s) link), an already-acceptable image, and
// anything we cannot decode are all returned untouched, so this is safe to run
// on every request.
func NormalizeDataURL(raw string) (string, bool) {
	mime, payload, ok := splitDataURL(raw)
	if !ok || len(payload) == 0 || len(payload) > maxInputBytes {
		return raw, false
	}
	if acceptable(mime) {
		return raw, false
	}
	img, _, err := image.Decode(bytes.NewReader(payload))
	if err != nil {
		// Undecodable or a format we have no decoder for (HEIC, corrupt, or a
		// mislabeled mime): there is nothing useful to convert to.
		return raw, false
	}
	encoded, outMime, err := encode(img)
	if err != nil {
		return raw, false
	}
	return "data:" + outMime + ";base64," + base64.StdEncoding.EncodeToString(encoded), true
}

// encode writes the image as PNG when it carries transparency (JPEG has no
// alpha channel and would silently flatten it) and as JPEG otherwise, where the
// smaller payload is worth the compression for a photo.
func encode(img image.Image) ([]byte, string, error) {
	var buf bytes.Buffer
	if hasAlpha(img) {
		if err := png.Encode(&buf, img); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), PNGMime, nil
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), JPEGMime, nil
}

// hasAlpha reports whether any pixel is not fully opaque. Formats that cannot
// carry alpha skip the scan entirely.
func hasAlpha(img image.Image) bool {
	switch img.(type) {
	case *image.YCbCr, *image.Gray, *image.Gray16, *image.CMYK:
		return false
	}
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return true
			}
		}
	}
	return false
}

// splitDataURL returns the declared mime and the decoded payload of a base64
// data URL. Any other string (including an http(s) URL) reports ok=false.
func splitDataURL(raw string) (string, []byte, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:") {
		return "", nil, false
	}
	comma := strings.Index(raw, ",")
	if comma < 0 {
		return "", nil, false
	}
	header := raw[len("data:"):comma]
	if !strings.Contains(strings.ToLower(header), "base64") {
		// Percent-encoded data URLs are not used for images in practice.
		return "", nil, false
	}
	mime := strings.TrimSpace(strings.SplitN(header, ";", 2)[0])
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw[comma+1:]))
	if err != nil {
		return "", nil, false
	}
	if mime == "" {
		mime = SniffMime(decoded)
	}
	return strings.ToLower(mime), decoded, true
}

// SniffMime reports the image mime implied by the leading bytes. A data URL
// sometimes carries a wrong or generic declared type, so the bytes are the more
// trustworthy signal.
func SniffMime(data []byte) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(http.DetectContentType(data), ";", 2)[0]))
}

// NormalizeImagePart rewrites the data URL inside one OpenAI-style content part
// and reports whether anything changed. It understands the shapes callers use
// for images and leaves any other part untouched.
func NormalizeImagePart(part map[string]any) bool {
	changed := false
	// image_url may be a plain string or an object carrying "url".
	switch holder := part["image_url"].(type) {
	case string:
		if next, ok := NormalizeDataURL(holder); ok {
			part["image_url"] = next
			changed = true
		}
	case map[string]any:
		if current, ok := holder["url"].(string); ok {
			if next, ok := NormalizeDataURL(current); ok {
				holder["url"] = next
				changed = true
			}
		}
	}
	// Some clients put the URL at the top level, or send raw base64 plus a
	// mime type instead of a data URL.
	if current, ok := part["url"].(string); ok {
		if next, ok := NormalizeDataURL(current); ok {
			part["url"] = next
			changed = true
		}
	}
	if data, ok := part["data"].(string); ok && data != "" {
		mime, _ := part["mime_type"].(string)
		if mime == "" {
			mime, _ = part["media_type"].(string)
		}
		if next, ok := ConvertBase64(data, mime); ok {
			part["data"] = next.Base64
			part["mime_type"] = next.Mime
			changed = true
		}
	}
	return changed
}

// Converted is a re-encoded image payload plus the mime it is now encoded as.
type Converted struct {
	Base64 string
	Mime   string
}

// ConvertBase64 re-encodes raw base64 image data that is not already PNG or
// JPEG. It reports ok=false when no change was needed or possible.
func ConvertBase64(data, mime string) (Converted, bool) {
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
	if err != nil || len(payload) == 0 || len(payload) > maxInputBytes {
		return Converted{}, false
	}
	if mime == "" {
		mime = SniffMime(payload)
	}
	if acceptable(mime) {
		return Converted{}, false
	}
	img, _, err := image.Decode(bytes.NewReader(payload))
	if err != nil {
		return Converted{}, false
	}
	encoded, outMime, err := encode(img)
	if err != nil {
		return Converted{}, false
	}
	return Converted{Base64: base64.StdEncoding.EncodeToString(encoded), Mime: outMime}, true
}
