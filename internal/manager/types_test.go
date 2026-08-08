package manager

import (
	"strings"
	"testing"
)

func TestConfigValidateRejectsSharedServiceName(t *testing.T) {
	cfg := Config{
		CoreDir:        "/opt/proxyscene-config-test",
		InstallBin:     "/usr/local/bin/proxyscene",
		SystemdService: "same.service",
		RestoreService: "same.service",
	}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "不能使用同一个") {
		t.Fatalf("Validate() error = %v, want shared-unit rejection", err)
	}
}
