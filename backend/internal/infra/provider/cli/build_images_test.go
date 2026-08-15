package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"strings"
	"testing"
)

func TestNormalizeBuildInputGIFsConvertsFirstFrame(t *testing.T) {
	gifData := testAnimatedGIF(t)
	tests := []struct {
		name       string
		mediaType  string
		input      string
		imageParam string
	}{
		{
			name:       "declared gif in message content",
			mediaType:  "image/gif",
			input:      `[{"role":"user","content":[{"type":"input_image","image_url":"%s"}]}]`,
			imageParam: "content",
		},
		{
			name:       "gif magic mislabeled as png in tool output",
			mediaType:  "image/png",
			input:      `[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"%s"}]}]`,
			imageParam: "output",
		},
		{
			name:       "unpadded gif base64",
			mediaType:  "image/gif",
			input:      `[{"role":"user","content":[{"type":"input_image","image_url":"%s"}]}]`,
			imageParam: "content",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString(gifData)
			if test.name == "unpadded gif base64" {
				encoded = strings.TrimRight(encoded, "=")
			}
			dataURI := "data:" + test.mediaType + ";base64," + encoded
			body := []byte(`{"input":` + strings.Replace(test.input, "%s", dataURI, 1) + `}`)
			normalized, err := normalizeBuildRequest(body, "grok-4.6")
			if err != nil {
				t.Fatal(err)
			}
			imageURL := findTestImageURL(t, normalized, test.imageParam)
			if !strings.HasPrefix(imageURL, "data:image/png;base64,") {
				t.Fatalf("image_url = %.64q", imageURL)
			}
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(imageURL, "data:image/png;base64,"))
			if err != nil || !bytes.HasPrefix(decoded, []byte("\x89PNG\r\n\x1a\n")) {
				t.Fatalf("transcoded data is not PNG: %v", err)
			}
			frame, err := png.Decode(bytes.NewReader(decoded))
			if err != nil {
				t.Fatal(err)
			}
			if got := color.NRGBAModel.Convert(frame.At(0, 0)).(color.NRGBA); got.R != 255 || got.G != 0 || got.B != 0 || got.A != 255 {
				t.Fatalf("first-frame pixel = %#v, want opaque red", got)
			}
		})
	}
}

func TestNormalizeBuildInputGIFsPreservesNonGIFDataURI(t *testing.T) {
	value := "data:image/png;base64,iVBORw0KGgo="
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"` + value + `"}]}]}`)
	normalized, err := normalizeBuildRequest(body, "grok-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if got := findTestImageURL(t, normalized, "content"); got != value {
		t.Fatalf("image_url = %q, want unchanged %q", got, value)
	}
}

func TestNormalizeBuildInputGIFsRejectsDamagedGIF(t *testing.T) {
	value := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("GIF89a damaged"))
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"` + value + `"}]}]}`)
	_, err := normalizeBuildRequest(body, "grok-4.6")
	if err == nil {
		t.Fatal("damaged GIF was accepted")
	}
	requestError, ok := err.(*responsesRequestError)
	if !ok || requestError.Code != "invalid_image" || requestError.Param != "input[0].content[0].image_url" {
		t.Fatalf("error = %#v", err)
	}
}

func testAnimatedGIF(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.NRGBA{R: 255, A: 255}, color.NRGBA{B: 255, A: 255}}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	for index := range second.Pix {
		second.Pix[index] = 1
	}
	var output bytes.Buffer
	if err := gif.EncodeAll(&output, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func findTestImageURL(t *testing.T, body []byte, containerKey string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	items := payload["input"].([]any)
	blocks := items[0].(map[string]any)[containerKey].([]any)
	return blocks[0].(map[string]any)["image_url"].(string)
}
