package media

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *mediaapp.Service
}

func NewHandler(service *mediaapp.Service) *Handler { return &Handler{service: service} }

// RegisterPublic 注册使用不可猜测资源 ID 的公开图片读取端点。
func (h *Handler) RegisterPublic(router *gin.Engine) {
	router.GET("/v1/media/images/:assetId", h.getImage)
	router.HEAD("/v1/media/images/:assetId", h.getImage)
}

// RegisterAdmin 注册管理端媒体列表和统计端点。
func (h *Handler) RegisterAdmin(router *gin.RouterGroup) {
	router.GET("/media/images", h.listImages)
	router.GET("/media/images/stats", h.imageStats)
	router.GET("/media/videos", h.listVideos)
	router.GET("/media/videos/stats", h.videoStats)
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
	for _, j := range jobs {
		var completedAt *string
		if j.CompletedAt != nil {
			formatted := j.CompletedAt.Format("2006-01-02T15:04:05Z")
			completedAt = &formatted
		}
		items = append(items, mediaJobDTO{
			ID: j.ID, Model: j.Model, Prompt: j.Prompt, Status: string(j.Status),
			Progress: j.Progress, Seconds: j.Seconds, Size: j.Size, Quality: j.Quality,
			AccountName: j.AccountName, ClientKeyName: j.ClientKeyName,
			CreatedAt:   j.CreatedAt.Format("2006-01-02T15:04:05Z"),
			CompletedAt: completedAt, ErrorMessage: j.ErrorMessage,
		})
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
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
