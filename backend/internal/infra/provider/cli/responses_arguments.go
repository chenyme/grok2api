package cli

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

const (
	maxExactJSONInteger      int64 = 1<<53 - 1
	maxNormalizedNumberBytes       = 256
	maxExactJSONIntegerText        = "9007199254740991"
)

// normalizeFunctionArguments repairs semantically integral JSON numbers that strict
// downstream decoders reject for integer fields. Grok Build can emit 60000.0 where
// clients such as Codex require the integer spelling 60000.
func normalizeFunctionArguments(arguments string, schema any) (string, bool) {
	if strings.TrimSpace(arguments) == "" {
		return arguments, false
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return arguments, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return arguments, false
	}
	root, ok := schema.(map[string]any)
	if !ok {
		return arguments, false
	}
	normalized, changed := normalizeArgumentValue(value, root, root, 0)
	if !changed {
		return arguments, false
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return arguments, false
	}
	return string(encoded), true
}

func normalizeArgumentValue(value any, schema, root map[string]any, depth int) (any, bool) {
	if depth > 64 {
		return value, false
	}
	changed := false
	if ref, ok := schema["$ref"].(string); ok {
		if resolved, ok := resolveLocalSchemaRef(root, ref); ok {
			var current bool
			value, current = normalizeArgumentValue(value, resolved, root, depth+1)
			changed = changed || current
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[keyword].([]any)
		for _, rawBranch := range branches {
			branch, ok := rawBranch.(map[string]any)
			if !ok {
				continue
			}
			var current bool
			value, current = normalizeArgumentValue(value, branch, root, depth+1)
			changed = changed || current
		}
	}
	if number, ok := value.(json.Number); ok && schemaRequiresInteger(schema) {
		if normalized, ok := normalizeIntegralNumber(number); ok {
			return normalized, true
		}
		return value, changed
	}
	switch typed := value.(type) {
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		additional, _ := schema["additionalProperties"].(map[string]any)
		for key, item := range typed {
			property, ok := properties[key].(map[string]any)
			if !ok {
				property = additional
			}
			if property == nil {
				continue
			}
			normalized, current := normalizeArgumentValue(item, property, root, depth+1)
			if current {
				typed[key] = normalized
				changed = true
			}
		}
	case []any:
		prefixItems, _ := schema["prefixItems"].([]any)
		items, _ := schema["items"].(map[string]any)
		for index, item := range typed {
			itemSchema := items
			if index < len(prefixItems) {
				if prefixSchema, ok := prefixItems[index].(map[string]any); ok {
					itemSchema = prefixSchema
				}
			}
			if itemSchema == nil {
				continue
			}
			normalized, current := normalizeArgumentValue(item, itemSchema, root, depth+1)
			if current {
				typed[index] = normalized
				changed = true
			}
		}
	}
	return value, changed
}

func schemaRequiresInteger(schema map[string]any) bool {
	switch value := schema["type"].(type) {
	case string:
		return value == "integer"
	case []any:
		integer := false
		for _, item := range value {
			kind, _ := item.(string)
			if kind == "number" {
				return false
			}
			integer = integer || kind == "integer"
		}
		return integer
	default:
		return false
	}
}

func normalizeIntegralNumber(number json.Number) (json.Number, bool) {
	raw := number.String()
	if len(raw) > maxNormalizedNumberBytes || !strings.ContainsAny(raw, ".eE") {
		return number, false
	}
	mantissa := raw
	exponentText := ""
	if index := strings.IndexAny(mantissa, "eE"); index >= 0 {
		exponentText = mantissa[index+1:]
		mantissa = mantissa[:index]
	}
	negative := strings.HasPrefix(mantissa, "-")
	if negative {
		mantissa = strings.TrimPrefix(mantissa, "-")
	}
	whole, fraction, hasFraction := strings.Cut(mantissa, ".")
	if !hasFraction {
		fraction = ""
	}
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return json.Number("0"), raw != "0"
	}
	exponent, ok := parseBoundedDecimalExponent(exponentText)
	if !ok {
		return number, false
	}
	decimalShift := exponent - len(fraction)
	if decimalShift < 0 {
		fractionalDigits := -decimalShift
		if fractionalDigits > len(digits) || strings.Trim(digits[len(digits)-fractionalDigits:], "0") != "" {
			return number, false
		}
		digits = strings.TrimLeft(digits[:len(digits)-fractionalDigits], "0")
		if digits == "" {
			return json.Number("0"), true
		}
	} else if decimalShift > 0 {
		if decimalShift > len(maxExactJSONIntegerText)-len(digits) {
			return number, false
		}
		digits += strings.Repeat("0", decimalShift)
	}
	if len(digits) > len(maxExactJSONIntegerText) || len(digits) == len(maxExactJSONIntegerText) && digits > maxExactJSONIntegerText {
		return number, false
	}
	normalized := digits
	if negative {
		normalized = "-" + normalized
	}
	return json.Number(normalized), normalized != raw
}

func parseBoundedDecimalExponent(raw string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	sign := 1
	if raw[0] == '+' || raw[0] == '-' {
		if raw[0] == '-' {
			sign = -1
		}
		raw = raw[1:]
		if raw == "" {
			return 0, false
		}
	}
	raw = strings.TrimLeft(raw, "0")
	if raw == "" {
		return 0, true
	}
	if len(raw) > 9 {
		return 0, false
	}
	exponent, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return sign * exponent, true
}

func schemaContainsInteger(value any) bool {
	root, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return schemaContainsReachableInteger(root, root, make(map[string]struct{}), 0)
}

func schemaContainsReachableInteger(schema, root map[string]any, visitedRefs map[string]struct{}, depth int) bool {
	if depth > 64 || schema == nil {
		return false
	}
	if schemaRequiresInteger(schema) {
		return true
	}
	if ref, ok := schema["$ref"].(string); ok {
		if _, visited := visitedRefs[ref]; !visited {
			visitedRefs[ref] = struct{}{}
			if resolved, resolvedOK := resolveLocalSchemaRef(root, ref); resolvedOK && schemaContainsReachableInteger(resolved, root, visitedRefs, depth+1) {
				return true
			}
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		branches, _ := schema[keyword].([]any)
		for _, rawBranch := range branches {
			branch, ok := rawBranch.(map[string]any)
			if ok && schemaContainsReachableInteger(branch, root, visitedRefs, depth+1) {
				return true
			}
		}
	}
	for _, keyword := range []string{"items", "additionalProperties"} {
		child, _ := schema[keyword].(map[string]any)
		if schemaContainsReachableInteger(child, root, visitedRefs, depth+1) {
			return true
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, rawProperty := range properties {
		property, ok := rawProperty.(map[string]any)
		if ok && schemaContainsReachableInteger(property, root, visitedRefs, depth+1) {
			return true
		}
	}
	return false
}
