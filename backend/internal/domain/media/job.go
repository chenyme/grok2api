package media

import "time"

type Status string

const (
	StatusQueued     Status = "queued"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// MaxInputJSONBytes is the persisted media_jobs.input_json ceiling (32 MiB).
// Keep the relational CHECK constraint and gateway encode guard aligned with this value.
const MaxInputJSONBytes = 32 << 20

// MaxInputImages is the maximum number of reference images accepted for a video job.
const MaxInputImages = 8

// Job 表示可跨进程重启恢复的异步视频任务。
type Job struct {
	ID              string
	RequestID       string
	ClientKeyID     uint64
	ClientKeyName   string
	AccountID       uint64
	AccountName     string
	EgressNodeID    *uint64
	EgressNodeName  string
	EgressScope     string
	EgressMode      string
	Provider        string
	Model           string
	ModelRouteID    uint64
	UpstreamModel   string
	Prompt          string
	Seconds         int
	Size            string
	Quality         string
	Status          Status
	Progress        int
	InputJSON       string
	InputImageCount int
	UpstreamURL     string
	// ResultAssetID 指向本地媒体资产；XAI ZDR 上传完成后优先从此读取。
	ResultAssetID   string
	ContentType     string
	ErrorCode       string
	ErrorMessage    string
	LeaseUntil      *time.Time
	ClaimToken      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CompletedAt     *time.Time
	UsageRecordedAt *time.Time
}
