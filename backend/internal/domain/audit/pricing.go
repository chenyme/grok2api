package audit

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	OfficialPricingSource             = "https://docs.x.ai/developers/pricing"
	OfficialPricingAsOf               = "2026-07-14"
	officialImageEditInputTicks int64 = 100_000_000
	officialLiteImageInputTicks int64 = 20_000_000
)

type PricingResult struct {
	Model          string
	CostInUSDTicks int64
}

// PricingBreakdown describes the exact rate components used to reconstruct a stored estimate.
type PricingBreakdown struct {
	Model          string
	CostInUSDTicks int64
	Tier           PricingTier
	Components     []PricingComponent
}

type PricingTier string

const (
	PricingTierStandard    PricingTier = "standard"
	PricingTierLongContext PricingTier = "long_context"
	PricingTierMedia       PricingTier = "media"
)

type PricingComponentKind string

const (
	PricingComponentUncachedInput PricingComponentKind = "uncached_input"
	PricingComponentCachedInput   PricingComponentKind = "cached_input"
	PricingComponentOutput        PricingComponentKind = "output"
	PricingComponentInputImage    PricingComponentKind = "input_image"
	PricingComponentOutputImage   PricingComponentKind = "output_image"
	PricingComponentOutputSecond  PricingComponentKind = "output_second"
)

type PricingUnit string

const (
	PricingUnitToken  PricingUnit = "token"
	PricingUnitImage  PricingUnit = "image"
	PricingUnitSecond PricingUnit = "second"
)

type PricingComponent struct {
	Kind                PricingComponentKind
	Unit                PricingUnit
	Quantity            int64
	UnitPriceInUSDTicks int64
	CostInUSDTicks      int64
}

type tokenPrice struct {
	CanonicalModel    string
	InputTicks        int64
	CachedInputTicks  int64
	OutputTicks       int64
	LongContextTokens int64
	LongInputTicks    int64
	LongCachedTicks   int64
	LongOutputTicks   int64
}

var officialTokenPrices = buildOfficialTokenPrices()

type tokenPriceRule struct {
	Pattern        *regexp.Regexp
	CanonicalModel string
}

var officialTokenPriceRules = []tokenPriceRule{
	{Pattern: regexp.MustCompile(`^grok-(?:build-0\.1|code-fast(?:-1)?|composer-2\.5-fast)(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-build-0.1"},
	{Pattern: regexp.MustCompile(`^grok-4\.5(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.5"},
	{Pattern: regexp.MustCompile(`^grok-4\.3(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.3"},
	{Pattern: regexp.MustCompile(`^grok-4\.20-multi-agent(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.20-multi-agent-0309"},
	{Pattern: regexp.MustCompile(`^grok-4\.20(?:-[a-z0-9.]+)*-non-reasoning(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.20-0309-non-reasoning"},
	{Pattern: regexp.MustCompile(`^grok-4\.20(?:-[a-z0-9.]+)*$`), CanonicalModel: "grok-4.20-0309-reasoning"},
}

// buildOfficialTokenPrices 使用 xAI 官方每 Token USD ticks 费率。
// 1 USD = 10,000,000,000 ticks；官方页面展示价格均为每 1M Tokens。
func buildOfficialTokenPrices() map[string]tokenPrice {
	prices := make(map[string]tokenPrice)
	register := func(canonical string, price tokenPrice, names ...string) {
		price.CanonicalModel = canonical
		for _, name := range append([]string{canonical}, names...) {
			prices[name] = price
		}
	}
	register("grok-build-0.1", tokenPrice{InputTicks: 10000, CachedInputTicks: 2000, OutputTicks: 20000, LongContextTokens: 200000, LongInputTicks: 20000, LongCachedTicks: 4000, LongOutputTicks: 40000},
		"grok-code-fast-1", "grok-code-fast", "grok-code-fast-1-0825", "grok-composer-2.5-fast")
	register("grok-4.5", tokenPrice{InputTicks: 20000, CachedInputTicks: 3000, OutputTicks: 60000, LongContextTokens: 200000, LongInputTicks: 40000, LongCachedTicks: 6000, LongOutputTicks: 120000},
		"grok-4.5-latest", "grok-build-latest")
	standard := tokenPrice{InputTicks: 12500, CachedInputTicks: 2000, OutputTicks: 25000, LongContextTokens: 200000, LongInputTicks: 25000, LongCachedTicks: 4000, LongOutputTicks: 50000}
	register("grok-4.3", standard, "grok-4.3-latest", "grok-latest")
	register("grok-4.20-multi-agent-0309", standard,
		"grok-4.20-multi-agent", "grok-4.20-multi-agent-latest", "grok-4.20-multi-agent-beta-latest", "grok-4.20-multi-agent-beta-0309")
	register("grok-4.20-0309-reasoning", standard,
		"grok-4.20-reasoning-latest", "grok-4.20", "grok-4.20-reasoning", "grok-4.20-0309", "grok-4.20-beta", "grok-4.20-beta-0309", "grok-4.20-beta-latest", "grok-4.20-beta-reasoning", "grok-4.20-beta-latest-reasoning")
	register("grok-4.20-0309-non-reasoning", standard,
		"grok-4.20-non-reasoning", "grok-4.20-non-reasoning-latest", "grok-4.20-beta-non-reasoning", "grok-4.20-beta-latest-non-reasoning")
	return prices
}

// resolveOfficialTokenPrice 先处理内部来源前缀和官方精确别名，再使用锚定规则识别同一模型家族的版本后缀。
func resolveOfficialTokenPrice(model string) (tokenPrice, bool) {
	normalized := normalizePricingModel(model)
	if price, ok := officialTokenPrices[normalized]; ok {
		return price, true
	}
	for _, rule := range officialTokenPriceRules {
		if !rule.Pattern.MatchString(normalized) {
			continue
		}
		price, ok := officialTokenPrices[rule.CanonicalModel]
		return price, ok
	}
	return tokenPrice{}, false
}

// normalizePricingModel 只移除系统已知的来源前缀，避免任意路径片段被误识别为可计费模型。
func normalizePricingModel(model string) string {
	normalized := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"build/", "web/", "console/", "grok_build/", "grok_web/", "grok_console/"} {
		if strings.HasPrefix(normalized, prefix) {
			return strings.TrimSpace(normalized[len(prefix):])
		}
	}
	return normalized
}

// EstimateOfficialCost 按官方模型价格计算单次请求成本；未知模型返回 false。
func EstimateOfficialCost(model string, inputTokens, cachedInputTokens, outputTokens, contextInputTokens int64) (PricingResult, bool) {
	price, ok := resolveOfficialTokenPrice(model)
	if !ok {
		return PricingResult{}, false
	}
	inputPrice := price.InputTicks
	cachedPrice := price.CachedInputTicks
	outputPrice := price.OutputTicks
	contextTokens := contextInputTokens
	if contextTokens <= 0 {
		contextTokens = inputTokens
	}
	if price.LongContextTokens > 0 && contextTokens > price.LongContextTokens {
		inputPrice = price.LongInputTicks
		cachedPrice = price.LongCachedTicks
		outputPrice = price.LongOutputTicks
	}
	cachedTokens := max(int64(0), min(cachedInputTokens, inputTokens))
	uncachedTokens := max(int64(0), inputTokens-cachedTokens)
	outputTokens = max(int64(0), outputTokens)
	return PricingResult{Model: price.CanonicalModel, CostInUSDTicks: uncachedTokens*inputPrice + cachedTokens*cachedPrice + outputTokens*outputPrice}, true
}

// EstimateOfficialTextReservation 根据请求内容和输出上限计算保守的文本费用预留。
func EstimateOfficialTextReservation(model string, body []byte) (PricingResult, bool) {
	if _, ok := resolveOfficialTokenPrice(model); !ok {
		return PricingResult{}, false
	}
	inputTokens := estimateRequestInputTokens(body)
	outputTokens := estimateRequestOutputLimit(body)
	return EstimateOfficialCost(model, inputTokens, 0, outputTokens, inputTokens)
}

func estimateRequestOutputLimit(body []byte) int64 {
	const defaultOutputTokens int64 = 16_384
	const maximumOutputTokens int64 = 131_072
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return defaultOutputTokens
	}
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		var value int64
		if raw, ok := payload[key]; ok && json.Unmarshal(raw, &value) == nil && value > 0 {
			return min(value, maximumOutputTokens)
		}
	}
	return defaultOutputTokens
}

func estimateRequestInputTokens(body []byte) int64 {
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return max(256, int64((len(body)+2)/3))
	}
	return max(256, estimateJSONTokens(payload)+128)
}

func estimateJSONTokens(value any) int64 {
	switch typed := value.(type) {
	case map[string]any:
		var total int64
		for key, child := range typed {
			total += int64((len(key)+2)/3) + 1 + estimateJSONTokens(child)
		}
		return total
	case []any:
		var total int64
		for _, child := range typed {
			total += 1 + estimateJSONTokens(child)
		}
		return total
	case string:
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "data:image/") || strings.HasPrefix(trimmed, "data:video/") {
			return 256
		}
		return max(1, int64((len(typed)+2)/3))
	case json.Number, float64, bool:
		return 1
	case nil:
		return 0
	default:
		encoded, _ := json.Marshal(typed)
		return max(1, int64((len(encoded)+2)/3))
	}
}

// EstimateOfficialImageCost 按客户端请求的 n 计算 Grok Imagine 图片费用。
func EstimateOfficialImageCost(model, resolution string, count int) (PricingResult, bool) {
	if count <= 0 {
		return PricingResult{}, false
	}
	model = normalizePricingModel(model)
	if model == "grok-imagine-image" {
		return PricingResult{Model: "grok-imagine-image", CostInUSDTicks: int64(count) * 200_000_000}, true
	}
	if model != "grok-imagine-image-quality" {
		return PricingResult{}, false
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	if resolution == "" {
		resolution = "1k"
	}
	var ticksPerImage int64
	switch resolution {
	case "1k":
		ticksPerImage = 500_000_000
	case "2k":
		ticksPerImage = 700_000_000
	default:
		return PricingResult{}, false
	}
	return PricingResult{
		Model:          "grok-imagine-image-quality-" + resolution,
		CostInUSDTicks: int64(count) * ticksPerImage,
	}, true
}

// EstimateOfficialImageEditCost 按输出图片数量计费，并叠加每张输入图片的处理费用。
func EstimateOfficialImageEditCost(model, resolution string, outputCount, inputCount int) (PricingResult, bool) {
	model = normalizePricingModel(model)
	if outputCount <= 0 || inputCount <= 0 {
		return PricingResult{}, false
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	if resolution == "" {
		resolution = "1k"
	}
	pricingModel := ""
	inputTicks := officialImageEditInputTicks
	var outputTicks int64
	switch model {
	case "grok-imagine-image-edit":
		pricingModel = "grok-imagine-image-edit-" + resolution
	case "grok-imagine-image-quality":
		pricingModel = "grok-imagine-image-quality-edit-" + resolution
	case "grok-imagine-image":
		pricingModel = "grok-imagine-image-edit-lite-" + resolution
		inputTicks = officialLiteImageInputTicks
		outputTicks = 200_000_000
	default:
		return PricingResult{}, false
	}
	if resolution != "1k" && resolution != "2k" {
		return PricingResult{}, false
	}
	if outputTicks == 0 {
		if resolution == "1k" {
			outputTicks = 500_000_000
		} else {
			outputTicks = 700_000_000
		}
	}
	return PricingResult{
		Model:          pricingModel,
		CostInUSDTicks: int64(outputCount)*outputTicks + int64(inputCount)*inputTicks,
	}, true
}

// EstimateOfficialVideoCost 按请求视频时长和分辨率计算费用。
// 仅精确支持 grok-imagine-video 与 grok-imagine-video-1.5（可带来源前缀）；未知后缀拒绝。
func EstimateOfficialVideoCost(model, resolution string, seconds int) (PricingResult, bool) {
	if seconds <= 0 {
		return PricingResult{}, false
	}
	baseModel, ok := officialVideoPricingModel(model)
	if !ok {
		return PricingResult{}, false
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	var ticksPerSecond int64
	switch resolution {
	case "480p":
		ticksPerSecond = 800_000_000
	case "720p":
		ticksPerSecond = 1_400_000_000
	default:
		return PricingResult{}, false
	}
	return PricingResult{
		Model:          baseModel + "-" + resolution,
		CostInUSDTicks: int64(seconds) * ticksPerSecond,
	}, true
}

// ReconstructOfficialCost builds an admin-facing formula without adding work to the request settlement path.
func ReconstructOfficialCost(model string, inputTokens, cachedInputTokens, outputTokens, contextInputTokens, inputImages, outputImages, outputSeconds int64) (PricingBreakdown, bool) {
	normalized := normalizePricingModel(model)
	switch normalized {
	case "grok-imagine-image":
		return reconstructImageCost(normalized, "", outputImages)
	case "grok-imagine-image-quality-1k":
		return reconstructImageCost("grok-imagine-image-quality", "1k", outputImages)
	case "grok-imagine-image-quality-2k":
		return reconstructImageCost("grok-imagine-image-quality", "2k", outputImages)
	case "grok-imagine-image-edit-1k":
		return reconstructImageEditCost("grok-imagine-image-edit", "1k", inputImages, outputImages)
	case "grok-imagine-image-edit-2k":
		return reconstructImageEditCost("grok-imagine-image-edit", "2k", inputImages, outputImages)
	case "grok-imagine-image-quality-edit-1k":
		return reconstructImageEditCost("grok-imagine-image-quality", "1k", inputImages, outputImages)
	case "grok-imagine-image-quality-edit-2k":
		return reconstructImageEditCost("grok-imagine-image-quality", "2k", inputImages, outputImages)
	case "grok-imagine-image-edit-lite-1k":
		return reconstructImageEditCost("grok-imagine-image", "1k", inputImages, outputImages)
	case "grok-imagine-image-edit-lite-2k":
		return reconstructImageEditCost("grok-imagine-image", "2k", inputImages, outputImages)
	case "grok-imagine-video-480p":
		return reconstructVideoCost("grok-imagine-video", "480p", outputSeconds)
	case "grok-imagine-video-720p":
		return reconstructVideoCost("grok-imagine-video", "720p", outputSeconds)
	case "grok-imagine-video-1.5-480p":
		return reconstructVideoCost("grok-imagine-video-1.5", "480p", outputSeconds)
	case "grok-imagine-video-1.5-720p":
		return reconstructVideoCost("grok-imagine-video-1.5", "720p", outputSeconds)
	default:
		return reconstructTextCost(normalized, inputTokens, cachedInputTokens, outputTokens, contextInputTokens)
	}
}

func reconstructTextCost(model string, inputTokens, cachedInputTokens, outputTokens, contextInputTokens int64) (PricingBreakdown, bool) {
	price, ok := resolveOfficialTokenPrice(model)
	if !ok {
		return PricingBreakdown{}, false
	}
	inputPrice, cachedPrice, outputPrice := price.InputTicks, price.CachedInputTicks, price.OutputTicks
	tier := PricingTierStandard
	contextTokens := contextInputTokens
	if contextTokens <= 0 {
		contextTokens = inputTokens
	}
	if price.LongContextTokens > 0 && contextTokens > price.LongContextTokens {
		inputPrice, cachedPrice, outputPrice = price.LongInputTicks, price.LongCachedTicks, price.LongOutputTicks
		tier = PricingTierLongContext
	}
	cachedTokens := max(int64(0), min(cachedInputTokens, inputTokens))
	return newPricingBreakdown(price.CanonicalModel, tier,
		newPricingComponent(PricingComponentUncachedInput, PricingUnitToken, max(int64(0), inputTokens-cachedTokens), inputPrice),
		newPricingComponent(PricingComponentOutput, PricingUnitToken, outputTokens, outputPrice),
		newPricingComponent(PricingComponentCachedInput, PricingUnitToken, cachedTokens, cachedPrice),
	), true
}

func reconstructImageCost(model, resolution string, count int64) (PricingBreakdown, bool) {
	if count <= 0 || int64(int(count)) != count {
		return PricingBreakdown{}, false
	}
	result, ok := EstimateOfficialImageCost(model, resolution, int(count))
	if !ok {
		return PricingBreakdown{}, false
	}
	return newPricingBreakdown(result.Model, PricingTierMedia,
		newPricingComponent(PricingComponentOutputImage, PricingUnitImage, count, result.CostInUSDTicks/count),
	), true
}

func reconstructImageEditCost(model, resolution string, inputCount, outputCount int64) (PricingBreakdown, bool) {
	if inputCount <= 0 || outputCount <= 0 || int64(int(inputCount)) != inputCount || int64(int(outputCount)) != outputCount {
		return PricingBreakdown{}, false
	}
	result, ok := EstimateOfficialImageEditCost(model, resolution, int(outputCount), int(inputCount))
	if !ok {
		return PricingBreakdown{}, false
	}
	inputTicks := officialImageEditInputTicks
	if normalizePricingModel(model) == "grok-imagine-image" {
		inputTicks = officialLiteImageInputTicks
	}
	outputCost := result.CostInUSDTicks - inputCount*inputTicks
	if outputCost < 0 || outputCost%outputCount != 0 {
		return PricingBreakdown{}, false
	}
	return newPricingBreakdown(result.Model, PricingTierMedia,
		newPricingComponent(PricingComponentOutputImage, PricingUnitImage, outputCount, outputCost/outputCount),
		newPricingComponent(PricingComponentInputImage, PricingUnitImage, inputCount, inputTicks),
	), true
}

func reconstructVideoCost(model, resolution string, seconds int64) (PricingBreakdown, bool) {
	if seconds <= 0 || int64(int(seconds)) != seconds {
		return PricingBreakdown{}, false
	}
	result, ok := EstimateOfficialVideoCost(model, resolution, int(seconds))
	if !ok {
		return PricingBreakdown{}, false
	}
	return newPricingBreakdown(result.Model, PricingTierMedia,
		newPricingComponent(PricingComponentOutputSecond, PricingUnitSecond, seconds, result.CostInUSDTicks/seconds),
	), true
}

func newPricingComponent(kind PricingComponentKind, unit PricingUnit, quantity, unitPriceInUSDTicks int64) PricingComponent {
	quantity = max(int64(0), quantity)
	unitPriceInUSDTicks = max(int64(0), unitPriceInUSDTicks)
	return PricingComponent{
		Kind: kind, Unit: unit, Quantity: quantity, UnitPriceInUSDTicks: unitPriceInUSDTicks,
		CostInUSDTicks: quantity * unitPriceInUSDTicks,
	}
}

func newPricingBreakdown(model string, tier PricingTier, components ...PricingComponent) PricingBreakdown {
	result := PricingBreakdown{Model: model, Tier: tier, Components: components}
	for _, component := range components {
		result.CostInUSDTicks += component.CostInUSDTicks
	}
	return result
}

func officialVideoPricingModel(model string) (string, bool) {
	switch normalizePricingModel(model) {
	case "grok-imagine-video":
		return "grok-imagine-video", true
	case "grok-imagine-video-1.5":
		return "grok-imagine-video-1.5", true
	default:
		return "", false
	}
}
