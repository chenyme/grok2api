package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// LocalStore 将媒体对象限制在单一根目录内，并使用临时文件与原子硬链接完成提交。
type LocalStore struct {
	root            string
	removeTemporary func(string) error
}

func NewLocalStore(root string) (*LocalStore, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil || strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("媒体存储目录无效")
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("创建媒体存储目录: %w", err)
	}
	return &LocalStore{root: absolute, removeTemporary: os.Remove}, nil
}

func (s *LocalStore) SaveImage(ctx context.Context, id, mimeType string, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	extension, ok := imageExtension(mimeType)
	if !ok || len(id) < 2 {
		return "", fmt.Errorf("图片存储参数无效")
	}
	storageKey := filepath.ToSlash(filepath.Join("images", id[:2], id+extension))
	path, err := s.resolve(storageKey)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("创建图片目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".image-*")
	if err != nil {
		return "", fmt.Errorf("创建图片临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanupPending := true
	defer func() {
		_ = temporary.Close()
		if cleanupPending {
			if cleanupErr := s.removeTemporary(temporaryPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				slog.Warn("media_temp_cleanup_failed", "path", temporaryPath, "error", cleanupErr)
			}
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		return "", fmt.Errorf("写入图片: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("同步图片文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("关闭图片文件: %w", err)
	}
	// 硬链接提交具有 no-replace 语义，极端 ID 冲突时不会覆盖已有图片。
	if err := os.Link(temporaryPath, path); err != nil {
		return "", fmt.Errorf("提交图片文件: %w", err)
	}
	// 提交已经成功，清理失败不能回滚永久文件；defer 会再重试一次并记录持续失败。
	cleanupErr := s.removeTemporary(temporaryPath)
	cleanupPending = cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist)
	return storageKey, nil
}

func (s *LocalStore) Open(ctx context.Context, storageKey string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.resolve(storageKey)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("打开媒体文件: %w", err)
	}
	return file, nil
}

func (s *LocalStore) Delete(ctx context.Context, storageKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.resolve(storageKey)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("删除媒体文件: %w", err)
	}
	return nil
}

func (s *LocalStore) resolve(storageKey string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(storageKey)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("媒体存储路径无效")
	}
	full := filepath.Join(s.root, clean)
	relative, err := filepath.Rel(s.root, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("媒体存储路径越界")
	}
	return full, nil
}

func imageExtension(mimeType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg":
		return ".jpg", true
	case "image/png":
		return ".png", true
	case "image/webp":
		return ".webp", true
	case "image/gif":
		return ".gif", true
	default:
		return "", false
	}
}
