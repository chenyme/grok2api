package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestPreflightDeploymentCreatesAndValidatesSharedMediaMarker(t *testing.T) {
	directory := t.TempDir()
	cfg := config.Config{
		Deployment: config.DeploymentConfig{Replicas: 2, InstanceID: "replica-a", ClusterID: "cluster-a", SharedMedia: true},
		Media:      config.MediaConfig{Local: config.LocalMediaConfig{Path: directory}},
	}
	if err := preflightDeployment(cfg); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(directory, sharedMediaMarkerName))
	if err != nil || string(marker) != "cluster-a\n" {
		t.Fatalf("marker = %q, err = %v", marker, err)
	}
	cfg.Deployment.InstanceID = "replica-b"
	if err := preflightDeployment(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Deployment.ClusterID = "cluster-b"
	if err := preflightDeployment(cfg); err == nil {
		t.Fatal("expected cluster marker mismatch")
	}
}
