package media

import (
	"context"
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
	"github.com/chenyme/grok2api/backend/internal/pkg/netguard"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

const (
	// 20 MiB 为各 Provider 的共同安全输入上限，同时为全局 32 MiB multipart 请求上限保留编码开销。
	ingestMaxImageBytes = 20 << 20
	// ingestFetchTimeout 是 URL 导入单次抓取的整体超时。
	ingestFetchTimeout = 20 * time.Second
	// 独立 bulkhead 防止临时导入与推理流量争抢内存和连接。
	ingestConcurrency = 4
)

var (
	// errImageTooLarge 表示下载/上传的图片超过读取上限。
	errImageTooLarge = errors.New("图片超过大小上限")
	// errFetchBlocked 表示目标地址被 SSRF 防护拒绝（内网/环回/链路本地/元数据等）。
	errFetchBlocked = errors.New("目标地址不允许访问")
)

// ingestHTTPClient 用于把远程图片导入隐藏的临时视频输入区。它带 SSRF 防护拨号器，
// 在建立连接前校验目标 IP，拒绝内网/环回/链路本地/元数据地址（每次拨号都校验，覆盖重定向与 DNS rebinding）。
// 不使用任何出网代理，直接抓取用户提供的公网图片 URL。
var ingestHTTPClient = &http.Client{
	Timeout: ingestFetchTimeout,
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 10 * time.Second,
			Control:   ssrfSafeControl,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          8,
		MaxConnsPerHost:       ingestConcurrency,
		IdleConnTimeout:       30 * time.Second,
	},
	// 最多 5 次重定向；重定向产生的新连接同样经过 ssrfSafeControl 校验。
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("重定向次数过多")
		}
		if err := validateImportURL(req.URL); err != nil {
			return err
		}
		return nil
	},
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

func validateImportURL(parsed *url.URL) error {
	if parsed == nil || (!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("图片 URL 无效，仅支持无凭据的 http/https 地址")
	}
	if port := parsed.Port(); port != "" && port != "80" && port != "443" {
		return errors.New("图片 URL 仅允许 80 或 443 端口")
	}
	return nil
}

// fetchRemoteImage 抓取远端图片字节，读取上限 ingestMaxImageBytes，超限返回 errImageTooLarge。
// 目标地址被 SSRF 防护拒绝时返回 errFetchBlocked。
func fetchRemoteImage(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "grok2api-media-importer/1.0")

	resp, err := ingestHTTPClient.Do(req)
	if err != nil {
		if errors.Is(err, errFetchBlocked) {
			return nil, errFetchBlocked
		}
		return nil, err
	}
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
	if err != nil || validateImportURL(parsed) != nil {
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

// uploadInputImage 接收管理员上传的本地图片文件（multipart 字段名 file），登记到临时输入区。
func (h *Handler) uploadInputImage(c *gin.Context) {
	if !h.acquireIngest(c) {
		return
	}
	defer h.releaseIngest()
	fileHeader, err := c.FormFile("file")
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			response.Error(c, http.StatusRequestEntityTooLarge, "imageTooLarge", "图片超过请求大小上限")
			return
		}
		response.Error(c, http.StatusBadRequest, "invalidRequest", "缺少上传文件")
		return
	}
	if fileHeader.Size > ingestMaxImageBytes {
		response.Error(c, http.StatusRequestEntityTooLarge, "imageTooLarge", "图片超过大小上限")
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
		response.Error(c, http.StatusRequestEntityTooLarge, "imageTooLarge", "图片超过大小上限")
		return
	}
	h.saveIngestedImage(c, data)
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
	expiresAt := ""
	if asset.ExpiresAt != nil {
		expiresAt = asset.ExpiresAt.Format(time.RFC3339)
	}
	response.Success(c, http.StatusCreated, gin.H{
		"fileId": asset.ID, "mimeType": asset.MIMEType, "sizeBytes": asset.SizeBytes, "expiresAt": expiresAt,
	})
}

func (h *Handler) acquireIngest(c *gin.Context) bool {
	select {
	case h.ingestSlots <- struct{}{}:
		return true
	default:
		response.Error(c, http.StatusServiceUnavailable, "mediaIngestBusy", "图片暂存并发已满，请稍后重试")
		return false
	}
}

func (h *Handler) releaseIngest() { <-h.ingestSlots }
