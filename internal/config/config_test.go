package config

import (
	"os"
	"testing"
)

func TestLoadRequiresAPIKey(t *testing.T) {
	os.Clearenv()
	t.Setenv("BARRAHOME_WORKSPACE", t.TempDir())
	if _, err := Load(); err == nil {
		t.Fatal("Load should fail without MOONSHOT_API_KEY")
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	os.Clearenv()
	ws := t.TempDir()
	t.Setenv("MOONSHOT_API_KEY", "sk-placeholder")
	t.Setenv("BARRAHOME_WORKSPACE", ws)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "kimi-k2.6" {
		t.Errorf("Model = %q, want kimi-k2.6", cfg.Model)
	}
	if cfg.ListenPort != 9000 {
		t.Errorf("ListenPort = %d, want 9000", cfg.ListenPort)
	}
	if cfg.MaxTurns != 20 {
		t.Errorf("MaxTurns = %d, want 20", cfg.MaxTurns)
	}
	if len(cfg.AllowedOrigins) == 0 {
		t.Error("AllowedOrigins should have a default")
	}
	if cfg.Workspace != ws {
		t.Errorf("Workspace = %q, want %q", cfg.Workspace, ws)
	}
}

func TestLoadRejectsMissingWorkspace(t *testing.T) {
	os.Clearenv()
	t.Setenv("MOONSHOT_API_KEY", "sk-placeholder")
	t.Setenv("BARRAHOME_WORKSPACE", "/nonexistent-workspace-xyz")
	if _, err := Load(); err == nil {
		t.Fatal("Load should fail when the workspace does not exist")
	}
}
