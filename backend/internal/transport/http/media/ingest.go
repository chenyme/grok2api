package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

const (
	// ingestMaxImageBytes 是 URL 导入/本地上传读取图片的读取兜底上限（32 MiB，与图片存储硬上限一致）。
	// Service.SaveImage 会再次按运行期配置(maxImageBytes)校验，这里只防止无界读取。
	ingestMaxImageBytes = 32 << 20
	// ingestFetchTimeout 是 URL 导入单次抓取的整体超时。
	ingestFetchTimeout = 20 * time.Second
)

var (
	// errImageTooLarge 表示下载/上传的图片超过读取上限。
	errImageTooLarge = errors.New("图片超过大小上限")
	// errFetchBlocked 表示目标地址被 SSRF 防护拒绝（内网/环回/链路本地/元数据等）。
	errFetchBlocked = errors.New("目标地址不允许访问")
)

// ingestHTTPClient 用于"从 URL 导入图片到图库"。它带 SSRF 防护拨号器，
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
		IdleConnTimeout:       30 * time.Second,
	},
	// 最多 5 次重定向；重定向产生的新连接同样经过 ssrfSafeControl 校验。
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("重定向次数过多")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("不支持的重定向协议 %q", req.URL.Scheme)
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
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("目标地址不是有效 IP %q: %w", host, errFetchBlocked)
	}
	if !isPublicIP(ip) {
		return fmt.Errorf("拒绝访问非公网地址 %s: %w", host, errFetchBlocked)
	}
	return nil
}

// isPublicIP 仅当 IP 是可路由公网地址时返回 true。
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		// 169.254.0.0/16 云元数据（link-local 已覆盖，这里兜底）
		if v4[0] == 169 && v4[1] == 254 {
			return false
		}
		// 100.64.0.0/10 运营商级 NAT
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return false
		}
	}
	return true
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
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
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
	URL string `json:"url" binding:"required"`
}

// ingestImageFromURL 从管理员提供的 URL 抓取图片并登记到图库，返回稳定的 /v1/media/images/{id} 地址。
// 路由在 /api/admin/v1 下，已由 AdminAuth 保护；抓取带 SSRF 防护。
func (h *Handler) ingestImageFromURL(c *gin.Context) {
	var request importImageRequest
	if c.ShouldBindJSON(&request) != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效")
		return
	}
	rawURL := strings.TrimSpace(request.URL)
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		response.Error(c, http.StatusBadRequest, "invalidImageURL", "图片 URL 无效，仅支持 http/https")
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

// uploadImage 接收管理员上传的本地图片文件（multipart 字段名 file），登记到图库。
func (h *Handler) uploadImage(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
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

// saveIngestedImage 收口两种入图路径：校验+落盘+登记 media_assets，返回资产 DTO。
func (h *Handler) saveIngestedImage(c *gin.Context, data []byte) {
	asset, err := h.service.SaveImage(c.Request.Context(), data)
	if errors.Is(err, mediaapp.ErrInvalidImage) {
		response.Error(c, http.StatusBadRequest, "invalidImage", "图片内容无效或格式不支持（仅 jpeg/png/webp/gif）")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaSaveImageFailed", "保存图片失败")
		return
	}
	h.respondAsset(c, asset)
}

// respondAsset 以与列表接口一致的 DTO 结构返回单个资产（含公开 URL）。
func (h *Handler) respondAsset(c *gin.Context, asset mediadomain.Asset) {
	response.Success(c, http.StatusCreated, mediaAssetDTO{
		ID: asset.ID, Kind: asset.Kind, MimeType: asset.MIMEType, SizeBytes: asset.SizeBytes,
		SHA256: asset.SHA256, CreatedAt: asset.CreatedAt.Format("2006-01-02T15:04:05Z"),
		URL: h.service.PublicImageURL(asset.ID),
	})
}
