package media

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/netguard"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

const (
	// 20 MiB 为各 Provider 的共同安全输入上限，同时为全局 32 MiB multipart 请求上限保留编码开销。
	ingestMaxImageBytes = mediadomain.MaxInputAssetBytes
	// ingestFetchTimeout 是 URL 导入单次抓取的整体超时。
	ingestFetchTimeout = 20 * time.Second
	// ingestResolveTimeout 限制每个重定向目标的 DNS 解析时间。
	ingestResolveTimeout = 3 * time.Second
	// ingestMaxRedirects 限制跨域重定向次数；每一跳都会重新解析、校验并固定目标 IP。
	ingestMaxRedirects = 5
	// 独立 bulkhead 防止临时导入与推理流量争抢内存和连接。
	ingestConcurrency = 4
)

var (
	// errImageTooLarge 表示下载/上传的图片超过读取上限。
	errImageTooLarge = errors.New("图片超过大小上限")
	// errFetchBlocked 表示目标地址被 SSRF 防护拒绝（内网/环回/链路本地/元数据等）。
	errFetchBlocked = errors.New("目标地址不允许访问")
)

type importURLResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// importTarget 把用户 URL 的请求语义与实际连接目标分离：fetchURL 只包含已验证的公网 IP，
// hostHeader/serverName 则保留原始虚拟主机与 TLS 证书校验语义。
type importTarget struct {
	fetchURL   *url.URL
	hostHeader string
	serverName string
}

// ssrfSafeControl 在 TCP 连接建立前检查目标 IP，拒绝私有/环回/链路本地/未指定/多播地址及云元数据地址。
// 返回的错误包裹 errFetchBlocked，便于上层用 errors.Is 识别为"地址被拒绝"。
func ssrfSafeControl(network, address string, _ syscall.RawConn) error {
	if network != "tcp4" && network != "tcp6" && network != "tcp" {
		return fmt.Errorf("不支持的网络类型 %q: %w", network, errFetchBlocked)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("解析目标地址失败 %q: %w", address, errFetchBlocked)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("目标地址不是有效 IP %q: %w", host, errFetchBlocked)
	}
	if !isPublicIP(ip.Unmap()) {
		return fmt.Errorf("拒绝访问非公网地址 %s: %w", host, errFetchBlocked)
	}
	return nil
}

// isPublicIP 仅当 IP 是可路由公网地址时返回 true。
func isPublicIP(ip netip.Addr) bool {
	return netguard.IsPublicAddress(ip)
}

// isValidRedirectURL 是初始导入地址和每次重定向共用的语法级安全守卫。
// 网络级策略由 resolveImportTarget 和 ssrfSafeControl 继续执行。
func isValidRedirectURL(parsed *url.URL) bool {
	if parsed == nil || (!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return false
	}
	return true
}

// resolveImportTarget 先解析并校验全部 DNS 结果，再固定本次请求的连接 IP。
// 这同时消除了校验与拨号之间的 DNS rebinding 窗口；ssrfSafeControl 仍在拨号时做第二道校验。
func resolveImportTarget(ctx context.Context, parsed *url.URL, resolver importURLResolver) (*importTarget, error) {
	if !isValidRedirectURL(parsed) {
		return nil, errFetchBlocked
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || strings.Contains(host, "%") {
		return nil, errFetchBlocked
	}

	var addresses []netip.Addr
	if address, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{address.Unmap()}
	} else {
		resolveCtx, cancel := context.WithTimeout(ctx, ingestResolveTimeout)
		defer cancel()
		resolved, err := resolver.LookupNetIP(resolveCtx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("解析图片主机失败: %w", err)
		}
		if len(resolved) == 0 {
			return nil, errors.New("解析图片主机失败: DNS 未返回地址")
		}
		addresses = make([]netip.Addr, 0, len(resolved))
		for _, address := range resolved {
			addresses = append(addresses, address.Unmap())
		}
	}
	for _, address := range addresses {
		if !isPublicIP(address) {
			return nil, fmt.Errorf("图片主机解析到非公网地址 %s: %w", address, errFetchBlocked)
		}
	}

	port := parsed.Port()
	if port == "" {
		if strings.EqualFold(parsed.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	fetchURL := *parsed
	fetchURL.Scheme = strings.ToLower(parsed.Scheme)
	fetchURL.Host = net.JoinHostPort(addresses[0].String(), port)
	fetchURL.Fragment = ""
	return &importTarget{fetchURL: &fetchURL, hostHeader: parsed.Host, serverName: host}, nil
}

func newIngestHTTPClient(target *importTarget) (*http.Client, *http.Transport) {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 10 * time.Second,
			Control:   ssrfSafeControl,
		}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, ServerName: target.serverName},
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		ExpectContinueTimeout:  1 * time.Second,
		MaxResponseHeaderBytes: 1 << 20,
		MaxIdleConns:           1,
		MaxConnsPerHost:        1,
		IdleConnTimeout:        30 * time.Second,
		ForceAttemptHTTP2:      true,
	}
	client := &http.Client{
		Transport: transport,
		// 重定向必须回到 fetchRemoteImage 重新解析和固定目标，禁止 net/http 自动跟随。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client, transport
}

func isImportRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

// fetchRemoteImage 抓取远端图片字节，读取上限 ingestMaxImageBytes，超限返回 errImageTooLarge。
// 目标地址被 SSRF 防护拒绝时返回 errFetchBlocked。
func fetchRemoteImage(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || !isValidRedirectURL(parsed) {
		return nil, errFetchBlocked
	}
	fetchCtx, cancel := context.WithTimeout(ctx, ingestFetchTimeout)
	defer cancel()

	for redirects := 0; ; redirects++ {
		target, err := resolveImportTarget(fetchCtx, parsed, net.DefaultResolver)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, target.fetchURL.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Host = target.hostHeader
		req.Header.Set("Accept", "image/*")
		req.Header.Set("User-Agent", "grok2api-media-importer/1.0")

		client, transport := newIngestHTTPClient(target)
		resp, err := client.Do(req)
		if err != nil {
			transport.CloseIdleConnections()
			if errors.Is(err, errFetchBlocked) {
				return nil, errFetchBlocked
			}
			return nil, err
		}

		if isImportRedirect(resp.StatusCode) && resp.Header.Get("Location") != "" {
			resp.Body.Close()
			transport.CloseIdleConnections()
			if redirects >= ingestMaxRedirects {
				return nil, errors.New("重定向次数过多")
			}
			next, err := parsed.Parse(resp.Header.Get("Location"))
			if err != nil || !isValidRedirectURL(next) {
				return nil, fmt.Errorf("重定向地址无效: %w", errFetchBlocked)
			}
			parsed = next
			continue
		}

		data, err := readImportedImage(resp)
		transport.CloseIdleConnections()
		return data, err
	}
}

func readImportedImage(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > ingestMaxImageBytes {
		return nil, errImageTooLarge
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, ingestMaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > ingestMaxImageBytes {
		return nil, errImageTooLarge
	}
	return data, nil
}

type importImageRequest struct {
	URL string `json:"url" binding:"required,max=8192"`
}

// importInputImageFromURL 从管理员提供的 URL 抓取图片并登记到带 TTL 的隐藏输入区。
// 路由在 /api/admin/v1 下，已由 AdminAuth 保护；抓取带 SSRF 防护。
func (h *Handler) importInputImageFromURL(c *gin.Context) {
	if !h.acquireIngest(c) {
		return
	}
	defer h.releaseIngest()
	var request importImageRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	rawURL := strings.TrimSpace(request.URL)
	if len(rawURL) > 8192 {
		response.Error(c, http.StatusBadRequest, "invalidImageURL", "图片 URL 过长")
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || !isValidRedirectURL(parsed) {
		response.Error(c, http.StatusBadRequest, "invalidImageURL", "图片 URL 无效，仅支持无凭据的 http/https 80/443 地址")
		return
	}

	data, err := fetchRemoteImage(c.Request.Context(), rawURL)
	if err != nil {
		switch {
		case errors.Is(err, errImageTooLarge):
			response.Error(c, http.StatusRequestEntityTooLarge, "imageTooLarge", "图片超过大小上限")
		case errors.Is(err, errFetchBlocked):
			response.Error(c, http.StatusBadRequest, "imageURLBlocked", "该地址不允许访问")
		default:
			response.Error(c, http.StatusBadGateway, "imageFetchFailed", "下载图片失败")
		}
		return
	}

	h.saveIngestedImage(c, data)
}

// uploadInputAsset 接收管理员上传的本地图片或视频（multipart 字段名 file），登记到临时输入区。
func (h *Handler) uploadInputAsset(c *gin.Context) {
	if !h.acquireIngest(c) {
		return
	}
	defer h.releaseIngest()
	fileHeader, err := c.FormFile("file")
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			response.Error(c, http.StatusRequestEntityTooLarge, "mediaTooLarge", "文件超过请求大小上限")
			return
		}
		response.Error(c, http.StatusBadRequest, "invalidRequest", "缺少上传文件")
		return
	}
	if fileHeader.Size > ingestMaxImageBytes {
		response.Error(c, http.StatusRequestEntityTooLarge, "mediaTooLarge", "文件超过大小上限")
		return
	}
	src, err := fileHeader.Open()
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaUploadReadFailed", "读取上传文件失败")
		return
	}
	defer src.Close()
	data, err := io.ReadAll(io.LimitReader(src, ingestMaxImageBytes+1))
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaUploadReadFailed", "读取上传文件失败")
		return
	}
	if int64(len(data)) > ingestMaxImageBytes {
		response.Error(c, http.StatusRequestEntityTooLarge, "mediaTooLarge", "文件超过大小上限")
		return
	}
	declaredMIME := strings.ToLower(strings.TrimSpace(strings.Split(fileHeader.Header.Get("Content-Type"), ";")[0]))
	detectedMIME := strings.ToLower(http.DetectContentType(data))
	if strings.HasPrefix(detectedMIME, "image/") {
		h.saveIngestedImage(c, data)
		return
	}
	if strings.HasPrefix(declaredMIME, "video/") || strings.HasPrefix(detectedMIME, "video/") {
		contentType := declaredMIME
		if contentType == "" {
			contentType = detectedMIME
		}
		asset, saveErr := h.service.SaveInputVideo(c.Request.Context(), contentType, bytes.NewReader(data))
		if saveErr != nil {
			switch {
			case errors.Is(saveErr, mediaapp.ErrVideoUploadTooLarge):
				response.Error(c, http.StatusRequestEntityTooLarge, "mediaTooLarge", "视频超过大小上限")
			case errors.Is(saveErr, mediaapp.ErrInvalidVideoUpload):
				response.Error(c, http.StatusBadRequest, "invalidVideo", "视频内容无效或格式不支持（mp4/webm/quicktime）")
			case errors.Is(saveErr, mediaapp.ErrMediaCapacity):
				response.Error(c, http.StatusInsufficientStorage, "mediaCapacityExceeded", "媒体临时存储容量不足")
			default:
				response.Error(c, http.StatusInternalServerError, "mediaSaveVideoFailed", "保存视频失败")
			}
			return
		}
		h.writeInputAsset(c, asset)
		return
	}
	response.Error(c, http.StatusBadRequest, "invalidMedia", "仅支持图片或视频文件")
}

// saveIngestedImage 收口两种临时输入路径：校验、落盘并登记 TTL，不进入图库。
func (h *Handler) saveIngestedImage(c *gin.Context, data []byte) {
	asset, err := h.service.SaveInputImage(c.Request.Context(), data)
	if errors.Is(err, mediaapp.ErrInvalidImage) {
		response.Error(c, http.StatusBadRequest, "invalidImage", "图片内容无效或格式不支持（仅 jpeg/png/webp/gif）")
		return
	}
	if err != nil {
		if errors.Is(err, mediaapp.ErrMediaCapacity) {
			response.Error(c, http.StatusInsufficientStorage, "mediaCapacityExceeded", "媒体临时存储容量不足")
			return
		}
		response.Error(c, http.StatusInternalServerError, "mediaSaveImageFailed", "保存图片失败")
		return
	}
	h.writeInputAsset(c, asset)
}

func (h *Handler) writeInputAsset(c *gin.Context, asset mediadomain.Asset) {
	expiresAt := ""
	if asset.ExpiresAt != nil {
		expiresAt = asset.ExpiresAt.Format(time.RFC3339)
	}
	response.Success(c, http.StatusCreated, gin.H{
		"fileId": asset.ID, "kind": asset.Kind, "mimeType": asset.MIMEType, "sizeBytes": asset.SizeBytes, "expiresAt": expiresAt,
	})
}

func (h *Handler) acquireIngest(c *gin.Context) bool {
	select {
	case h.ingestSlots <- struct{}{}:
		return true
	default:
		response.Error(c, http.StatusServiceUnavailable, "mediaIngestBusy", "媒体暂存并发已满，请稍后重试")
		return false
	}
}

func (h *Handler) releaseIngest() { <-h.ingestSlots }
