package media

import "time"

// Asset 表示已归档到本地媒体存储的不可变资源。
type Asset struct {
	ID         string
	Kind       string
	StorageKey string
	MIMEType   string
	SizeBytes  int64
	SHA256     string
	CreatedAt  time.Time
}
