package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	maxChatImageAttachments = 8
	maxChatImageTotalBytes  = 64 << 20
	maxRemoteImageURLBytes  = 8192
)

var blockedRemoteImagePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

var errInvalidChatImage = errors.New("对话图片无效")

type uploadedFile struct {
	ID  string
	URI string
}

// prepareChatAttachments 在同一账号和出口租约内解析、下载并上传对话图片。
func (a *Adapter) prepareChatAttachments(ctx context.Context, cfg Config, lease *egress.Lease, token string, inputs []string) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > maxChatImageAttachments {
		return nil, fmt.Errorf("%w: 单次对话最多支持 %d 张图片", errInvalidChatImage, maxChatImageAttachments)
	}
	attachments := make([]string, 0, len(inputs))
	seen := make(map[string]string, len(inputs))
	total := int64(0)
	for _, input := range inputs {
		input = strings.TrimSpace(input)
		if seen[input] != "" {
			continue
		}
		image, err := a.loadChatImage(ctx, lease, input, cfg.MaxInputImageBytes)
		if err != nil {
			return nil, err
		}
		total += int64(len(image.Data))
		if total > maxChatImageTotalBytes {
			return nil, fmt.Errorf("%w: 对话图片总大小不能超过 64 MiB", errInvalidChatImage)
		}
		uploaded, err := a.uploadImage(ctx, cfg, lease, token, image, cfg.BaseURL+"/")
		if err != nil {
			return nil, err
		}
		if uploaded.ID == "" {
			return nil, fmt.Errorf("上传图片成功但上游未返回 fileMetadataId")
		}
		seen[input] = uploaded.ID
		attachments = append(attachments, uploaded.ID)
	}
	return attachments, nil
}

func (a *Adapter) loadChatImage(ctx context.Context, lease *egress.Lease, input string, maxBytes int64) (provider.ImageInput, error) {
	if strings.HasPrefix(strings.ToLower(input), "data:") {
		return parseChatImageDataURI(input, maxBytes)
	}
	parsed, err := validateRemoteImageURL(ctx, input)
	if err != nil {
		return provider.ImageInput{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return provider.ImageInput{}, err
	}
	// 外部图片地址不接收 SSO 或 Cloudflare Cookie，避免把上游凭据泄漏给第三方。
	request.Header = remoteImageHeaders(lease.UserAgent)
	response, err := lease.Do(request)
	if err != nil {
		return provider.ImageInput{}, fmt.Errorf("下载对话图片: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.ImageInput{}, fmt.Errorf("%w: 下载地址返回 %d", errInvalidChatImage, response.StatusCode)
	}
	if response.ContentLength > maxBytes {
		return provider.ImageInput{}, fmt.Errorf("%w: 图片超过 %d MiB", errInvalidChatImage, maxBytes>>20)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil || int64(len(raw)) > maxBytes {
		return provider.ImageInput{}, fmt.Errorf("%w: 下载失败或图片超过 %d MiB", errInvalidChatImage, maxBytes>>20)
	}
	mimeType, err := validatedImageMIME(raw, response.Header.Get("Content-Type"))
	if err != nil {
		return provider.ImageInput{}, err
	}
	return provider.ImageInput{Filename: imageFilename(parsed, mimeType), MIMEType: mimeType, Data: raw}, nil
}

func remoteImageHeaders(userAgent string) http.Header {
	value := http.Header{}
	value.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/gif;q=0.9,*/*;q=0.1")
	value.Set("User-Agent", userAgent)
	return value
}

func parseChatImageDataURI(value string, maxBytes int64) (provider.ImageInput, error) {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:image/") || !strings.Contains(strings.ToLower(header), ";base64") {
		return provider.ImageInput{}, fmt.Errorf("%w: data URI 必须是 Base64 图片", errInvalidChatImage)
	}
	encoded = strings.Join(strings.Fields(encoded), "")
	if encoded == "" || int64(base64.StdEncoding.DecodedLen(len(encoded))) > maxBytes {
		return provider.ImageInput{}, fmt.Errorf("%w: 图片为空或超过 %d MiB", errInvalidChatImage, maxBytes>>20)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(raw) == 0 || int64(len(raw)) > maxBytes {
		return provider.ImageInput{}, fmt.Errorf("%w: Base64 无效或图片超过 %d MiB", errInvalidChatImage, maxBytes>>20)
	}
	declared := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(header), "data:"), ";base64"))
	mimeType, err := validatedImageMIME(raw, declared)
	if err != nil {
		return provider.ImageInput{}, err
	}
	return provider.ImageInput{Filename: "image" + imageExtension(mimeType), MIMEType: mimeType, Data: raw}, nil
}

func validatedImageMIME(data []byte, declared string) (string, error) {
	detected := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if !supportedChatImageMIME(detected) {
		return "", fmt.Errorf("%w: 不支持该图片格式", errInvalidChatImage)
	}
	if declared != "" && declared != "application/octet-stream" && declared != detected {
		return "", fmt.Errorf("%w: Content-Type 与实际内容不一致", errInvalidChatImage)
	}
	return detected, nil
}

func supportedChatImageMIME(value string) bool {
	switch value {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

func validateRemoteImageURL(ctx context.Context, raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > maxRemoteImageURLBytes {
		return nil, fmt.Errorf("%w: URL 为空或过长", errInvalidChatImage)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "443") {
		return nil, fmt.Errorf("%w: URL 必须是无用户信息的 HTTPS 地址", errInvalidChatImage)
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, fmt.Errorf("%w: URL 指向受限主机", errInvalidChatImage)
	}
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		if !publicRemoteImageAddress(address) {
			return nil, fmt.Errorf("%w: URL 指向非公网地址", errInvalidChatImage)
		}
		return parsed, nil
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("%w: 无法解析图片主机", errInvalidChatImage)
	}
	for _, value := range addresses {
		address, ok := netip.AddrFromSlice(value.IP)
		if !ok || !publicRemoteImageAddress(address.Unmap()) {
			return nil, fmt.Errorf("%w: URL 解析到非公网地址", errInvalidChatImage)
		}
	}
	return parsed, nil
}

func publicRemoteImageAddress(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedRemoteImagePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func imageFilename(value *url.URL, mimeType string) string {
	name := path.Base(value.Path)
	if name == "." || name == "/" || name == "" || len(name) > 160 || strings.IndexFunc(name, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return "image" + imageExtension(mimeType)
	}
	if path.Ext(name) == "" {
		name += imageExtension(mimeType)
	}
	return name
}

func imageExtension(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".bin"
	}
}
