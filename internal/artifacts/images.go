package artifacts

import (
	"net/http"
	"path"
	"strings"
)

// MaxImageBytes bounds one image attached to a message (pasted or dropped
// in the web inbox, materialized on the recipient's node).
const MaxImageBytes = 20 << 20

// imageExts are the raster image types a message may carry as a viewable
// image, with the extension a materialized copy gets. SVG is excluded: it is
// active content (see IsActiveType).
var imageExts = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// SniffImage reports the media type of an allowed image from its leading
// bytes (http.DetectContentType; at most the first 512 are read). A declared
// type or filename is never trusted.
func SniffImage(head []byte) (mediaType string, ok bool) {
	mt := http.DetectContentType(head)
	if _, ok := imageExts[mt]; !ok {
		return mt, false
	}
	return mt, true
}

// ImageFilename is SafeFilename(name) with the extension of mediaType (an
// allowed image type), so a "photo.txt" holding PNG bytes becomes
// "photo.png". An empty name becomes "image<ext>".
func ImageFilename(name, mediaType string) string {
	ext := imageExts[mediaType]
	base := SafeFilename(name)
	if strings.TrimSpace(name) == "" {
		base = "image"
	}
	base = strings.TrimSuffix(base, path.Ext(base))
	if base == "" {
		base = "image"
	}
	return truncate(base, 255-len(ext)) + ext
}

// ImageExt is the extension for an allowed image media type ("" if the type
// is not an allowed image).
func ImageExt(mediaType string) string { return imageExts[mediaType] }
