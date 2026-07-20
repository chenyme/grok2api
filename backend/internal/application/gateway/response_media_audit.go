package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

type responseMediaSummary struct {
	InputImages   int64
	ImageBytes    int64
	ContentArrays int64
	TextBytes     int64
}

func (summary *responseMediaSummary) add(value responseMediaSummary) {
	summary.InputImages += value.InputImages
	summary.ImageBytes += value.ImageBytes
	summary.ContentArrays += value.ContentArrays
	summary.TextBytes += value.TextBytes
}

type mediaJSONKind uint8

const (
	mediaJSONOther mediaJSONKind = iota
	mediaJSONString
	mediaJSONObjectKind
	mediaJSONArrayKind
)

type mediaJSONValue struct {
	kind        mediaJSONKind
	stringBytes int64
	object      *mediaJSONObject
	array       *mediaJSONArray
}

type mediaJSONObject struct {
	typeName string
	role     string

	textBytes     int64
	imageURLBytes int64
	urlBytes      int64
	dataBytes     int64
	sourceType    string
	sourceBytes   int64

	content  *mediaJSONValue
	output   *mediaJSONValue
	input    *mediaJSONValue
	messages *mediaJSONValue
}

type mediaJSONArray struct {
	contentItems responseMediaSummary
	inputItems   responseMediaSummary
	messageItems responseMediaSummary
}

func summarizeResponseMedia(body []byte) (responseMediaSummary, error) {
	if !mayContainResponseMedia(body) {
		return responseMediaSummary{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	value, err := decodeMediaJSONValue(decoder)
	if err != nil {
		return responseMediaSummary{}, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return responseMediaSummary{}, fmt.Errorf("unexpected trailing JSON token %v", token)
		}
		return responseMediaSummary{}, err
	}
	if value.object == nil {
		return responseMediaSummary{}, nil
	}
	var summary responseMediaSummary
	summary.add(statsForRootInput(value.object.input))
	summary.add(statsForMessages(value.object.messages))
	return summary, nil
}

// mayContainResponseMedia 是推理热路径上的低成本预筛选。绝大多数纯文本请求
// 不再创建 JSON decoder；命中仅代表值得精确扫描，最终统计仍由结构化解析决定。
func mayContainResponseMedia(body []byte) bool {
	return bytes.Contains(body, []byte("image"))
}

func logResponseMediaSummary(logger *slog.Logger, requestID string, summary responseMediaSummary) {
	if logger == nil || summary.InputImages == 0 {
		return
	}
	logger.Debug(
		"request_media_input_summary",
		"request_id", requestID,
		"media_input_images", summary.InputImages,
		"media_input_image_bytes", summary.ImageBytes,
		"media_content_arrays", summary.ContentArrays,
		"media_text_bytes", summary.TextBytes,
	)
}

func decodeMediaJSONValue(decoder *json.Decoder) (*mediaJSONValue, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object, err := decodeMediaJSONObject(decoder)
			return &mediaJSONValue{kind: mediaJSONObjectKind, object: object}, err
		case '[':
			array, err := decodeMediaJSONArray(decoder)
			return &mediaJSONValue{kind: mediaJSONArrayKind, array: array}, err
		default:
			return nil, fmt.Errorf("unexpected JSON delimiter %q", value)
		}
	case string:
		return &mediaJSONValue{kind: mediaJSONString, stringBytes: int64(len(value))}, nil
	default:
		return &mediaJSONValue{kind: mediaJSONOther}, nil
	}
}

func decodeMediaJSONObject(decoder *json.Decoder) (*mediaJSONObject, error) {
	object := &mediaJSONObject{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("JSON object key is not a string")
		}
		switch key {
		case "type":
			object.typeName, err = decodeMediaShortString(decoder)
		case "role":
			object.role, err = decodeMediaShortString(decoder)
		case "text":
			object.textBytes, err = decodeMediaStringBytes(decoder)
		case "image_url":
			object.imageURLBytes, err = decodeMediaImageReference(decoder)
		case "url":
			object.urlBytes, err = decodeMediaDataURIBytes(decoder)
		case "data":
			object.dataBytes, err = decodeMediaBase64Bytes(decoder)
		case "source":
			var value *mediaJSONValue
			value, err = decodeMediaJSONValue(decoder)
			if value != nil && value.object != nil {
				object.sourceType = value.object.typeName
				object.sourceBytes = value.object.dataBytes
			}
		case "content":
			object.content, err = decodeMediaJSONValue(decoder)
		case "output":
			object.output, err = decodeMediaJSONValue(decoder)
		case "input":
			object.input, err = decodeMediaJSONValue(decoder)
		case "messages":
			object.messages, err = decodeMediaJSONValue(decoder)
		default:
			err = skipMediaJSONValue(decoder)
		}
		if err != nil {
			return nil, err
		}
	}
	if token, err := decoder.Token(); err != nil {
		return nil, err
	} else if token != json.Delim('}') {
		return nil, fmt.Errorf("unexpected JSON object terminator %v", token)
	}
	return object, nil
}

func decodeMediaJSONArray(decoder *json.Decoder) (*mediaJSONArray, error) {
	array := &mediaJSONArray{}
	for decoder.More() {
		value, err := decodeMediaJSONValue(decoder)
		if err != nil {
			return nil, err
		}
		array.contentItems.add(statsForContentBlock(value))
		array.inputItems.add(statsForInputItem(value))
		array.messageItems.add(statsForMessage(value))
	}
	if token, err := decoder.Token(); err != nil {
		return nil, err
	} else if token != json.Delim(']') {
		return nil, fmt.Errorf("unexpected JSON array terminator %v", token)
	}
	return array, nil
}

func decodeMediaShortString(decoder *json.Decoder) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", err
	}
	value, ok := token.(string)
	if !ok {
		if delimiter, ok := token.(json.Delim); ok {
			if err := skipMediaJSONContainer(decoder, delimiter); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	if len(value) > 64 {
		return "", nil
	}
	return value, nil
}

func decodeMediaStringBytes(decoder *json.Decoder) (int64, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if value, ok := token.(string); ok {
		return int64(len(value)), nil
	}
	if delimiter, ok := token.(json.Delim); ok {
		return 0, skipMediaJSONContainer(decoder, delimiter)
	}
	return 0, nil
}

func decodeMediaDataURIBytes(decoder *json.Decoder) (int64, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if value, ok := token.(string); ok {
		return dataURIImageBytes(value), nil
	}
	if delimiter, ok := token.(json.Delim); ok {
		return 0, skipMediaJSONContainer(decoder, delimiter)
	}
	return 0, nil
}

func decodeMediaBase64Bytes(decoder *json.Decoder) (int64, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if value, ok := token.(string); ok {
		return decodedBase64Bytes(value), nil
	}
	if delimiter, ok := token.(json.Delim); ok {
		return 0, skipMediaJSONContainer(decoder, delimiter)
	}
	return 0, nil
}

func decodeMediaImageReference(decoder *json.Decoder) (int64, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if value, ok := token.(string); ok {
		return dataURIImageBytes(value), nil
	}
	if delimiter, ok := token.(json.Delim); ok {
		if delimiter == '{' {
			object, err := decodeMediaJSONObject(decoder)
			if err != nil {
				return 0, err
			}
			return max(object.urlBytes, object.imageURLBytes), nil
		}
		return 0, skipMediaJSONContainer(decoder, delimiter)
	}
	return 0, nil
}

func skipMediaJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); ok {
		return skipMediaJSONContainer(decoder, delimiter)
	}
	return nil
}

func skipMediaJSONContainer(decoder *json.Decoder, opening json.Delim) error {
	if opening != '{' && opening != '[' {
		return fmt.Errorf("unexpected JSON delimiter %q", opening)
	}
	for decoder.More() {
		if opening == '{' {
			if _, err := decoder.Token(); err != nil {
				return err
			}
		}
		if err := skipMediaJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func statsForRootInput(value *mediaJSONValue) responseMediaSummary {
	if value == nil {
		return responseMediaSummary{}
	}
	if value.kind == mediaJSONString {
		return responseMediaSummary{TextBytes: value.stringBytes}
	}
	if value.array != nil {
		return value.array.inputItems
	}
	return statsForInputItem(value)
}

func statsForMessages(value *mediaJSONValue) responseMediaSummary {
	if value == nil {
		return responseMediaSummary{}
	}
	if value.array != nil {
		return value.array.messageItems
	}
	return statsForMessage(value)
}

func statsForInputItem(value *mediaJSONValue) responseMediaSummary {
	if value == nil {
		return responseMediaSummary{}
	}
	if value.kind == mediaJSONString {
		return responseMediaSummary{TextBytes: value.stringBytes}
	}
	if value.object == nil {
		return responseMediaSummary{}
	}
	switch value.object.typeName {
	case "message":
		return statsForContentField(value.object.content)
	case "function_call_output":
		return statsForContentField(value.object.output)
	case "input_text", "input_image", "image_url", "image", "text", "tool_result":
		return statsForContentBlock(value)
	default:
		if value.object.role != "" {
			return statsForContentField(value.object.content)
		}
		return responseMediaSummary{}
	}
}

func statsForMessage(value *mediaJSONValue) responseMediaSummary {
	if value == nil || value.object == nil {
		return responseMediaSummary{}
	}
	if value.object.role == "" && value.object.typeName != "message" {
		return responseMediaSummary{}
	}
	return statsForContentField(value.object.content)
}

func statsForContentField(value *mediaJSONValue) responseMediaSummary {
	if value == nil {
		return responseMediaSummary{}
	}
	if value.kind == mediaJSONString {
		return responseMediaSummary{TextBytes: value.stringBytes}
	}
	if value.array == nil {
		return responseMediaSummary{}
	}
	summary := value.array.contentItems
	summary.ContentArrays++
	return summary
}

func statsForContentBlock(value *mediaJSONValue) responseMediaSummary {
	if value == nil {
		return responseMediaSummary{}
	}
	if value.kind == mediaJSONString {
		return responseMediaSummary{TextBytes: value.stringBytes}
	}
	if value.object == nil {
		return responseMediaSummary{}
	}
	object := value.object
	switch object.typeName {
	case "input_text", "text", "output_text":
		return responseMediaSummary{TextBytes: object.textBytes}
	case "input_image":
		return responseMediaSummary{InputImages: 1, ImageBytes: object.imageURLBytes}
	case "image_url":
		return responseMediaSummary{InputImages: 1, ImageBytes: max(object.imageURLBytes, object.urlBytes)}
	case "image":
		imageBytes := object.imageURLBytes
		if object.sourceType == "base64" {
			imageBytes = object.sourceBytes
		}
		return responseMediaSummary{InputImages: 1, ImageBytes: imageBytes}
	case "tool_result":
		return statsForContentField(object.content)
	default:
		return responseMediaSummary{}
	}
}

func dataURIImageBytes(value string) int64 {
	comma := strings.IndexByte(value, ',')
	if comma <= 0 {
		return 0
	}
	header := strings.ToLower(value[:comma])
	if !strings.HasPrefix(header, "data:image/") || !strings.Contains(header, ";base64") {
		return 0
	}
	return decodedBase64Bytes(value[comma+1:])
}

func decodedBase64Bytes(value string) int64 {
	var symbols int64
	var padding int64
	seenPadding := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character == ' ' || character == '\n' || character == '\r' || character == '\t':
			continue
		case character == '=':
			seenPadding = true
			padding++
			if padding > 2 {
				return 0
			}
		case (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '+' || character == '/' || character == '-' || character == '_':
			if seenPadding {
				return 0
			}
		default:
			return 0
		}
		symbols++
	}
	if symbols == 0 {
		return 0
	}
	if padding > 0 && symbols%4 != 0 {
		return 0
	}
	switch remainder := symbols % 4; remainder {
	case 0:
		return symbols/4*3 - padding
	case 2:
		return symbols/4*3 + 1
	case 3:
		return symbols/4*3 + 2
	default:
		return 0
	}
}
