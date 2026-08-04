package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

type credentialJSONTestEntry struct {
	Token string `json:"token"`
}

// 计数型 entry：证明限额预检发生在任何元素解析之前（而非仅仅错误优先级靠前）。
type countedJSONEntry struct {
	Token string `json:"token"`
}

var countedJSONEntryUnmarshalCalls atomic.Int32

func (e *countedJSONEntry) UnmarshalJSON(data []byte) error {
	countedJSONEntryUnmarshalCalls.Add(1)
	type plain countedJSONEntry
	return json.Unmarshal(data, (*plain)(e))
}

func TestDecodeCredentialJSONEntriesAcceptsDocumentAndJSONLines(t *testing.T) {
	data := []byte("\xef\xbb\xbf{\n  \"provider\": \"grok_test\",\n  \"accounts\": [{\"token\":\"one\"}]\n}\r\n\r\n{\"token\":\"two\"}\r\n")
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry](data, "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Token != "one" || values[1].Token != "two" {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesReportsMalformedLineWithoutContent(t *testing.T) {
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte("{\"token\":\"safe\"}\n\nnot-json-secret\n"), "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "第 3 行") {
		t.Fatalf("error = %v, want line number", err)
	}
	if strings.Contains(err.Error(), "not-json-secret") || strings.Contains(err.Error(), "safe") {
		t.Fatalf("error exposes import contents: %v", err)
	}
}

func TestDecodeCredentialJSONEntriesRejectsNonObjects(t *testing.T) {
	for _, data := range []string{`"token"`, `null`} {
		if _, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(data), "grok_test", 10); err == nil || !strings.Contains(err.Error(), "必须是 JSON 对象") {
			t.Fatalf("data = %s, error = %v", data, err)
		}
	}
}

func TestDecodeCredentialJSONEntriesEnforcesLimitAcrossValues(t *testing.T) {
	data := []byte("{\"accounts\":[{\"token\":\"one\"}]}\n{\"token\":\"two\"}\n")
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry](data, "grok_test", 1)
	if !errors.Is(err, ErrCredentialLimit) {
		t.Fatalf("error = %v, want credential limit", err)
	}
}

func TestDecodeCredentialJSONEntriesRejectsWrongProvider(t *testing.T) {
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`{"provider":"other","token":"one"}`), "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "Provider 必须是 grok_test") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeCredentialJSONEntriesAcceptsBareArray(t *testing.T) {
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`[{"token":"one"},{"token":"two"}]`), "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Token != "one" || values[1].Token != "two" {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesAcceptsEmptyArray(t *testing.T) {
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte("[]\n"), "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("values = %#v", values)
	}

	values, err = DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte("[]\n{\"token\":\"one\"}"), "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].Token != "one" {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesMergesArrayAndJSONLinesInOrder(t *testing.T) {
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte("[{\"token\":\"one\"}]\n{\"token\":\"two\"}\n"), "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Token != "one" || values[1].Token != "two" {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesAcceptsConsecutiveArrays(t *testing.T) {
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte("[{\"token\":\"one\"}] [{\"token\":\"two\"}]"), "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Token != "one" || values[1].Token != "two" {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesAcceptsMultilineArrayAfterObject(t *testing.T) {
	data := []byte("{\"token\":\"zero\"}\n[\n  {\"token\": \"one\"},\n  {\"token\": \"two\"}\n]\n")
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry](data, "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 || values[0].Token != "zero" || values[1].Token != "one" || values[2].Token != "two" {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesAcceptsMultilineArrayWithBOMAndCRLF(t *testing.T) {
	data := []byte("\xef\xbb\xbf[\r\n  {\"token\":\"one\"},\r\n  {\"token\":\"two\"}\r\n]")
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry](data, "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Token != "one" || values[1].Token != "two" {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesRejectsArrayWithNonObjectElements(t *testing.T) {
	for _, data := range []string{`[42]`, `["token"]`, `[true]`, `[[{"token":"one"}]]`} {
		_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(data), "grok_test", 10)
		if err == nil || !strings.Contains(err.Error(), "第 1 个元素必须是 JSON 对象") {
			t.Fatalf("data = %s, error = %v", data, err)
		}
	}

	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`[{"token":"one"}, 42]`), "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "第 2 个元素必须是 JSON 对象") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeCredentialJSONEntriesRejectsArrayElementWithInvalidContent(t *testing.T) {
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`[{"token":123}]`), "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "第 1 个元素内容无效") {
		t.Fatalf("error = %v", err)
	}
}

func TestDecodeCredentialJSONEntriesArrayErrorsDoNotLeakSecrets(t *testing.T) {
	// 哨兵位于合法前序元素：错误文本不得捎带它。
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`[{"token":"s3cr3t-sentinel"}, 42]`), "grok_test", 10)
	if err == nil {
		t.Fatal("expected element error")
	}
	if strings.Contains(err.Error(), "s3cr3t-sentinel") {
		t.Fatalf("error leaks import contents: %v", err)
	}
	// 哨兵位于实际报错的元素内部（字段类型错误）：错误文本同样不得捎带。
	_, err = DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`[{"token":123,"note":"s3cr3t-sentinel"}]`), "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "内容无效") {
		t.Fatalf("error = %v, want invalid element content", err)
	}
	if strings.Contains(err.Error(), "s3cr3t-sentinel") {
		t.Fatalf("error leaks element contents: %v", err)
	}
}

func TestDecodeCredentialJSONEntriesPassesNullElementsThrough(t *testing.T) {
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`[null, {"token":"one"}]`), "grok_test", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Token != "" || values[1].Token != "one" {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesAllowsArrayExactlyAtLimit(t *testing.T) {
	values, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`[{"token":"one"},{"token":"two"}]`), "grok_test", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 {
		t.Fatalf("values = %#v", values)
	}
}

func TestDecodeCredentialJSONEntriesLimitTakesPriorityOverBadElements(t *testing.T) {
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte(`[{"token":"one"}, "bad-element"]`), "grok_test", 1)
	if !errors.Is(err, ErrCredentialLimit) {
		t.Fatalf("error = %v, want credential limit", err)
	}
}

// 限额预检必须发生在任何元素解析之前：超限数组中没有一个元素被 unmarshal。
func TestDecodeCredentialJSONEntriesLimitSkipsElementParsingEntirely(t *testing.T) {
	countedJSONEntryUnmarshalCalls.Store(0)
	_, err := DecodeCredentialJSONEntries[countedJSONEntry]([]byte(`[{"token":"one"},{"token":"two"}]`), "grok_test", 1)
	if !errors.Is(err, ErrCredentialLimit) {
		t.Fatalf("error = %v, want credential limit", err)
	}
	if calls := countedJSONEntryUnmarshalCalls.Load(); calls != 0 {
		t.Fatalf("element unmarshal ran %d times despite limit pre-check", calls)
	}
}

// 行号契约：数组元素错误报「数组起始行 + 元素序号」，不追求元素实际物理行。
func TestDecodeCredentialJSONEntriesReportsArrayStartLineWithElementIndex(t *testing.T) {
	data := []byte("{\"token\":\"zero\"}\n[\n  {\"token\":\"one\"},\n  42\n]\n")
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry](data, "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "第 2 行账号数组的第 2 个元素") {
		t.Fatalf("error = %v, want array start line with element index", err)
	}
}

func TestDecodeCredentialJSONEntriesReportsMalformedLineAfterArray(t *testing.T) {
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry]([]byte("[{\"token\":\"one\"}]\n\nnot-json\n"), "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "第 3 行") {
		t.Fatalf("error = %v, want line number", err)
	}
	if strings.Contains(err.Error(), "not-json") {
		t.Fatalf("error exposes import contents: %v", err)
	}
}

func TestDecodeCredentialJSONEntriesRejectsCommaLessMultilineArray(t *testing.T) {
	data := []byte("[\n{\"token\":\"one\"}\n{\"token\":\"two\"}\n]")
	_, err := DecodeCredentialJSONEntries[credentialJSONTestEntry](data, "grok_test", 10)
	if err == nil || !strings.Contains(err.Error(), "格式无效") {
		t.Fatalf("error = %v", err)
	}
}
