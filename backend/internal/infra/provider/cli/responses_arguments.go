package cli

import (
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
)

const maxExactJSONInteger = float64(1<<53 - 1)

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
	if !strings.ContainsAny(raw, ".eE") {
		return number, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || math.Trunc(value) != value || math.Abs(value) > maxExactJSONInteger {
		return number, false
	}
	normalized := strconv.FormatFloat(value, 'f', -1, 64)
	if normalized == "-0" {
		normalized = "0"
	}
	return json.Number(normalized), normalized != raw
}

func schemaContainsInteger(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if schemaRequiresInteger(typed) {
			return true
		}
		for _, child := range typed {
			if schemaContainsInteger(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if schemaContainsInteger(child) {
				return true
			}
		}
	}
	return false
}
