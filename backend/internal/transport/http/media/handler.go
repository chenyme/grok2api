package media

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service       *mediaapp.Service
	previewTokens videoPreviewTokenService
}

type videoPreviewTokenService interface {
	CreateVideoPreviewToken(jobID string, ttl time.Duration) (string, error)
	ParseVideoPreviewToken(raw, jobID string) error
}

const videoPreviewTokenTTL = 5 * time.Minute

func NewHandler(service *mediaapp.Service, previewTokens videoPreviewTokenService) *Handler {
	return &Handler{service: service, previewTokens: previewTokens}
}

// RegisterPublic 注册使用不可猜测资源 ID 的公开图片读取与视频上传接收端点。
// 上传 PUT 不使用客户端 API key：xAI 无法携带，票据本身即授权。
func (h *Handler) RegisterPublic(router *gin.Engine) {
	router.GET("/v1/media/images/:assetId", h.getImage)
	router.HEAD("/v1/media/images/:assetId", h.getImage)
	router.PUT("/v1/media/uploads/:token", h.putVideoUpload)
	router.GET("/v1/media/video-previews/:jobId", h.streamVideoPreview)
	router.HEAD("/v1/media/video-previews/:jobId", h.streamVideoPreview)
}

// RegisterAdmin 注册管理端媒体列表和统计端点。
func (h *Handler) RegisterAdmin(router *gin.RouterGroup) {
	router.GET("/media/images", h.listImages)
	router.GET("/media/images/stats", h.imageStats)
	router.GET("/media/videos", h.listVideos)
	router.GET("/media/videos/stats", h.videoStats)
	router.POST("/media/videos/:jobId/preview", h.issueVideoPreview)
}

func (h *Handler) getImage(c *gin.Context) {
	asset, body, err := h.service.OpenImage(c.Request.Context(), c.Param("assetId"))
	if errors.Is(err, mediaapp.ErrAssetNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer body.Close()
	etag := `"` + asset.SHA256 + `"`
	if strings.TrimSpace(c.GetHeader("If-None-Match")) == etag {
		c.Header("ETag", etag)
		c.Status(http.StatusNotModified)
		return
	}
	c.Header("Content-Type", asset.MIMEType)
	c.Header("Content-Length", strconv.FormatInt(asset.SizeBytes, 10))
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("ETag", etag)
	c.Header("X-Content-Type-Options", "nosniff")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, body)
}

// putVideoUpload 接收 XAI ZDR 视频 PUT。响应与错误不得回显完整票据。
func (h *Handler) putVideoUpload(c *gin.Context) {
	_, err := h.service.ReceiveVideoUpload(c.Request.Context(), c.Param("token"), c.GetHeader("Content-Type"), c.Request.Body)
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, mediaapp.ErrUploadTicketNotFound):
		c.Status(http.StatusNotFound)
	case errors.Is(err, mediaapp.ErrUploadTicketExpired):
		c.Status(http.StatusGone)
	case errors.Is(err, mediaapp.ErrUploadTicketConsumed):
		c.Status(http.StatusConflict)
	case errors.Is(err, mediaapp.ErrVideoUploadTooLarge):
		// 体积超限优先于通用无效上传，返回 413。
		c.Status(http.StatusRequestEntityTooLarge)
	case errors.Is(err, mediaapp.ErrInvalidVideoUpload):
		c.Status(http.StatusBadRequest)
	case errors.Is(err, mediaapp.ErrUploadTicketsUnavailable):
		c.Status(http.StatusServiceUnavailable)
	default:
		c.Status(http.StatusInternalServerError)
	}
}

func (h *Handler) listImages(c *gin.Context) {
	page, pageSize := parsePagination(c)
	assets, total, err := h.service.AdminListImages(c.Request.Context(), page, pageSize, c.Query("search"))
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaListImagesFailed", "读取图片列表失败")
		return
	}
	items := make([]mediaAssetDTO, 0, len(assets))
	for _, a := range assets {
		items = append(items, mediaAssetDTO{
			ID: a.ID, Kind: a.Kind, MimeType: a.MIMEType, SizeBytes: a.SizeBytes,
			SHA256: a.SHA256, CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z"),
			URL: h.service.PublicImageURL(a.ID),
		})
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) imageStats(c *gin.Context) {
	stats, err := h.service.AdminImageStats(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaImageStatsFailed", "读取图片统计失败")
		return
	}
	response.Success(c, http.StatusOK, imageStatsDTO{TotalImages: stats.TotalImages, TotalBytes: stats.TotalBytes})
}

func (h *Handler) listVideos(c *gin.Context) {
	page, pageSize := parsePagination(c)
	jobs, total, err := h.service.AdminListVideoJobs(c.Request.Context(), page, pageSize, c.Query("search"), c.Query("status"), repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))})
	if errors.Is(err, mediaapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaListVideosFailed", "读取视频任务列表失败")
		return
	}
	items := make([]mediaJobDTO, 0, len(jobs))
	for _, item := range jobs {
		j := item.Job
		var completedAt *string
		if j.CompletedAt != nil {
			formatted := j.CompletedAt.UTC().Format(time.RFC3339)
			completedAt = &formatted
		}
		// 仅 failed 输出错误文案，遮蔽 completed 历史脏数据。
		errorMessage := ""
		if j.Status == mediadomain.StatusFailed {
			errorMessage = j.ErrorMessage
		}
		items = append(items, mediaJobDTO{
			ID: j.ID, Model: j.Model, Prompt: j.Prompt, Status: string(j.Status),
			Progress: j.Progress, Seconds: j.Seconds, Size: j.Size, Quality: j.Quality,
			AccountName: j.AccountName, ClientKeyName: j.ClientKeyName,
			CreatedAt: j.CreatedAt.UTC().Format(time.RFC3339), CompletedAt: completedAt,
			ErrorMessage: errorMessage, PreviewAvailable: item.PreviewAvailable,
		})
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

// issueVideoPreview 为已认证管理员签发仅绑定当前任务的短时流媒体票据。
func (h *Handler) issueVideoPreview(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if h.previewTokens == nil {
		response.Error(c, http.StatusServiceUnavailable, "mediaVideoPreviewUnavailable", "视频预览服务未配置")
		return
	}
	jobID := strings.TrimSpace(c.Param("jobId"))
	_, body, err := h.service.AdminOpenVideoJobContent(c.Request.Context(), jobID)
	if errors.Is(err, mediaapp.ErrAssetNotFound) {
		response.Error(c, http.StatusNotFound, "mediaVideoPreviewUnavailable", "本地视频缓存不可用或已清理")
		return
	}
	if errors.Is(err, mediaapp.ErrMediaJobsUnavailable) {
		response.Error(c, http.StatusServiceUnavailable, "mediaVideoPreviewUnavailable", "视频任务仓储未配置")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaVideoPreviewFailed", "读取本地视频缓存失败")
		return
	}
	_ = body.Close()
	ticket, err := h.previewTokens.CreateVideoPreviewToken(jobID, videoPreviewTokenTTL)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaVideoPreviewFailed", "创建视频预览票据失败")
		return
	}
	path := "/v1/media/video-previews/" + url.PathEscape(jobID) + "?ticket=" + url.QueryEscape(ticket)
	response.Success(c, http.StatusOK, videoPreviewDTO{URL: path})
}

// streamVideoPreview 验证短时票据后交由 ServeContent 处理 Range、HEAD 和条件请求。
func (h *Handler) streamVideoPreview(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Disposition", "inline")
	jobID := strings.TrimSpace(c.Param("jobId"))
	if h.previewTokens == nil || h.previewTokens.ParseVideoPreviewToken(strings.TrimSpace(c.Query("ticket")), jobID) != nil {
		c.Status(http.StatusUnauthorized)
		return
	}
	asset, body, err := h.service.AdminOpenVideoJobContent(c.Request.Context(), jobID)
	if errors.Is(err, mediaapp.ErrAssetNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	if errors.Is(err, mediaapp.ErrMediaJobsUnavailable) {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer body.Close()
	content, ok := body.(io.ReadSeeker)
	if !ok {
		c.Status(http.StatusInternalServerError)
		return
	}
	mimeType := strings.TrimSpace(asset.MIMEType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	c.Header("Content-Type", mimeType)
	http.ServeContent(c.Writer, c.Request, asset.ID, asset.CreatedAt, content)
}

func (h *Handler) videoStats(c *gin.Context) {
	stats, err := h.service.AdminVideoStats(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaVideoStatsFailed", "读取视频统计失败")
		return
	}
	response.Success(c, http.StatusOK, videoStatsDTO{
		TotalJobs: stats.TotalJobs, Completed: stats.Completed, Failed: stats.Failed,
		InProgress: stats.InProgress, Queued: stats.Queued,
	})
}

func parsePagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
