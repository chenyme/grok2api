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
	maxChatAttachments        = 8
	maxChatAttachmentTotal    = 64 << 20
	maxRemoteAttachmentURLLen = 8192
)

var blockedRemoteImagePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

var (
	errInvalidChatAttachment = errors.New("对话附件无效")
	errInvalidChatImage      = errors.New("对话图片无效")
	errInvalidChatFile       = errors.New("对话文件无效")
)

type uploadedFile struct {
	ID  string
	URI string
}

type remoteImageTarget struct {
	originalURL *url.URL
	fetchURL    *url.URL
	serverName  string
	hostHeader  string
}

type remoteImageResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// prepareChatAttachments 在同一账号和出口租约内解析、下载并上传对话附件。
func (a *Adapter) prepareChatAttachments(ctx context.Context, cfg Config, lease *egress.Lease, token string, inputs []chatAttachmentInput) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > maxChatAttachments {
		return nil, fmt.Errorf("%w: 单次对话最多支持 %d 个附件", errInvalidChatAttachment, maxChatAttachments)
	}
	pending := make([]provider.ImageInput, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	total := int64(0)
	for _, input := range inputs {
		input.Source = strings.TrimSpace(input.Source)
		key := fmt.Sprintf("%t\x00%s\x00%s", input.Image, input.Filename, input.Source)
		if _, exists := seen[key]; exists {
			continue
		}
		var file provider.ImageInput
		var err error
		if input.Image {
			file, err = a.loadChatImage(ctx, lease, input.Source, cfg.MaxInputImageBytes)
		} else {
			file, err = a.loadChatFile(ctx, lease, input.Source, input.Filename, cfg.MaxInputImageBytes)
		}
		if err != nil {
			return nil, err
		}
		size := int64(len(file.Data))
		if size > maxChatAttachmentTotal || total > maxChatAttachmentTotal-size {
			return nil, fmt.Errorf("%w: 总大小不能超过 64 MiB", errInvalidChatAttachment)
		}
		total += size
		seen[key] = struct{}{}
		pending = append(pending, file)
	}
	attachments := make([]string, 0, len(pending))
	directUploadAvailable := true
	for _, file := range pending {
		uploaded, directAvailable, err := a.uploadFileWithFallback(ctx, cfg, lease, token, file, cfg.BaseURL+"/", "", directUploadAvailable)
		directUploadAvailable = directAvailable
		if err != nil {
			return nil, err
		}
		if uploaded.ID == "" {
			return nil, fmt.Errorf("上传附件成功但上游未返回 fileMetadataId")
		}
		attachments = append(attachments, uploaded.ID)
	}
	return attachments, nil
}

func (a *Adapter) loadChatImage(ctx context.Context, lease *egress.Lease, input string, maxBytes int64) (provider.ImageInput, error) {
	if strings.HasPrefix(strings.ToLower(input), "data:") {
		return parseChatImageDataURI(input, maxBytes)
	}
	target, err := validateRemoteImageURL(ctx, input)
	if err != nil {
		return provider.ImageInput{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target.fetchURL.String(), nil)
	if err != nil {
		return provider.ImageInput{}, err
	}
	request.Host = target.hostHeader
	// 外部图片地址不接收 SSO 或 Cloudflare Cookie，避免把上游凭据泄漏给第三方。
	request.Header = remoteImageHeaders(lease.UserAgent)
	response, err := lease.DoPinnedHTTPS(request, target.serverName)
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
	return provider.ImageInput{Filename: imageFilename(target.originalURL, mimeType), MIMEType: mimeType, Data: raw}, nil
}

func (a *Adapter) loadChatFile(ctx context.Context, lease *egress.Lease, input, filename string, maxBytes int64) (provider.ImageInput, error) {
	if strings.HasPrefix(strings.ToLower(input), "data:") {
		return parseChatFileDataURI(input, filename, maxBytes)
	}
	target, err := validateRemoteAttachmentURL(ctx, input, errInvalidChatFile)
	if err != nil {
		return provider.ImageInput{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target.fetchURL.String(), nil)
	if err != nil {
		return provider.ImageInput{}, err
	}
	request.Host = target.hostHeader
	request.Header = remoteFileHeaders(lease.UserAgent)
	response, err := lease.DoPinnedHTTPS(request, target.serverName)
	if err != nil {
		return provider.ImageInput{}, fmt.Errorf("下载对话文件: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.ImageInput{}, fmt.Errorf("%w: 下载地址返回 %d", errInvalidChatFile, response.StatusCode)
	}
	if response.ContentLength > maxBytes {
		return provider.ImageInput{}, fmt.Errorf("%w: 文件超过 %d MiB", errInvalidChatFile, maxBytes>>20)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil || int64(len(raw)) > maxBytes {
		return provider.ImageInput{}, fmt.Errorf("%w: 下载失败或文件超过 %d MiB", errInvalidChatFile, maxBytes>>20)
	}
	mimeType, err := validatedChatFileMIME(raw, response.Header.Get("Content-Type"), firstNonEmpty(filename, path.Base(target.originalURL.Path)))
	if err != nil {
		return provider.ImageInput{}, err
	}
	return provider.ImageInput{Filename: chatFileName(filename, target.originalURL, mimeType), MIMEType: mimeType, Data: raw}, nil
}

func remoteImageHeaders(userAgent string) http.Header {
	value := http.Header{}
	value.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/gif;q=0.9,*/*;q=0.1")
	value.Set("User-Agent", userAgent)
	return value
}

func remoteFileHeaders(userAgent string) http.Header {
	value := http.Header{}
	value.Set("Accept", "application/pdf,text/*,application/json,application/xml,application/rtf,application/msword,application/zip,image/*,*/*;q=0.1")
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

func parseChatFileDataURI(value, filename string, maxBytes int64) (provider.ImageInput, error) {
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:") || !strings.Contains(strings.ToLower(header), ";base64") {
		return provider.ImageInput{}, fmt.Errorf("%w: file_data 必须是 Base64 data URI", errInvalidChatFile)
	}
	encoded = strings.Join(strings.Fields(encoded), "")
	if encoded == "" || int64(base64.StdEncoding.DecodedLen(len(encoded))) > maxBytes {
		return provider.ImageInput{}, fmt.Errorf("%w: 文件为空或超过 %d MiB", errInvalidChatFile, maxBytes>>20)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(raw) == 0 || int64(len(raw)) > maxBytes {
		return provider.ImageInput{}, fmt.Errorf("%w: Base64 无效或文件超过 %d MiB", errInvalidChatFile, maxBytes>>20)
	}
	declared := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.ToLower(header), "data:"), ";base64"))
	mimeType, err := validatedChatFileMIME(raw, declared, filename)
	if err != nil {
		return provider.ImageInput{}, err
	}
	return provider.ImageInput{Filename: chatFileName(filename, nil, mimeType), MIMEType: mimeType, Data: raw}, nil
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

func validatedChatFileMIME(data []byte, declared, filename string) (string, error) {
	detected := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0]))
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if hinted := chatFileMIMEFromExtension(path.Ext(filename)); hinted != "" && (declared == "" || declared == "application/octet-stream" || declared == "application/zip") {
		declared = hinted
	}
	if supportedChatImageMIME(declared) || supportedChatImageMIME(detected) {
		return validatedImageMIME(data, declared)
	}
	if supportedChatFileMIME(declared) {
		return declared, nil
	}
	if supportedChatFileMIME(detected) {
		return detected, nil
	}
	return "", fmt.Errorf("%w: 不支持该文件格式", errInvalidChatFile)
}

func chatFileMIMEFromExtension(extension string) string {
	switch strings.ToLower(extension) {
	case ".pdf":
		return "application/pdf"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".rtf":
		return "application/rtf"
	case ".doc":
		return "application/msword"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".ppt":
		return "application/vnd.ms-powerpoint"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".xls":
		return "application/vnd.ms-excel"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".csv":
		return "text/csv"
	case ".md", ".markdown":
		return "text/markdown"
	case ".html", ".htm":
		return "text/html"
	case ".txt", ".log":
		return "text/plain"
	default:
		return ""
	}
}

func supportedChatFileMIME(value string) bool {
	switch value {
	case "application/pdf", "application/json", "application/xml", "application/rtf", "application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.ms-powerpoint", "application/vnd.ms-excel",
		"text/plain", "text/csv", "text/markdown", "text/html", "text/xml", "text/rtf":
		return true
	default:
		return false
	}
}

func validateRemoteImageURL(ctx context.Context, raw string) (*remoteImageTarget, error) {
	return validateRemoteAttachmentURL(ctx, raw, errInvalidChatImage)
}

func validateRemoteImageURLWithResolver(ctx context.Context, raw string, resolver remoteImageResolver) (*remoteImageTarget, error) {
	return validateRemoteAttachmentURLWithResolver(ctx, raw, resolver, errInvalidChatImage)
}

func validateRemoteAttachmentURL(ctx context.Context, raw string, invalid error) (*remoteImageTarget, error) {
	return validateRemoteAttachmentURLWithResolver(ctx, raw, net.DefaultResolver, invalid)
}

func validateRemoteAttachmentURLWithResolver(ctx context.Context, raw string, resolver remoteImageResolver, invalid error) (*remoteImageTarget, error) {
	if len(raw) == 0 || len(raw) > maxRemoteAttachmentURLLen {
		return nil, fmt.Errorf("%w: URL 为空或过长", invalid)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Port() != "" && parsed.Port() != "443") {
		return nil, fmt.Errorf("%w: URL 必须是无用户信息的 HTTPS 地址", invalid)
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, fmt.Errorf("%w: URL 指向受限主机", invalid)
	}
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		if !publicRemoteImageAddress(address) {
			return nil, fmt.Errorf("%w: URL 指向非公网地址", invalid)
		}
		return newRemoteImageTarget(parsed, host, address), nil
	}
	resolveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addresses, err := resolver.LookupNetIP(resolveCtx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("%w: 无法解析附件主机", invalid)
	}
	for _, address := range addresses {
		if !publicRemoteImageAddress(address.Unmap()) {
			return nil, fmt.Errorf("%w: URL 解析到非公网地址", invalid)
		}
	}
	return newRemoteImageTarget(parsed, host, addresses[0].Unmap()), nil
}

func newRemoteImageTarget(original *url.URL, serverName string, address netip.Addr) *remoteImageTarget {
	fetchURL := *original
	fetchURL.Host = net.JoinHostPort(address.String(), "443")
	fetchURL.Fragment = ""
	return &remoteImageTarget{originalURL: original, fetchURL: &fetchURL, serverName: serverName, hostHeader: original.Host}
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

func chatFileName(preferred string, source *url.URL, mimeType string) string {
	name := strings.TrimSpace(preferred)
	if name == "" && source != nil {
		name = path.Base(source.Path)
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" || name == "" || len(name) > 160 || strings.IndexFunc(name, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		name = "file" + chatFileExtension(mimeType)
	}
	if path.Ext(name) == "" {
		name += chatFileExtension(mimeType)
	}
	return name
}

func chatFileExtension(mimeType string) string {
	switch mimeType {
	case "application/pdf":
		return ".pdf"
	case "application/json":
		return ".json"
	case "application/xml", "text/xml":
		return ".xml"
	case "application/rtf", "text/rtf":
		return ".rtf"
	case "application/msword":
		return ".doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.ms-powerpoint":
		return ".ppt"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	case "application/vnd.ms-excel":
		return ".xls"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "text/csv":
		return ".csv"
	case "text/markdown":
		return ".md"
	case "text/html":
		return ".html"
	case "text/plain":
		return ".txt"
	default:
		return imageExtension(mimeType)
	}
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
