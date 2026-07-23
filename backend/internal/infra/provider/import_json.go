package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

// DecodeCredentialJSONEntries accepts a regular credential document or a
// sequence of JSON objects, including one object per line.
func DecodeCredentialJSONEntries[T any](data []byte, expectedProvider string, limit int) ([]T, error) {
	data = bytes.TrimPrefix(data, utf8BOM)
	decoder := json.NewDecoder(bytes.NewReader(data))
	entries := make([]T, 0)
	for {
		start := nextJSONValueOffset(data, decoder.InputOffset())
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return entries, nil
			}
			return nil, fmt.Errorf("第 %d 行的账号 JSON 格式无效", jsonErrorLine(data, start, err))
		}

		line := lineAtJSONOffset(data, start)
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return nil, fmt.Errorf("第 %d 行必须是 JSON 对象", line)
		}
		if rawProvider, exists := object["provider"]; exists {
			var providerName string
			if err := json.Unmarshal(rawProvider, &providerName); err != nil {
				return nil, fmt.Errorf("第 %d 行的 provider 必须是字符串", line)
			}
			providerName = strings.TrimSpace(providerName)
			if providerName != "" && providerName != expectedProvider {
				return nil, fmt.Errorf("第 %d 行的 Provider 必须是 %s", line, expectedProvider)
			}
		}

		if rawAccounts, batch := object["accounts"]; batch {
			var values []T
			if err := json.Unmarshal(rawAccounts, &values); err != nil {
				return nil, fmt.Errorf("第 %d 行的 accounts 必须是 JSON 对象数组", line)
			}
			if err := appendCredentialJSONEntries(&entries, values, limit); err != nil {
				return nil, err
			}
			continue
		}

		var value T
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("第 %d 行的账号 JSON 格式无效", line)
		}
		if err := appendCredentialJSONEntries(&entries, []T{value}, limit); err != nil {
			return nil, err
		}
	}
}

func appendCredentialJSONEntries[T any](target *[]T, values []T, limit int) error {
	if limit > 0 && len(values) > limit-len(*target) {
		return fmt.Errorf("%w: 单次最多导入 %d 个账号", ErrCredentialLimit, limit)
	}
	*target = append(*target, values...)
	return nil
}

func nextJSONValueOffset(data []byte, offset int64) int64 {
	for offset < int64(len(data)) {
		switch data[offset] {
		case ' ', '\t', '\r', '\n':
			offset++
		default:
			return offset
		}
	}
	return offset
}

func jsonErrorLine(data []byte, fallback int64, err error) int {
	var syntaxError *json.SyntaxError
	if errors.As(err, &syntaxError) && syntaxError.Offset > 0 {
		return lineAtJSONOffset(data, syntaxError.Offset-1)
	}
	return lineAtJSONOffset(data, fallback)
}

func lineAtJSONOffset(data []byte, offset int64) int {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	return 1 + bytes.Count(data[:offset], []byte{'\n'})
}
