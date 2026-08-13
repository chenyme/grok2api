package media

import (
	"encoding/base64"
	"strings"
	"time"
)

const (
	// InputAssetIDPrefix 标识不会公开展示、带生命周期的视频任务输入资产。
	InputAssetIDPrefix = "input_"
	// InputReferencePrefix 标识 media_jobs.input_json 中的本地临时输入引用。
	InputReferencePrefix = "grok2api-input:"
	inputAssetIDBytes    = 24
)

// IsInputAssetID 校验临时输入 ID 的命名空间和 192-bit 随机标识格式。
func IsInputAssetID(fileID string) bool {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(fileID), InputAssetIDPrefix)
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && len(raw) == inputAssetIDBytes
}

// InputReference 将临时资产 ID 编码为只在服务端内部解释的任务引用。
func InputReference(fileID string) string {
	return InputReferencePrefix + strings.TrimSpace(fileID)
}

// ParseInputReference 解析服务端内部临时输入引用。
func ParseInputReference(reference string) (string, bool) {
	fileID, ok := strings.CutPrefix(reference, InputReferencePrefix)
	fileID = strings.TrimSpace(fileID)
	return fileID, ok && IsInputAssetID(fileID)
}

// Asset 表示已归档到本地媒体存储的不可变资源。
type Asset struct {
	ID         string
	Kind       string
	StorageKey string
	MIMEType   string
	SizeBytes  int64
	SHA256     string
	// ExpiresAt 仅用于临时输入；nil 表示持久图库/视频资产。
	ExpiresAt *time.Time
	CreatedAt time.Time
}
