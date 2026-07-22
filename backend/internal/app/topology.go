package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

const sharedMediaMarkerName = ".grok2api-cluster"

// preflightDeployment validates the operator-declared shared media mount with a stable cluster marker and a read/write probe.
func preflightDeployment(cfg config.Config) error {
	if cfg.Deployment.Replicas <= 1 {
		return nil
	}
	directory := cfg.Media.Local.Path
	markerPath := filepath.Join(directory, sharedMediaMarkerName)
	want := strings.TrimSpace(cfg.Deployment.ClusterID) + "\n"
	current, err := os.ReadFile(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr == nil {
			if _, writeErr := file.WriteString(want); writeErr != nil {
				_ = file.Close()
				return fmt.Errorf("write shared media cluster marker: %w", writeErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				return fmt.Errorf("close shared media cluster marker: %w", closeErr)
			}
			current = []byte(want)
		} else if errors.Is(createErr, os.ErrExist) {
			current, err = os.ReadFile(markerPath)
			if err != nil {
				return fmt.Errorf("read shared media cluster marker: %w", err)
			}
		} else {
			return fmt.Errorf("create shared media cluster marker: %w", createErr)
		}
	} else if err != nil {
		return fmt.Errorf("read shared media cluster marker: %w", err)
	}
	if string(current) != want {
		return fmt.Errorf("shared media cluster marker mismatch: configured cluster %q does not own %s", cfg.Deployment.ClusterID, markerPath)
	}

	probe, err := os.CreateTemp(directory, ".grok2api-preflight-")
	if err != nil {
		return fmt.Errorf("create shared media preflight file: %w", err)
	}
	probePath := probe.Name()
	defer os.Remove(probePath)
	payload := []byte(strings.TrimSpace(cfg.Deployment.InstanceID))
	if _, err := probe.Write(payload); err != nil {
		_ = probe.Close()
		return fmt.Errorf("write shared media preflight file: %w", err)
	}
	if err := probe.Close(); err != nil {
		return fmt.Errorf("close shared media preflight file: %w", err)
	}
	readBack, err := os.ReadFile(probePath)
	if err != nil {
		return fmt.Errorf("read shared media preflight file: %w", err)
	}
	if string(readBack) != string(payload) {
		return errors.New("shared media preflight read-back mismatch")
	}
	return nil
}
