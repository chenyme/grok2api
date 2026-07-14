package audit

import "time"

type Operation string

const (
	OperationResponses Operation = "responses"
	OperationChat      Operation = "chat"
	OperationMessages  Operation = "messages"
	OperationImage     Operation = "image"
	OperationImageEdit Operation = "image_edit"
	OperationVideo     Operation = "video"
)

type UsageSource string

const (
	UsageSourceUpstream  UsageSource = "upstream"
	UsageSourceEstimated UsageSource = "estimated"
	UsageSourceNone      UsageSource = "none"
)

type AttemptSource string

const (
	AttemptSourceUpstreamHTTP AttemptSource = "upstream_http"
	AttemptSourceTransport    AttemptSource = "gateway_transport"
	AttemptSourceCredential   AttemptSource = "credential"
)

type ErrorFrame struct {
	Type    string
	Message string
}

// Attempt 保存一次失败尝试经过裁剪和脱敏的管理员诊断快照。
type Attempt struct {
	ID                    uint64
	AuditID               uint64
	Number                int
	Source                AttemptSource
	Stage                 string
	AccountID             *uint64
	AccountName           string
	Method                string
	RequestPath           string
	UpstreamURL           string
	StartedAt             time.Time
	DurationMS            int64
	UpstreamStatusCode    *int
	UpstreamStatus        string
	ResponseHeaders       map[string][]string
	ResponseBody          []byte
	ResponseBodyTruncated bool
	TransportError        string
	ErrorChain            []ErrorFrame
}

type EgressMode string

const (
	EgressModeDirect EgressMode = "direct"
	EgressModeProxy  EgressMode = "proxy"
)

// Record 表示推理请求审计；成功请求不保存正文，失败请求仅保留受限诊断快照。
type Record struct {
	ID                      uint64
	EventID                 string
	RequestID               string
	ClientKeyID             uint64
	ClientKeyName           string
	ModelRouteID            uint64
	ModelPublicID           string
	ModelUpstreamModel      string
	Provider                string
	Operation               Operation
	UsageSource             UsageSource
	AccountID               *uint64
	AccountName             string
	EgressNodeID            *uint64
	EgressNodeName          string
	EgressScope             string
	EgressMode              EgressMode
	StatusCode              int
	Streaming               bool
	MediaInputImages        int64
	MediaOutputImages       int64
	MediaOutputSeconds      int64
	InputTokens             int64
	CachedInputTokens       int64
	OutputTokens            int64
	ReasoningTokens         int64
	TotalTokens             int64
	CostInUSDTicks          int64
	EstimatedCostInUSDTicks int64
	PricingModel            string
	PricingVersion          string
	NumSourcesUsed          int64
	NumServerSideToolsUsed  int64
	ContextInputTokens      int64
	ContextOutputTokens     int64
	DurationMS              int64
	ErrorCode               string
	AttemptCount            int
	Attempts                []Attempt
	CreatedAt               time.Time
}

// Summary 表示指定审计范围内的聚合用量。
type Summary struct {
	Requests                int64
	SuccessfulRequests      int64
	FailedRequests          int64
	InputTokens             int64
	CachedInputTokens       int64
	OutputTokens            int64
	ReasoningTokens         int64
	TotalTokens             int64
	DurationMS              int64
	EstimatedCostInUSDTicks int64
	PricedRequests          int64
	UnpricedRequests        int64
	PricedTokens            int64
	UnpricedTokens          int64
}
