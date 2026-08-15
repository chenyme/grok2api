package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/gif"
	"image/png"
	"strings"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
)

const (
	buildGIFMaxPixels    = 64_000_000
	buildGIFMaxWalkDepth = 64
)

var (
	gif87aMagic = []byte("GIF87a")
	gif89aMagic = []byte("GIF89a")
)

// normalizeBuildInputGIFs converts inline GIF inputs to a PNG of their first
// frame because Grok Build only accepts static JPG, PNG, WebP, and ICO images.
// Detection uses decoded magic bytes so mislabeled data URIs are covered too.
func normalizeBuildInputGIFs(payload map[string]json.RawMessage) (bool, error) {
	raw, exists := payload["input"]
	if !exists || isEmptyJSON(raw) {
		return false, nil
	}
	normalized, changed, err := normalizeBuildGIFNode(raw, "input", 0)
	if err != nil {
		return false, err
	}
	if changed {
		payload["input"] = normalized
	}
	return changed, nil
}

func normalizeBuildGIFNode(raw json.RawMessage, param string, depth int) (json.RawMessage, bool, error) {
	if depth >= buildGIFMaxWalkDepth {
		return raw, false, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, false, nil
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, false, fmt.Errorf("解析 %s: %w", param, err)
		}
		changed := false
		for index, item := range items {
			normalized, itemChanged, err := normalizeBuildGIFNode(item, fmt.Sprintf("%s[%d]", param, index), depth+1)
			if err != nil {
				return nil, false, err
			}
			if itemChanged {
				items[index] = normalized
				changed = true
			}
		}
		if !changed {
			return raw, false, nil
		}
		normalized, err := json.Marshal(items)
		return normalized, true, err
	case '{':
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, false, fmt.Errorf("解析 %s: %w", param, err)
		}
		changed := false
		var blockType string
		_ = json.Unmarshal(value["type"], &blockType)
		if blockType == "input_image" {
			var imageURL string
			if err := json.Unmarshal(value["image_url"], &imageURL); err == nil && imageURL != "" {
				normalized, imageChanged, err := normalizeBuildGIFDataURI(imageURL, param+".image_url")
				if err != nil {
					return nil, false, err
				}
				if imageChanged {
					value["image_url"] = mustJSON(normalized)
					changed = true
				}
			}
		}
		for _, key := range []string{"content", "output"} {
			child, exists := value[key]
			if !exists || isEmptyJSON(child) {
				continue
			}
			normalized, childChanged, err := normalizeBuildGIFNode(child, param+"."+key, depth+1)
			if err != nil {
				return nil, false, err
			}
			if childChanged {
				value[key] = normalized
				changed = true
			}
		}
		if !changed {
			return raw, false, nil
		}
		normalized, err := json.Marshal(value)
		return normalized, true, err
	default:
		return raw, false, nil
	}
}

func normalizeBuildGIFDataURI(value, param string) (string, bool, error) {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:") || !strings.Contains(strings.ToLower(header), ";base64") {
		return value, false, nil
	}
	compact := strings.Map(func(character rune) rune {
		if character == ' ' || character == '\t' || character == '\r' || character == '\n' {
			return -1
		}
		return character
	}, encoded)
	if len(compact) < 8 {
		return value, false, nil
	}
	prefix, err := base64.StdEncoding.DecodeString(compact[:8])
	if err != nil || !hasGIFMagic(prefix) {
		return value, false, nil
	}
	if base64.StdEncoding.DecodedLen(len(compact)) > mediadomain.MaxInputAssetBytes {
		return "", false, invalidBuildGIF(param, fmt.Sprintf("图片超过 %d MiB", mediadomain.MaxInputAssetBytes>>20))
	}
	raw, err := decodeBuildBase64(compact)
	if err != nil {
		return "", false, invalidBuildGIF(param, "Base64 无效")
	}
	if len(raw) > mediadomain.MaxInputAssetBytes {
		return "", false, invalidBuildGIF(param, fmt.Sprintf("图片超过 %d MiB", mediadomain.MaxInputAssetBytes>>20))
	}
	config, err := gif.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return "", false, invalidBuildGIF(param, "GIF 数据损坏")
	}
	if int64(config.Width)*int64(config.Height) > buildGIFMaxPixels {
		return "", false, invalidBuildGIF(param, fmt.Sprintf("GIF 像素超过 %d MP", buildGIFMaxPixels/1_000_000))
	}
	firstFrame, err := gif.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", false, invalidBuildGIF(param, "GIF 数据损坏")
	}
	var output bytes.Buffer
	if err := png.Encode(&output, firstFrame); err != nil {
		return "", false, invalidBuildGIF(param, "首帧 PNG 编码失败")
	}
	if output.Len() > mediadomain.MaxInputAssetBytes {
		return "", false, invalidBuildGIF(param, fmt.Sprintf("首帧 PNG 超过 %d MiB", mediadomain.MaxInputAssetBytes>>20))
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(output.Bytes()), true, nil
}

func decodeBuildBase64(value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

func hasGIFMagic(value []byte) bool {
	return bytes.HasPrefix(value, gif87aMagic) || bytes.HasPrefix(value, gif89aMagic)
}

func invalidBuildGIF(param, message string) error {
	return &responsesRequestError{Message: "Grok Build GIF 输入无效：" + message, Param: param, Code: "invalid_image"}
}
