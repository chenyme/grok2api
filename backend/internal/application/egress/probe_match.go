package egress

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	MatchContains = "contains"
	MatchLastLine = "last_line"
	MatchRegex    = "regex"
)

func NormalizeMatchMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case MatchLastLine, "last-line", "lastline":
		return MatchLastLine
	case MatchRegex, "regexp":
		return MatchRegex
	default:
		return MatchContains
	}
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

// MatchExpected reports whether the probe body satisfies the expected marker.
// An empty expected string always matches so throughput-only profiles can skip
// content checks.
func MatchExpected(text, expected, mode string) bool {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return true
	}
	text = strings.TrimRightFunc(text, unicode.IsSpace)
	switch NormalizeMatchMode(mode) {
	case MatchLastLine:
		line := lastNonEmptyLine(text)
		if line == "" {
			return false
		}
		return strings.EqualFold(line, expected)
	case MatchRegex:
		re, err := regexp.Compile(expected)
		if err != nil {
			return false
		}
		return re.MatchString(text)
	default:
		return strings.Contains(text, expected)
	}
}
