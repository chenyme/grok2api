package audit

const (
	DegradeClassBurst = "buffered_burst"
	DegradeClassSoft  = "soft_tps"
	DegradeClassHard  = "hard_tps"
)

const (
	DefaultDegradeSoftTPS   = 500.0
	DefaultDegradeHardTPS   = 1000.0
	DefaultDegradeMinGenMS  = int64(1000)
	DefaultDegradeMinOutput = int64(32)
)

// ClassifyOutputSpeed matches the quality-guard panel formula:
// output tokens / (durationMs - firstTokenMs). In fail-closed mode, short
// generation windows with a soft-or-higher rate are buffered_burst; otherwise
// the hard and soft thresholds apply in that order.
func ClassifyOutputSpeed(outputTokens, firstTokenMS, durationMS int64, softTPS, hardTPS float64, minGenMS int64, failClosed bool) (class string, tps float64, genMS int64) {
	genMS = durationMS - firstTokenMS
	if genMS <= 0 || outputTokens <= 0 {
		return "", 0, genMS
	}
	tps = float64(outputTokens) * 1000 / float64(genMS)
	if failClosed && minGenMS > 0 && genMS < minGenMS && tps >= softTPS {
		return DegradeClassBurst, tps, genMS
	}
	if tps >= hardTPS {
		return DegradeClassHard, tps, genMS
	}
	if tps >= softTPS {
		return DegradeClassSoft, tps, genMS
	}
	return "", tps, genMS
}
