package qualityguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestPrepareWritesPrivateScopedBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	value := config.QualityGuardConfig{
		Enabled: true, Model: "grok-4.5", NodeIDs: []uint64{2, 9}, Mode: "hybrid",
		ActiveInterval: config.Duration(30 * time.Minute), PassivePollInterval: config.Duration(5 * time.Second),
		SoftTPS: 500, HardTPS: 1000, ConsecutiveSoft: 2, ConsecutiveErrors: 2,
		QuarantineDuration: config.Duration(5 * time.Minute), NoAccountBackoff: config.Duration(5 * time.Minute),
		MinimumHealthyNodes: 1, MaxOutputTokens: 384, MinimumGenerationWindow: config.Duration(time.Second),
		RotationTimeout: config.Duration(45 * time.Second),
	}
	token, err := Prepare(path, value, "12345678901234567890123456789012")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected scoped token")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX permission bits: the os.WriteFile mode argument
	// is ignored and files always report 0666-style permissions.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	var payload bootstrapFile
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Enabled || payload.InternalToken != token || len(payload.Config.NodeIDs) != 2 || payload.Config.Prompt != ProbePrompt || payload.Config.Expected != ProbeExpected {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestPrepareRequiresPathOnlyWhenEnabled(t *testing.T) {
	if token, err := Prepare("", config.QualityGuardConfig{}, "secret"); err != nil || token != "" {
		t.Fatalf("disabled result = %q, %v", token, err)
	}
	if _, err := Prepare("", config.QualityGuardConfig{Enabled: true}, "secret"); err == nil {
		t.Fatal("expected enabled guard without bootstrap path to fail")
	}
}
