package audit

import "testing"

func TestEstimateOfficialCostHandlesCacheAliasesAndLongContext(t *testing.T) {
	result, ok := EstimateOfficialCost("grok-code-fast-1", 1_000_000, 200_000, 500_000, 100_000)
	if !ok || result.Model != "grok-build-0.1" || result.CostInUSDTicks != 18_400_000_000 {
		t.Fatalf("standard result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialCost("grok-composer-2.5-fast", 1_000_000, 200_000, 500_000, 256_000)
	if !ok || result.Model != "grok-build-0.1" || result.CostInUSDTicks != 36_800_000_000 {
		t.Fatalf("composer result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialCost("grok-4.5", 1_000_000, 200_000, 500_000, 210_000)
	if !ok || result.CostInUSDTicks != 93_200_000_000 {
		t.Fatalf("long-context result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialCost("Web/grok-4.5-build-free", 100, 0, 50, 100)
	if !ok || result.Model != "grok-4.5" || result.CostInUSDTicks != 5_000_000 {
		t.Fatalf("dynamic prefixed alias = %#v, ok = %v", result, ok)
	}
}

func TestEstimateOfficialCostMatchesControlledModelFamilies(t *testing.T) {
	tests := []struct {
		model     string
		canonical string
	}{
		{model: "Build/grok-build-0.1", canonical: "grok-build-0.1"},
		{model: "grok_build/grok-code-fast-1-0825", canonical: "grok-build-0.1"},
		{model: "Console/grok-4.3-high", canonical: "grok-4.3"},
		{model: "Web/grok-4.5-2026-07-14", canonical: "grok-4.5"},
		{model: "Console/grok-4.20-multi-agent-beta-0310", canonical: "grok-4.20-multi-agent-0309"},
		{model: "Console/grok-4.20-experimental-beta-0310-non-reasoning", canonical: "grok-4.20-0309-non-reasoning"},
		{model: "Console/grok-4.20-experimental-beta-0310-reasoning", canonical: "grok-4.20-0309-reasoning"},
	}
	for _, test := range tests {
		result, ok := EstimateOfficialCost(test.model, 1_000_000, 0, 0, 100_000)
		if !ok || result.Model != test.canonical {
			t.Fatalf("EstimateOfficialCost(%q) = %#v, %v", test.model, result, ok)
		}
	}
	for _, model := range []string{"my-grok-4.5", "Other/grok-4.5", "grok-4.50", "grok-4.5/preview"} {
		if result, ok := EstimateOfficialCost(model, 100, 0, 50, 100); ok {
			t.Fatalf("unsafe model %q was priced as %#v", model, result)
		}
	}
}

func TestOfficialPricingMatchesPublishedTokenRates(t *testing.T) {
	tests := []struct {
		model                             string
		inputCost, cachedCost, outputCost int64
	}{
		{model: "grok-build-0.1", inputCost: 10_000_000_000, cachedCost: 2_000_000_000, outputCost: 20_000_000_000},
		{model: "grok-4.5", inputCost: 20_000_000_000, cachedCost: 3_000_000_000, outputCost: 60_000_000_000},
		{model: "grok-4.3", inputCost: 12_500_000_000, cachedCost: 2_000_000_000, outputCost: 25_000_000_000},
		{model: "grok-4.20-multi-agent-0309", inputCost: 12_500_000_000, cachedCost: 2_000_000_000, outputCost: 25_000_000_000},
		{model: "grok-4.20-0309-reasoning", inputCost: 12_500_000_000, cachedCost: 2_000_000_000, outputCost: 25_000_000_000},
		{model: "grok-4.20-0309-non-reasoning", inputCost: 12_500_000_000, cachedCost: 2_000_000_000, outputCost: 25_000_000_000},
	}
	for _, test := range tests {
		input, inputOK := EstimateOfficialCost(test.model, 1_000_000, 0, 0, 100_000)
		cached, cachedOK := EstimateOfficialCost(test.model, 1_000_000, 1_000_000, 0, 100_000)
		output, outputOK := EstimateOfficialCost(test.model, 0, 0, 1_000_000, 100_000)
		if !inputOK || !cachedOK || !outputOK || input.CostInUSDTicks != test.inputCost || cached.CostInUSDTicks != test.cachedCost || output.CostInUSDTicks != test.outputCost {
			t.Fatalf("published price %q = input %#v, cached %#v, output %#v", test.model, input, cached, output)
		}
	}
	longBuild, ok := EstimateOfficialCost("Build/grok-build-0.1", 1_000_000, 0, 1_000_000, 200_001)
	if !ok || longBuild.CostInUSDTicks != 60_000_000_000 {
		t.Fatalf("long-context Build price = %#v, %v", longBuild, ok)
	}
}

func TestMediaPricingAcceptsProviderPrefixes(t *testing.T) {
	if result, ok := EstimateOfficialImageCost("Web/grok-imagine-image", "1k", 1); !ok || result.CostInUSDTicks != 200_000_000 {
		t.Fatalf("prefixed image price = %#v, %v", result, ok)
	}
	if result, ok := EstimateOfficialImageEditCost("grok_web/grok-imagine-image-edit", "1k", 1, 1); !ok || result.CostInUSDTicks != 600_000_000 {
		t.Fatalf("prefixed image edit price = %#v, %v", result, ok)
	}
	if result, ok := EstimateOfficialVideoCost("Web/grok-imagine-video", "480p", 1); !ok || result.CostInUSDTicks != 800_000_000 {
		t.Fatalf("prefixed video price = %#v, %v", result, ok)
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

	result, ok = EstimateOfficialVideoCost("grok-imagine-video-1.5", "720p", 6)
	if !ok || result.Model != "grok-imagine-video-1.5-720p" || result.CostInUSDTicks != 8_400_000_000 {
		t.Fatalf("1.5 720p video result = %#v, ok = %v", result, ok)
	}
	result, ok = EstimateOfficialVideoCost("Build/grok-imagine-video-1.5", "480p", 1)
	if !ok || result.Model != "grok-imagine-video-1.5-480p" || result.CostInUSDTicks != 800_000_000 {
		t.Fatalf("prefixed 1.5 video result = %#v, ok = %v", result, ok)
	}
	if result, ok = EstimateOfficialVideoCost("grok-imagine-video-1.5-fast", "720p", 6); ok || result.CostInUSDTicks != 0 {
		t.Fatalf("unknown 1.5 suffix was priced = %#v, ok = %v", result, ok)
	}
	if result, ok = EstimateOfficialVideoCost("grok-imagine-video-2.0", "720p", 6); ok || result.CostInUSDTicks != 0 {
		t.Fatalf("unknown video model was priced = %#v, ok = %v", result, ok)
	}
}
