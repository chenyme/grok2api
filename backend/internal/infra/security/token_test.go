package security

import (
	"testing"
	"time"
)

func TestClientKeyFormat(t *testing.T) {
	raw := FormatClientKey("abc123", "secret_value")
	if raw != "g2a_abc123_secret_value" {
		t.Fatalf("formatted key = %q", raw)
	}
	prefix, ok := SplitClientKey(raw)
	if !ok || prefix != "abc123" {
		t.Fatalf("SplitClientKey(%q) = %q, %v", raw, prefix, ok)
	}
	for _, value := range []string{"", "g2a_", "g2a__secret", "other_abc123_secret", "gbp_abc123_old_secret"} {
		if _, ok := SplitClientKey(value); ok {
			t.Fatalf("SplitClientKey(%q) unexpectedly succeeded", value)
		}
	}
}

func TestVideoPreviewTokenIsShortLivedAndJobScoped(t *testing.T) {
	service := NewTokenService("12345678901234567890123456789012")
	ticket, err := service.CreateVideoPreviewToken("job_123", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ParseVideoPreviewToken(ticket, "job_123"); err != nil {
		t.Fatalf("valid ticket rejected: %v", err)
	}
	if err := service.ParseVideoPreviewToken(ticket, "job_other"); err == nil {
		t.Fatal("ticket was accepted for another job")
	}
	if _, err := service.ParseAccessToken(ticket); err == nil {
		t.Fatal("preview ticket was accepted as an admin access token")
	}

	accessToken, _, err := service.CreateAccessToken(1, 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ParseVideoPreviewToken(accessToken, "job_123"); err == nil {
		t.Fatal("admin access token was accepted as a preview ticket")
	}

	expired, err := service.CreateVideoPreviewToken("job_123", -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ParseVideoPreviewToken(expired, "job_123"); err == nil {
		t.Fatal("expired preview ticket was accepted")
	}
}
