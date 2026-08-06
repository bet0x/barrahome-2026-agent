package config

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"
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
	// 3, not 4: each round costs a full API round-trip (~2.3s TTFT measured),
	// and the graceful-exit fallback makes a lower ceiling safe to ship.
	if cfg.MaxToolRounds != 3 {
		t.Errorf("MaxToolRounds = %d, want 3", cfg.MaxToolRounds)
	}
	if len(cfg.AllowedOrigins) == 0 {
		t.Error("AllowedOrigins should have a default")
	}
	if cfg.Workspace != ws {
		t.Errorf("Workspace = %q, want %q", cfg.Workspace, ws)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 30s", cfg.ShutdownTimeout)
	}
}

func TestLoadAppliesShutdownTimeoutOverride(t *testing.T) {
	os.Clearenv()
	t.Setenv("MOONSHOT_API_KEY", "sk-placeholder")
	t.Setenv("BARRAHOME_WORKSPACE", t.TempDir())
	t.Setenv("BARRAHOME_SHUTDOWN_TIMEOUT_SEC", "45")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ShutdownTimeout != 45*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 45s", cfg.ShutdownTimeout)
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

// TestLoadDefaultsThinkingDisabledOnK26 guards the shipped default: the
// default model is kimi-k2.6, and thinking must default to disabled without
// the operator having to set anything.
func TestLoadDefaultsThinkingDisabledOnK26(t *testing.T) {
	os.Clearenv()
	t.Setenv("MOONSHOT_API_KEY", "sk-placeholder")
	t.Setenv("BARRAHOME_WORKSPACE", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Thinking != "disabled" {
		t.Errorf("Thinking = %q, want %q", cfg.Thinking, "disabled")
	}
	if cfg.ReasoningEffort != "" {
		t.Errorf("ReasoningEffort = %q, want empty (not applicable to k2.6)", cfg.ReasoningEffort)
	}
}

func TestLoadAcceptsThinkingEnabled(t *testing.T) {
	os.Clearenv()
	t.Setenv("MOONSHOT_API_KEY", "sk-placeholder")
	t.Setenv("BARRAHOME_WORKSPACE", t.TempDir())
	t.Setenv("MOONSHOT_THINKING", "enabled")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Thinking != "enabled" {
		t.Errorf("Thinking = %q, want %q", cfg.Thinking, "enabled")
	}
}

func TestLoadRejectsInvalidThinking(t *testing.T) {
	os.Clearenv()
	t.Setenv("MOONSHOT_API_KEY", "sk-placeholder")
	t.Setenv("BARRAHOME_WORKSPACE", t.TempDir())
	t.Setenv("MOONSHOT_THINKING", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("Load should fail on an unrecognised MOONSHOT_THINKING value")
	}
}

func TestLoadRejectsInvalidReasoningEffort(t *testing.T) {
	os.Clearenv()
	t.Setenv("MOONSHOT_API_KEY", "sk-placeholder")
	t.Setenv("BARRAHOME_WORKSPACE", t.TempDir())
	t.Setenv("MOONSHOT_MODEL", "kimi-k3")
	t.Setenv("MOONSHOT_REASONING_EFFORT", "medium")
	if _, err := Load(); err == nil {
		t.Fatal("Load should fail on an unrecognised MOONSHOT_REASONING_EFFORT value")
	}
}

func TestLoadAppliesReasoningEffortOnK3(t *testing.T) {
	os.Clearenv()
	t.Setenv("MOONSHOT_API_KEY", "sk-placeholder")
	t.Setenv("BARRAHOME_WORKSPACE", t.TempDir())
	t.Setenv("MOONSHOT_MODEL", "kimi-k3")
	t.Setenv("MOONSHOT_REASONING_EFFORT", "low")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReasoningEffort != "low" {
		t.Errorf("ReasoningEffort = %q, want %q", cfg.ReasoningEffort, "low")
	}
	if cfg.Thinking != "" {
		t.Errorf("Thinking = %q, want empty (not applicable to k3)", cfg.Thinking)
	}
}

// TestLoadIgnoresMismatchedKnobsAndWarns guards against a broken request
// shape: setting the k2.6 knob while running k3 (or vice versa) must not be
// sent upstream, and must be visible in the log rather than fail silently.
func TestLoadIgnoresMismatchedKnobsAndWarns(t *testing.T) {
	os.Clearenv()
	t.Setenv("MOONSHOT_API_KEY", "sk-placeholder")
	t.Setenv("BARRAHOME_WORKSPACE", t.TempDir())
	t.Setenv("MOONSHOT_MODEL", "kimi-k3")
	t.Setenv("MOONSHOT_THINKING", "enabled")

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Thinking != "" {
		t.Errorf("Thinking = %q, want empty: k3 does not support it", cfg.Thinking)
	}
	if !strings.Contains(logs.String(), "MOONSHOT_THINKING") {
		t.Errorf("log output = %q, want a warning naming MOONSHOT_THINKING", logs.String())
	}
}
