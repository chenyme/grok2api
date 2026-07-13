package audit

import "testing"

func TestEstimateOfficialCostHandlesCacheAliasesAndLongContext(t *testing.T) {
	result, ok := EstimateOfficialCost("grok-code-fast-1", 1_000_000, 200_000, 500_000, 100_000)
	if !ok || result.Model != "grok-build-0.1" || result.CostInUSDTicks != 18_400_000_000 {
		t.Fatalf("standard result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialCost("grok-composer-2.5-fast", 1_000_000, 200_000, 500_000, 256_000)
	if !ok || result.Model != "grok-build-0.1" || result.CostInUSDTicks != 18_400_000_000 {
		t.Fatalf("composer result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialCost("grok-4.5", 1_000_000, 200_000, 500_000, 210_000)
	if !ok || result.CostInUSDTicks != 94_000_000_000 {
		t.Fatalf("long-context result = %#v, ok = %v", result, ok)
	}
	if result, ok = EstimateOfficialCost("grok-4.5-build-free", 100, 0, 50, 100); ok || result.CostInUSDTicks != 0 {
		t.Fatalf("unknown model result = %#v, ok = %v", result, ok)
	}
}

func TestEstimateOfficialTextReservationUsesOutputLimitAndIgnoresInlineMediaBytes(t *testing.T) {
	small, ok := EstimateOfficialTextReservation("grok-4.5", []byte(`{"input":"hello","max_output_tokens":1000}`))
	if !ok || small.CostInUSDTicks <= 60_000_000 {
		t.Fatalf("small reservation = %#v, ok = %v", small, ok)
	}
	largeInline, ok := EstimateOfficialTextReservation("grok-4.5", []byte(`{"input":[{"type":"input_image","image_url":"data:image/png;base64,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}],"max_output_tokens":1000}`))
	if !ok || largeInline.CostInUSDTicks > small.CostInUSDTicks+20_000_000 {
		t.Fatalf("inline media reservation = %#v, small = %#v", largeInline, small)
	}
	if _, ok := EstimateOfficialTextReservation("unknown-model", []byte(`{"input":"hello"}`)); ok {
		t.Fatal("unknown model was priced")
	}
}

func TestEstimateOfficialImageCost(t *testing.T) {
	result, ok := EstimateOfficialImageCost("grok-imagine-image-quality", "1k", 2)
	if !ok || result.Model != "grok-imagine-image-quality-1k" || result.CostInUSDTicks != 1_000_000_000 {
		t.Fatalf("1k result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialImageCost("grok-imagine-image-quality", "2k", 3)
	if !ok || result.Model != "grok-imagine-image-quality-2k" || result.CostInUSDTicks != 2_100_000_000 {
		t.Fatalf("2k result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialImageCost("grok-imagine-image", "2k", 4)
	if !ok || result.Model != "grok-imagine-image" || result.CostInUSDTicks != 800_000_000 {
		t.Fatalf("Lite result = %#v, ok = %v", result, ok)
	}
}

func TestEstimateOfficialImageEditCost(t *testing.T) {
	result, ok := EstimateOfficialImageEditCost("grok-imagine-image-edit", "1k", 2, 1)
	if !ok || result.Model != "grok-imagine-image-edit-1k" || result.CostInUSDTicks != 1_100_000_000 {
		t.Fatalf("1k edit result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialImageEditCost("grok-imagine-image-edit", "2K", 3, 4)
	if !ok || result.Model != "grok-imagine-image-edit-2k" || result.CostInUSDTicks != 2_500_000_000 {
		t.Fatalf("2k edit result = %#v, ok = %v", result, ok)
	}
	if result, ok = EstimateOfficialImageEditCost("grok-imagine-image-edit", "4k", 1, 1); ok || result.CostInUSDTicks != 0 {
		t.Fatalf("unknown edit resolution = %#v, ok = %v", result, ok)
	}
}

func TestEstimateOfficialVideoCost(t *testing.T) {
	result, ok := EstimateOfficialVideoCost("grok-imagine-video", "480p", 10)
	if !ok || result.Model != "grok-imagine-video-480p" || result.CostInUSDTicks != 8_000_000_000 {
		t.Fatalf("480p video result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialVideoCost("grok-imagine-video", "720P", 6)
	if !ok || result.Model != "grok-imagine-video-720p" || result.CostInUSDTicks != 8_400_000_000 {
		t.Fatalf("720p video result = %#v, ok = %v", result, ok)
	}
	if result, ok = EstimateOfficialVideoCost("grok-imagine-video", "1080p", 10); ok || result.CostInUSDTicks != 0 {
		t.Fatalf("unpriced video resolution = %#v, ok = %v", result, ok)
	}
}
