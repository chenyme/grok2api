package mediafile

import (
	"mime"
	"strings"
)

// VideoExtension returns the canonical file extension for a supported video
// media type. Parameters such as codecs are ignored when selecting the suffix.
func VideoExtension(contentType string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return "", false
	}
	switch strings.ToLower(mediaType) {
	case "video/mp4":
		return ".mp4", true
	case "video/webm":
		return ".webm", true
	case "video/quicktime":
		return ".mov", true
	default:
		return "", false
	}
}

// VideoContentDisposition builds a safe inline filename for video responses.
// Public request and asset identifiers are restricted to a conservative ASCII
// subset before they are placed in an HTTP response header.
func VideoContentDisposition(name, contentType string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, strings.TrimSpace(name))
	if name == "" {
		name = "video"
	}
	if extension, ok := VideoExtension(contentType); ok {
		name += extension
	}
	return `inline; filename="` + name + `"`
}
