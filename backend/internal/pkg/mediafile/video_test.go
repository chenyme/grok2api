package mediafile

import "testing"

func TestVideoExtension(t *testing.T) {
	for _, testCase := range []struct {
		contentType string
		want        string
		ok          bool
	}{
		{contentType: "video/mp4", want: ".mp4", ok: true},
		{contentType: "video/webm; codecs=vp9", want: ".webm", ok: true},
		{contentType: "VIDEO/QUICKTIME", want: ".mov", ok: true},
		{contentType: "application/octet-stream"},
		{contentType: "video/mp4; invalid"},
	} {
		got, ok := VideoExtension(testCase.contentType)
		if got != testCase.want || ok != testCase.ok {
			t.Errorf("VideoExtension(%q) = %q, %v; want %q, %v", testCase.contentType, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestVideoContentDispositionSanitizesName(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		contentType string
		want        string
	}{
		{name: `bad"name; drop`, contentType: "video/mp4", want: `inline; filename="badnamedrop.mp4"`},
		{name: "   ", contentType: "video/mp4", want: `inline; filename="video.mp4"`},
		{name: "video_request_1", contentType: "application/octet-stream", want: `inline; filename="video_request_1"`},
	} {
		if got := VideoContentDisposition(testCase.name, testCase.contentType); got != testCase.want {
			t.Errorf("VideoContentDisposition(%q, %q) = %q, want %q", testCase.name, testCase.contentType, got, testCase.want)
		}
	}
}
