package qualityguard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

const bootstrapVersion = 1

const (
	ProbePrompt   = "Write exactly 16 numbered lines about reliable distributed systems. Each line must be one complete English sentence, with no markdown heading. The final line must end with the exact marker QUALITY_OK."
	ProbeExpected = "QUALITY_OK"
)

type bootstrapFile struct {
	Version       int             `json:"version"`
	Enabled       bool            `json:"enabled"`
	InternalToken string          `json:"internal_token,omitempty"`
	Config        bootstrapConfig `json:"config"`
}

type bootstrapConfig struct {
	Model                   string   `json:"model"`
	Prompt                  string   `json:"prompt"`
	Expected                string   `json:"expected"`
	NodeIDs                 []string `json:"node_ids"`
	Mode                    string   `json:"mode"`
	ActiveIntervalSeconds   int      `json:"active_interval_seconds"`
	PassivePollSeconds      int      `json:"passive_poll_seconds"`
	SoftTPS                 float64  `json:"soft_tps"`
	HardTPS                 float64  `json:"hard_tps"`
	ConsecutiveSoft         int      `json:"consecutive_soft"`
	ConsecutiveErrors       int      `json:"consecutive_errors"`
	QuarantineSeconds       int      `json:"quarantine_seconds"`
	NoAccountBackoffSeconds int      `json:"no_account_backoff_seconds"`
	MinHealthyNodes         int      `json:"min_healthy_nodes"`
	MaxOutputTokens         int      `json:"max_output_tokens"`
	FailClosed              bool     `json:"fail_closed"`
	MinGenerationMS         int      `json:"min_generation_ms"`
	RotationURL             string   `json:"rotation_url"`
	RotationToken           string   `json:"rotation_token"`
	RotationTimeoutSeconds  int      `json:"rotation_timeout_seconds"`
	RotatableNodeIDs        []string `json:"rotatable_node_ids"`
}

// Prepare writes the sidecar bootstrap file and returns the scoped internal
// token accepted by the HTTP server. The token is deterministically derived
// from jwtSecret with domain separation and is never exposed through an API.
func Prepare(path string, value config.QualityGuardConfig, jwtSecret string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		if value.Enabled {
			return "", errors.New("qualityGuard 已启用，但未配置内部 bootstrap 文件路径")
		}
		return "", nil
	}
	token := ""
	if value.Enabled {
		token = deriveToken(jwtSecret)
	}
	payload := bootstrapFile{
		Version: bootstrapVersion, Enabled: value.Enabled, InternalToken: token,
		Config: bootstrapConfig{
			Model:  strings.TrimSpace(value.Model),
			Prompt: ProbePrompt, Expected: ProbeExpected,
			NodeIDs: uint64Strings(value.NodeIDs), Mode: value.Mode,
			ActiveIntervalSeconds: int(value.ActiveInterval.Value().Seconds()), PassivePollSeconds: int(value.PassivePollInterval.Value().Seconds()),
			SoftTPS: value.SoftTPS, HardTPS: value.HardTPS, ConsecutiveSoft: value.ConsecutiveSoft, ConsecutiveErrors: value.ConsecutiveErrors,
			QuarantineSeconds: int(value.QuarantineDuration.Value().Seconds()), NoAccountBackoffSeconds: int(value.NoAccountBackoff.Value().Seconds()),
			MinHealthyNodes: value.MinimumHealthyNodes, MaxOutputTokens: value.MaxOutputTokens, FailClosed: value.FailClosed,
			MinGenerationMS: int(value.MinimumGenerationWindow.Value().Milliseconds()), RotationURL: strings.TrimSpace(value.RotationURL),
			RotationToken: value.RotationToken, RotationTimeoutSeconds: int(value.RotationTimeout.Value().Seconds()),
			RotatableNodeIDs: uint64Strings(value.RotatableNodeIDs),
		},
	}
	if err := writeAtomic(path, payload); err != nil {
		return "", fmt.Errorf("写入质量守护 bootstrap: %w", err)
	}
	return token, nil
}

func deriveToken(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("grok2api:quality-guard:internal:v1"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func uint64Strings(values []uint64) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != 0 {
			result = append(result, fmt.Sprintf("%d", value))
		}
	}
	return result
}

func writeAtomic(path string, value bootstrapFile) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".quality-guard-bootstrap-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
