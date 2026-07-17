package media

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	// DefaultMaxVideoBytes 是视频上传与本地视频资产的安全上限（256 MiB）。
	DefaultMaxVideoBytes = 256 << 20
	// videoUploadTicketTTL 限制票据有效期，过期后不可再写入。
	videoUploadTicketTTL = 2 * time.Hour
	// videoUploadWaitInterval 轮询本地资产就绪间隔。
	videoUploadWaitInterval = 500 * time.Millisecond
)

var (
	ErrInvalidVideoUpload = errors.New("视频上传无效")
	// ErrVideoUploadTooLarge 表示 body 超过票据/中间件体积上限；handler 映射为 HTTP 413。
	// 同时包装 ErrInvalidVideoUpload，便于统一归类为无效上传。
	ErrVideoUploadTooLarge      = errors.New("视频超过体积上限")
	ErrUploadTicketNotFound     = errors.New("上传票据不存在")
	ErrUploadTicketExpired      = errors.New("上传票据已过期")
	ErrUploadTicketConsumed     = errors.New("上传票据已使用")
	ErrUploadPublicBase         = errors.New("公开 API 地址不可用于 XAI 视频上传")
	ErrVideoUploadIncomplete    = errors.New("视频尚未上传完成")
	ErrUploadTicketsUnavailable = errors.New("视频上传票据仓储未配置")
)

func errVideoTooLarge() error {
	return fmt.Errorf("%w: %w", ErrInvalidVideoUpload, ErrVideoUploadTooLarge)
}

type stagedVideo struct {
	ID         string
	TempPath   string
	StorageKey string
	MIMEType   string
	SizeBytes  int64
	SHA256     string
}

// IssueVideoUpload 签发一次性高熵 PUT 地址，供 XAI ZDR 视频写入。
// 错误信息不得包含完整 URL 或 token 明文。
func (s *Service) IssueVideoUpload(ctx context.Context, jobID string) (uploadURL, assetID string, err error) {
	if s.tickets == nil {
		return "", "", ErrUploadTicketsUnavailable
	}
	publicBase, err := s.httpsPublicBaseURL()
	if err != nil {
		return "", "", err
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return "", "", fmt.Errorf("%w: 缺少视频任务 ID", ErrInvalidVideoUpload)
	}
	token, err := newUploadToken()
	if err != nil {
		return "", "", err
	}
	assetID, err = newVideoAssetID()
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	ticket := repository.MediaUploadTicket{
		TokenHash:   hashUploadToken(token),
		AssetID:     assetID,
		JobID:       jobID,
		MaxBytes:    DefaultMaxVideoBytes,
		AllowedMIME: "video/mp4",
		ExpiresAt:   now.Add(videoUploadTicketTTL),
		CreatedAt:   now,
	}
	if err := s.tickets.CreateUploadTicket(ctx, ticket); err != nil {
		return "", "", err
	}
	// 提前绑定任务结果资产，便于轮询/清理感知进行中的本地目标。
	// 生产中 job 已存在：真实数据库错误必须中止签发，不得返回未绑定目标的 upload URL。
	// ErrNotFound 仅保留给隔离/测试占位 job 的既有行为，且不删除已创建票据。
	if err := s.tickets.BindJobResultAsset(ctx, jobID, assetID); err != nil && !errors.Is(err, repository.ErrNotFound) {
		// 补偿删除刚创建的票据，避免 bind 失败后留下不可达行直至 TTL。
		if delErr := s.tickets.DeleteUploadTicketByHash(ctx, ticket.TokenHash); delErr != nil {
			return "", "", fmt.Errorf("绑定视频任务结果资产失败: %w", errors.Join(err, fmt.Errorf("回滚上传票据失败: %w", delErr)))
		}
		return "", "", fmt.Errorf("绑定视频任务结果资产失败: %w", err)
	}
	uploadURL = publicBase + "/v1/media/uploads/" + token
	return uploadURL, assetID, nil
}

// WaitVideoUpload 等待 PUT 完成后资产元数据就绪。
func (s *Service) WaitVideoUpload(ctx context.Context, assetID string) (contentType string, err error) {
	assetID = strings.TrimSpace(assetID)
	if assetID == "" {
		return "", fmt.Errorf("%w: 缺少资产 ID", ErrInvalidVideoUpload)
	}
	ticker := time.NewTicker(videoUploadWaitInterval)
	defer ticker.Stop()
	for {
		asset, getErr := s.assets.GetMediaAsset(ctx, assetID)
		if getErr == nil && asset.Kind == "video" && asset.SizeBytes > 0 {
			return asset.MIMEType, nil
		}
		if getErr != nil && !errors.Is(getErr, repository.ErrNotFound) {
			return "", getErr
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", ErrVideoUploadIncomplete
			}
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

// ReceiveVideoUpload 处理公开 PUT：校验票据、流式限长、内容嗅探、原子提交并登记资产。
// 票据本身即授权，无需客户端 API key。
func (s *Service) ReceiveVideoUpload(ctx context.Context, rawToken string, contentType string, body io.Reader) (mediadomain.Asset, error) {
	if s.tickets == nil {
		return mediadomain.Asset{}, ErrUploadTicketsUnavailable
	}
	token := strings.TrimSpace(rawToken)
	if !validUploadToken(token) {
		return mediadomain.Asset{}, ErrUploadTicketNotFound
	}
	tokenHash := hashUploadToken(token)
	now := time.Now().UTC()
	// 先读取票据以做预检，再原子消费，防止重复成功写入。
	existing, err := s.tickets.GetUploadTicketByHash(ctx, tokenHash)
	if errors.Is(err, repository.ErrNotFound) {
		return mediadomain.Asset{}, ErrUploadTicketNotFound
	}
	if err != nil {
		return mediadomain.Asset{}, err
	}
	if existing.ConsumedAt != nil {
		return mediadomain.Asset{}, ErrUploadTicketConsumed
	}
	if !existing.ExpiresAt.After(now) {
		return mediadomain.Asset{}, ErrUploadTicketExpired
	}
	// 若资产已存在则视为重复成功写入，拒绝覆盖；仓储故障必须向上传播，禁止继续写入或消费票据。
	if _, getErr := s.assets.GetMediaAsset(ctx, existing.AssetID); getErr == nil {
		return mediadomain.Asset{}, ErrUploadTicketConsumed
	} else if getErr != nil && !errors.Is(getErr, repository.ErrNotFound) {
		return mediadomain.Asset{}, getErr
	}

	// XAI ZDR 当前契约为 MP4：票据 AllowedMIME 约束声明类型与嗅探结果。
	allowedMIME := normalizeVideoMIME(existing.AllowedMIME)
	if allowedMIME == "" {
		allowedMIME = "video/mp4"
	}
	if !supportedVideoMIME(allowedMIME) {
		return mediadomain.Asset{}, fmt.Errorf("%w: 票据 MIME 无效", ErrInvalidVideoUpload)
	}
	declaredMIME := normalizeVideoMIME(contentType)
	if declaredMIME != "" && declaredMIME != allowedMIME {
		return mediadomain.Asset{}, fmt.Errorf("%w: Content-Type 与票据不允许的类型不一致", ErrInvalidVideoUpload)
	}

	staged, err := s.stageVideo(ctx, existing.AssetID, allowedMIME, body, existing.MaxBytes)
	if err != nil {
		return mediadomain.Asset{}, err
	}
	defer func() { _ = s.objects.AbortVideoUpload(context.WithoutCancel(ctx), staged.TempPath) }()

	// 原子消费票据：并发 PUT 仅一个能消费成功。
	ticket, consumed, err := s.tickets.ConsumeUploadTicket(ctx, tokenHash, now)
	if err != nil {
		return mediadomain.Asset{}, err
	}
	if !consumed {
		return mediadomain.Asset{}, ErrUploadTicketConsumed
	}
	// 消费后若提交/登记失败，必须释放票据以便同 token 重试，避免永久烧毁。
	releaseTicket := true
	defer func() {
		if releaseTicket {
			_, _ = s.tickets.ReleaseUploadTicket(context.WithoutCancel(ctx), tokenHash)
		}
	}()

	asset, err := s.commitStagedVideo(ctx, staged, now)
	if err != nil {
		return mediadomain.Asset{}, err
	}
	releaseTicket = false
	_ = s.tickets.BindJobResultAsset(ctx, ticket.JobID, ticket.AssetID)
	return asset, nil
}

// SaveVideo 将 Provider 已生成的远程视频流式归档为本地媒体资产。
func (s *Service) SaveVideo(ctx context.Context, jobID, contentType string, body io.Reader) (mediadomain.Asset, error) {
	mimeType := normalizeVideoMIME(contentType)
	if mimeType == "" {
		mimeType = "video/mp4"
	}
	if !supportedVideoMIME(mimeType) {
		return mediadomain.Asset{}, fmt.Errorf("%w: Content-Type 无效", ErrInvalidVideoUpload)
	}
	id, err := newVideoAssetID()
	if err != nil {
		return mediadomain.Asset{}, err
	}
	jobID = strings.TrimSpace(jobID)
	bound := false
	if s.tickets != nil && jobID != "" {
		bindErr := s.tickets.BindJobResultAsset(ctx, jobID, id)
		if bindErr != nil && !errors.Is(bindErr, repository.ErrNotFound) {
			return mediadomain.Asset{}, fmt.Errorf("绑定视频任务结果资产失败: %w", bindErr)
		}
		bound = bindErr == nil
	}
	saved := false
	defer func() {
		if bound && !saved {
			_ = s.tickets.BindJobResultAsset(context.WithoutCancel(ctx), jobID, "")
		}
	}()
	staged, err := s.stageVideo(ctx, id, mimeType, body, DefaultMaxVideoBytes)
	if err != nil {
		return mediadomain.Asset{}, err
	}
	defer func() { _ = s.objects.AbortVideoUpload(context.WithoutCancel(ctx), staged.TempPath) }()
	asset, err := s.commitStagedVideo(ctx, staged, time.Now().UTC())
	if err == nil {
		saved = true
	}
	return asset, err
}

func (s *Service) stageVideo(ctx context.Context, id, mimeType string, body io.Reader, maxBytes int64) (stagedVideo, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxVideoBytes
	}
	tempPath, storageKey, err := s.objects.BeginVideoUpload(ctx, id, mimeType)
	if err != nil {
		return stagedVideo{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = s.objects.AbortVideoUpload(context.WithoutCancel(ctx), tempPath)
		}
	}()
	file, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return stagedVideo{}, err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(body, maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		if isBodyTooLargeError(copyErr) {
			return stagedVideo{}, errVideoTooLarge()
		}
		return stagedVideo{}, copyErr
	}
	if closeErr != nil {
		return stagedVideo{}, closeErr
	}
	if written == 0 {
		return stagedVideo{}, fmt.Errorf("%w: 空内容", ErrInvalidVideoUpload)
	}
	if written > maxBytes {
		return stagedVideo{}, errVideoTooLarge()
	}
	sniffed, err := sniffVideoFile(tempPath)
	if err != nil {
		return stagedVideo{}, err
	}
	if sniffed != mimeType {
		return stagedVideo{}, fmt.Errorf("%w: 内容类型与声明类型不一致", ErrInvalidVideoUpload)
	}
	cleanup = false
	return stagedVideo{ID: id, TempPath: tempPath, StorageKey: storageKey, MIMEType: mimeType, SizeBytes: written, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func (s *Service) commitStagedVideo(ctx context.Context, staged stagedVideo, createdAt time.Time) (mediadomain.Asset, error) {
	if err := s.objects.CommitVideoUpload(ctx, staged.TempPath, staged.StorageKey); err != nil {
		return mediadomain.Asset{}, err
	}
	asset := mediadomain.Asset{
		ID: staged.ID, Kind: "video", StorageKey: staged.StorageKey, MIMEType: staged.MIMEType,
		SizeBytes: staged.SizeBytes, SHA256: staged.SHA256, CreatedAt: createdAt,
	}
	if err := s.assets.CreateMediaAsset(ctx, asset); err != nil {
		_ = s.objects.Delete(context.WithoutCancel(ctx), staged.StorageKey)
		return mediadomain.Asset{}, err
	}
	if s.totalBytes.Add(asset.SizeBytes) > cleanupThresholdBytes(s.runtimeConfig()) {
		select {
		case s.cleanupSignal <- struct{}{}:
		default:
		}
	}
	return asset, nil
}

// OpenVideo 读取视频元数据与正文。
func (s *Service) OpenVideo(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error) {
	asset, err := s.assets.GetMediaAsset(ctx, strings.TrimSpace(id))
	if errors.Is(err, repository.ErrNotFound) {
		return mediadomain.Asset{}, nil, ErrAssetNotFound
	}
	if err != nil {
		return mediadomain.Asset{}, nil, err
	}
	if asset.Kind != "video" {
		return mediadomain.Asset{}, nil, ErrAssetNotFound
	}
	body, err := s.objects.Open(ctx, asset.StorageKey)
	if errors.Is(err, os.ErrNotExist) {
		return mediadomain.Asset{}, nil, ErrAssetNotFound
	}
	if err != nil {
		return mediadomain.Asset{}, nil, err
	}
	return asset, body, nil
}

func (s *Service) httpsPublicBaseURL() (string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.runtimeConfig().PublicBaseURL), "/")
	if base == "" {
		return "", ErrUploadPublicBase
	}
	parsed, err := url.ParseRequestURI(base)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrUploadPublicBase
	}
	return base, nil
}

func newUploadToken() (string, error) {
	// 256 bit 熵，hex 编码；校验时再 SHA-256 存库。
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成上传票据: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func hashUploadToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validUploadToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for _, c := range token {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func newVideoAssetID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成视频资源 ID: %w", err)
	}
	return "vid_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func supportedVideoMIME(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "video/mp4", "video/webm", "video/quicktime":
		return true
	default:
		return false
	}
}

func normalizeVideoMIME(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	if value == "application/octet-stream" || value == "" {
		return ""
	}
	return value
}

func sniffVideoFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 512)
	n, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", err
	}
	if n == 0 {
		return "", fmt.Errorf("%w: 空内容", ErrInvalidVideoUpload)
	}
	detected := http.DetectContentType(header[:n])
	// DetectContentType 对 mp4 通常返回 video/mp4；部分样本可能是 application/octet-stream。
	if supportedVideoMIME(detected) {
		return detected, nil
	}
	if looksLikeMP4(header[:n]) {
		return "video/mp4", nil
	}
	if looksLikeWebM(header[:n]) {
		return "video/webm", nil
	}
	return "", fmt.Errorf("%w: 非视频内容", ErrInvalidVideoUpload)
}

func looksLikeMP4(header []byte) bool {
	if len(header) < 12 {
		return false
	}
	// ISO BMFF: bytes 4-8 often "ftyp"
	return string(header[4:8]) == "ftyp"
}

func looksLikeWebM(header []byte) bool {
	// EBML header 0x1A45DFA3
	return len(header) >= 4 && header[0] == 0x1A && header[1] == 0x45 && header[2] == 0xDF && header[3] == 0xA3
}

func isBodyTooLargeError(err error) bool {
	if err == nil {
		return false
	}
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return true
	}
	// 兼容部分包装后的 MaxBytesReader 文案。
	msg := err.Error()
	return strings.Contains(msg, "http: request body too large") || strings.Contains(msg, "request body too large")
}
