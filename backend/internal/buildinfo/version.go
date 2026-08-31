package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
)

// Version 可在发布构建时通过 -ldflags -X 注入。
var Version string

// SourceCommit and BuildFingerprint are release-only values injected by the
// owned-fork image build. Development binaries remain runnable, but production
// attestation fails closed until both values and the immutable runtime image
// digest are present.
var SourceCommit string
var BuildFingerprint string

const runtimeImageDigestEnv = "GROK2API_RUNTIME_IMAGE_DIGEST"

const buildFingerprintDomain = "grok2api-owned-build-v1\x00"

var (
	sourceCommitPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256FingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	imageDigestPattern       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	versionPattern           = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)
)

type Attestation struct {
	SourceCommit       string `json:"sourceCommit"`
	RuntimeImageDigest string `json:"runtimeImageDigest"`
	BuildFingerprint   string `json:"buildFingerprint"`
}

// CurrentAttestation returns immutable release identity derived only from
// compile-time values and trusted process configuration. HTTP request input is
// deliberately absent from this contract.
func CurrentAttestation() (Attestation, error) {
	sourceCommit := SourceCommit
	buildFingerprint := BuildFingerprint
	runtimeImageDigest := os.Getenv(runtimeImageDigestEnv)
	if !sourceCommitPattern.MatchString(sourceCommit) {
		return Attestation{}, errors.New("release source commit attestation is unavailable")
	}
	if !versionPattern.MatchString(Version) || !sha256FingerprintPattern.MatchString(buildFingerprint) || buildFingerprint != expectedBuildFingerprint(sourceCommit, Version) {
		return Attestation{}, errors.New("release build fingerprint attestation is unavailable")
	}
	if !imageDigestPattern.MatchString(runtimeImageDigest) {
		return Attestation{}, errors.New("runtime image digest attestation is unavailable")
	}
	return Attestation{SourceCommit: sourceCommit, RuntimeImageDigest: runtimeImageDigest, BuildFingerprint: buildFingerprint}, nil
}

func expectedBuildFingerprint(sourceCommit, version string) string {
	sum := sha256.Sum256([]byte(buildFingerprintDomain + sourceCommit + "\x00" + version))
	return hex.EncodeToString(sum[:])
}

// CurrentVersion 返回当前运行实例的版本。源码运行优先读取仓库 VERSION，
// 容器和发行包可将 VERSION 放在可执行文件同目录。
func CurrentVersion() string {
	if value := cleanVersion(Version); value != "" {
		return value
	}
	if value := cleanVersion(os.Getenv("GROK2API_VERSION")); value != "" {
		return value
	}
	candidates := []string{"VERSION", filepath.Join("..", "VERSION")}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "VERSION"))
	}
	for _, candidate := range candidates {
		if data, err := os.ReadFile(candidate); err == nil {
			if value := cleanVersion(string(data)); value != "" {
				return value
			}
		}
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func cleanVersion(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return ""
	}
	return value
}
