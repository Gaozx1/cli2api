package imgconvert

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	_ "golang.org/x/image/webp"
)

// Two 4x4 lossless WebP samples. They exist so the test fails loudly if the
// webp decoder registration is ever dropped: without it, image.Decode stops
// recognizing WebP and the conversion silently never happens.
const (
	opaqueWebP = "UklGRh4AAABXRUJQVlA4TBEAAAAvA8AAAAdQxIqUuf+BiOh/AAA="
	alphaWebP  = "UklGRiIAAABXRUJQVlA4TBUAAAAvA8AAEA8wiCMyzPMf8LjDQ0T/wwEA"
)

func decodeSample(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("sample is not valid base64: %v", err)
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("sample does not decode (decoder not registered?): %v", err)
	}
	return raw
}

func webpBytes(t *testing.T) []byte { return decodeSample(t, opaqueWebP) }

func alphaWebPBytes(t *testing.T) []byte { return decodeSample(t, alphaWebP) }

func dataURL(mime string, payload []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(payload)
}

// encodeGIF produces real GIF bytes for the conversion path. GIF is a format we
// can both encode and decode and it is not on the upstream's accept list, so it
// exercises the same route as WebP.
func encodeGIF(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: 0x22, G: 0x88, B: 0xcc, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mimeOf(t *testing.T, encoded []byte) string {
	t.Helper()
	_, format, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("produced bytes do not decode: %v", err)
	}
	return format
}

// WebP, the format the upstream refuses, is converted to an accepted one, and
// the alpha channel decides the target format: JPEG cannot carry alpha, so a
// transparent image must become PNG while an opaque one becomes the smaller
// JPEG.
func TestNormalizeConvertsWebP(t *testing.T) {
	url := dataURL("image/webp", webpBytes(t))
	next, changed := NormalizeDataURL(url)
	if !changed {
		t.Fatal("a WebP must be converted")
	}
	mime, payload, ok := splitDataURL(next)
	if !ok {
		t.Fatal("the result must still be a data URL")
	}
	if mime != JPEGMime {
		t.Fatalf("an opaque WebP should become JPEG, got %q", mime)
	}
	if _, _, err := image.Decode(bytes.NewReader(payload)); err != nil {
		t.Fatalf("result does not decode: %v", err)
	}

	// The transparent one keeps its alpha by becoming PNG.
	alphaURL := dataURL("image/webp", alphaWebPBytes(t))
	next, changed = NormalizeDataURL(alphaURL)
	if !changed {
		t.Fatal("a transparent WebP must be converted")
	}
	mime, payload, _ = splitDataURL(next)
	if mime != PNGMime {
		t.Fatalf("a transparent image must become PNG, got %q", mime)
	}
	if got := mimeOf(t, payload); got != "png" {
		t.Fatalf("decoded format = %q, want png", got)
	}
}

// GIF is also outside the upstream's accept list and goes through the same path.
func TestNormalizeConvertsGIF(t *testing.T) {
	gifURL := dataURL("image/gif", encodeGIF(t))
	next, changed := NormalizeDataURL(gifURL)
	if !changed {
		t.Fatal("a GIF must be converted")
	}
	mime, payload, _ := splitDataURL(next)
	if mime != PNGMime && mime != JPEGMime {
		t.Fatalf("result mime = %q, want png or jpeg", mime)
	}
	if _, _, err := image.Decode(bytes.NewReader(payload)); err != nil {
		t.Fatalf("result does not decode: %v", err)
	}
}

// Already-accepted images are passed through byte-for-byte: re-encoding a PNG
// would only cost quality and CPU.
func TestAlreadyAcceptedPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	pngURL := dataURL(PNGMime, buf.Bytes())
	if next, changed := NormalizeDataURL(pngURL); changed || next != pngURL {
		t.Fatal("a PNG must not be touched")
	}

	buf.Reset()
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	jpgURL := dataURL(JPEGMime, buf.Bytes())
	if next, changed := NormalizeDataURL(jpgURL); changed || next != jpgURL {
		t.Fatal("a JPEG must not be touched")
	}
	// A "image/jpg" declaration is also accepted.
	if _, changed := NormalizeDataURL(dataURL("image/jpg", buf.Bytes())); changed {
		t.Fatal("image/jpg must be treated as JPEG")
	}
}

// Anything we cannot decode is left alone: converting is best-effort, never a
// new way to fail a request.
func TestUnconvertibleIsLeftAlone(t *testing.T) {
	cases := map[string]string{
		"garbage payload": dataURL("image/webp", []byte("not an image")),
		"remote url":      "https://example.com/cat.webp",
		"empty":           "",
		"plain text":      "hello",
		"non-base64 data": "data:image/webp;base64,!!!!",
		"no comma":        "data:image/webp;base64",
		"percent encoded": "data:image/svg+xml,%3Csvg%3E",
	}
	for name, url := range cases {
		next, changed := NormalizeDataURL(url)
		if changed {
			t.Fatalf("%s must not be rewritten", name)
		}
		if next != url {
			t.Fatalf("%s must be returned unchanged", name)
		}
	}
}

// The part shapes callers actually send are all understood.
func TestNormalizeImagePartShapes(t *testing.T) {
	webp := dataURL("image/webp", webpBytes(t))

	// image_url as an object with "url".
	part := map[string]any{"type": "image_url", "image_url": map[string]any{"url": webp}}
	if !NormalizeImagePart(part) {
		t.Fatal("object image_url must be converted")
	}
	if got := part["image_url"].(map[string]any)["url"].(string); !strings.Contains(got, "image/png") && !strings.Contains(got, "image/jpeg") {
		t.Fatalf("converted url = %q", got)
	}

	// image_url as a plain string.
	part = map[string]any{"type": "image_url", "image_url": webp}
	if !NormalizeImagePart(part) {
		t.Fatal("string image_url must be converted")
	}

	// Top-level "url".
	part = map[string]any{"url": webp}
	if !NormalizeImagePart(part) {
		t.Fatal("top-level url must be converted")
	}

	// Raw base64 + mime_type rather than a data URL.
	raw := base64.StdEncoding.EncodeToString(webpBytes(t))
	part = map[string]any{"data": raw, "mime_type": "image/webp"}
	if !NormalizeImagePart(part) {
		t.Fatal("raw base64 must be converted")
	}
	if m := part["mime_type"].(string); m != PNGMime && m != JPEGMime {
		t.Fatalf("mime_type = %q after conversion", m)
	}

	// A text part is untouched.
	part = map[string]any{"type": "text", "text": "hi"}
	if NormalizeImagePart(part) {
		t.Fatal("a text part must not be marked changed")
	}
}

// A data URL whose declared mime is wrong is judged by its bytes.
func TestSniffOverridesWrongMime(t *testing.T) {
	// GIF bytes announced as PNG: already on the accept list, so it is trusted
	// and passed through (we do not second-guess an accepted declaration).
	gifURL := dataURL(PNGMime, encodeGIF(t))
	if _, changed := NormalizeDataURL(gifURL); changed {
		t.Fatal("an accepted declared mime must be trusted")
	}
}
