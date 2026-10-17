package settings

import (
	"strconv"
	"strings"
)

const (
	RecommendedBuildClientVersion = "1.0.40"
	RecommendedBuildUserAgent     = "grok-shell/" + RecommendedBuildClientVersion + " (linux; x86_64)"
	MinimumBuildClientVersion     = "1.0.13"
)

type semanticVersion struct {
	major, minor, patch uint64
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	if before, _, ok := strings.Cut(value, "+"); ok {
		value = before
	}
	if before, _, ok := strings.Cut(value, "-"); ok {
		value = before
	}
	parts := strings.Split(value, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return semanticVersion{}, false
	}
	numbers := make([]uint64, 3)
	for i, part := range parts {
		if part == "" {
			return semanticVersion{}, false
		}
		num, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semanticVersion{}, false
		}
		numbers[i] = num
	}
	return semanticVersion{major: numbers[0], minor: numbers[1], patch: numbers[2]}, true
}

func compareSemanticVersion(left, right semanticVersion) int {
	for _, pair := range [][2]uint64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// CompareSemanticVersion compares two semantic version strings (e.g. "1.0.4" and "1.0.13").
// It returns -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2, and false if either string is invalid.
func CompareSemanticVersion(v1, v2 string) (int, bool) {
	parsed1, ok1 := parseSemanticVersion(v1)
	parsed2, ok2 := parseSemanticVersion(v2)
	if !ok1 || !ok2 {
		return 0, false
	}
	return compareSemanticVersion(parsed1, parsed2), true
}

// IsBuildClientVersionOutdated reports whether version is empty, invalid, or strictly
// lower than MinimumBuildClientVersion ("1.0.13").
func IsBuildClientVersionOutdated(version string) bool {
	v := strings.TrimSpace(version)
	if v == "" {
		return true
	}
	cmp, ok := CompareSemanticVersion(v, MinimumBuildClientVersion)
	if !ok {
		return true
	}
	return cmp < 0
}

// NormalizeBuildClientVersionAndUserAgent returns RecommendedBuildClientVersion and
// RecommendedBuildUserAgent if the given version is outdated (< MinimumBuildClientVersion).
// Otherwise it keeps the supplied version and userAgent (filling in a default userAgent if empty).
func NormalizeBuildClientVersionAndUserAgent(version, userAgent string) (string, string) {
	v := strings.TrimSpace(version)
	ua := strings.TrimSpace(userAgent)
	if IsBuildClientVersionOutdated(v) {
		return RecommendedBuildClientVersion, RecommendedBuildUserAgent
	}
	if ua == "" {
		ua = "grok-shell/" + v + " (linux; x86_64)"
	}
	return v, ua
}
