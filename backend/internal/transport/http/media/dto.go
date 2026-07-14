package media

type mediaAssetDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
}

type imageStatsDTO struct {
	TotalImages int64 `json:"totalImages"`
	TotalBytes  int64 `json:"totalBytes"`
}

type mediaJobDTO struct {
	ID            string  `json:"id"`
	Model         string  `json:"model"`
	Prompt        string  `json:"prompt"`
	Status        string  `json:"status"`
	Progress      int     `json:"progress"`
	Seconds       int     `json:"seconds"`
	Size          string  `json:"size"`
	Quality       string  `json:"quality"`
	AccountName   string  `json:"accountName"`
	ClientKeyName string  `json:"clientKeyName"`
	CreatedAt     string  `json:"createdAt"`
	CompletedAt   *string `json:"completedAt"`
	ErrorMessage  string  `json:"errorMessage"`
}

type videoStatsDTO struct {
	TotalJobs  int64 `json:"totalJobs"`
	Completed  int64 `json:"completed"`
	Failed     int64 `json:"failed"`
	InProgress int64 `json:"inProgress"`
	Queued     int64 `json:"queued"`
}
