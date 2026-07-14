package dashboard

// Resources 表示 Dashboard 所需的资源总量与可用量。
type Resources struct {
	ActiveAccounts   int64
	TotalAccounts    int64
	EnabledModels    int64
	TotalModels      int64
	ActiveClientKeys int64
	TotalClientKeys  int64
	AllTimeRequests  int64
}

// Usage 表示指定时间窗口内的请求聚合。
type Usage struct {
	Requests           int64
	SuccessfulRequests int64
	FailedRequests     int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
}

// Bucket 表示一个固定时间桶内的请求和 token 数量。
type Bucket struct {
	Index              int
	Requests           int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
}

// ModelUsage 表示指定时间范围内按公开模型聚合的调用量。
type ModelUsage struct {
	Model              string
	Requests           int64
	InputTokens        int64
	CachedInputTokens  int64
	OutputTokens       int64
	ReasoningTokens    int64
	Tokens             int64
	BilledCostUSDTicks int64
}

// ModelBucket 表示单个时间桶内某个模型的用量。
type ModelBucket struct {
	Index              int
	Model              string
	Tokens             int64
	BilledCostUSDTicks int64
}

// Aggregate 表示持久化层返回的 Dashboard 聚合快照。
type Aggregate struct {
	Resources    Resources
	Usage        Usage
	Buckets      []Bucket
	TopModels    []ModelUsage
	ModelBuckets []ModelBucket
}
