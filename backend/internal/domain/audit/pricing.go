package audit

import (
	"encoding/json"
	"strings"
)

const (
	OfficialPricingSource = "https://docs.x.ai/developers/pricing"
	OfficialPricingAsOf   = "2026-07-12"
)

type PricingResult struct {
	Model          string
	CostInUSDTicks int64
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
	register("grok-build-0.1", tokenPrice{InputTicks: 10000, CachedInputTicks: 2000, OutputTicks: 20000},
		"grok-code-fast-1", "grok-code-fast", "grok-code-fast-1-0825", "grok-composer-2.5-fast")
	register("grok-4.5", tokenPrice{InputTicks: 20000, CachedInputTicks: 5000, OutputTicks: 60000, LongContextTokens: 200000, LongInputTicks: 40000, LongCachedTicks: 10000, LongOutputTicks: 120000},
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

// EstimateOfficialCost 按官方模型价格计算单次请求成本；未知模型返回 false。
func EstimateOfficialCost(model string, inputTokens, cachedInputTokens, outputTokens, contextInputTokens int64) (PricingResult, bool) {
	price, ok := officialTokenPrices[strings.ToLower(strings.TrimSpace(model))]
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
	if _, ok := officialTokenPrices[strings.ToLower(strings.TrimSpace(model))]; !ok {
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
	model = strings.ToLower(strings.TrimSpace(model))
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
	if strings.ToLower(strings.TrimSpace(model)) != "grok-imagine-image-edit" || outputCount <= 0 || inputCount <= 0 {
		return PricingResult{}, false
	}
	resolution = strings.ToLower(strings.TrimSpace(resolution))
	if resolution == "" {
		resolution = "1k"
	}
	var outputTicks int64
	switch resolution {
	case "1k":
		outputTicks = 500_000_000
	case "2k":
		outputTicks = 700_000_000
	default:
		return PricingResult{}, false
	}
	return PricingResult{
		Model:          "grok-imagine-image-edit-" + resolution,
		CostInUSDTicks: int64(outputCount)*outputTicks + int64(inputCount)*100_000_000,
	}, true
}

// EstimateOfficialVideoCost 按请求视频时长和分辨率计算费用。
func EstimateOfficialVideoCost(model, resolution string, seconds int) (PricingResult, bool) {
	if strings.ToLower(strings.TrimSpace(model)) != "grok-imagine-video" || seconds <= 0 {
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
		Model:          "grok-imagine-video-" + resolution,
		CostInUSDTicks: int64(seconds) * ticksPerSecond,
	}, true
}
