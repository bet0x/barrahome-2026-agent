// Package config reads the agent's settings from the environment.
package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable. The API key is read from the environment only —
// never a file in the repo, never a flag that would land in a process listing.
type Config struct {
	Workspace       string
	ListenPort      int
	MoonshotAPIKey  string
	MoonshotBaseURL string
	Model           string
	// Thinking is "enabled" or "disabled", sent only when Model supports it
	// (kimi-k2.x). Empty means the field is omitted from the request.
	Thinking string
	// ReasoningEffort is "low", "high" or "max", sent only when Model
	// supports it (kimi-k3). Empty means the field is omitted.
	ReasoningEffort string
	AllowedOrigins  []string
	MaxTurns        int
	MaxTokens       int
	MaxToolRounds   int
	SessionTTL      time.Duration
	PerIPPerHour    int
	MaxConcurrent   int
	// ShutdownTimeout bounds how long serve's HTTP shutdown waits for
	// in-flight streams to drain. supervise() derives the worker's kill
	// grace from this value, so raising it does not need a second, unrelated
	// constant kept in sync by hand.
	ShutdownTimeout time.Duration
}

// thinkingModels support kimi-k2.6's "thinking": {"type": ...} field.
// reasoningEffortModel supports kimi-k3's "reasoning_effort" enum. The two
// parameters are mutually exclusive across Moonshot's model lineup, so
// sending the wrong one for the configured model would 400 on every request.
func modelSupportsThinking(model string) bool {
	switch model {
	case "kimi-k2.6", "kimi-k2.5", "kimi-k2.7-code":
		return true
	}
	return false
}

func modelSupportsReasoningEffort(model string) bool {
	return model == "kimi-k3"
}

// Load builds a Config, applying defaults and validating what must be present.
func Load() (*Config, error) {
	cfg := &Config{
		Workspace:       env("BARRAHOME_WORKSPACE", "/workspace"),
		ListenPort:      envInt("BARRAHOME_PORT", 9000),
		MoonshotAPIKey:  os.Getenv("MOONSHOT_API_KEY"),
		MoonshotBaseURL: env("MOONSHOT_BASE_URL", "https://api.moonshot.ai/v1"),
		Model:           env("MOONSHOT_MODEL", "kimi-k2.6"),
		AllowedOrigins: strings.Split(env("BARRAHOME_ALLOWED_ORIGINS",
			"https://barrahome.org,https://www.barrahome.org"), ","),
		MaxTurns:        envInt("BARRAHOME_MAX_TURNS", 20),
		MaxTokens:       envInt("BARRAHOME_MAX_TOKENS", 1024),
		MaxToolRounds:   envInt("BARRAHOME_MAX_TOOL_ROUNDS", 3),
		SessionTTL:      time.Duration(envInt("BARRAHOME_SESSION_TTL_MIN", 30)) * time.Minute,
		PerIPPerHour:    envInt("BARRAHOME_PER_IP_PER_HOUR", 20),
		MaxConcurrent:   envInt("BARRAHOME_MAX_CONCURRENT", 10),
		ShutdownTimeout: time.Duration(envInt("BARRAHOME_SHUTDOWN_TIMEOUT_SEC", 30)) * time.Second,
	}

	if cfg.MoonshotAPIKey == "" {
		return nil, fmt.Errorf("config: MOONSHOT_API_KEY is required")
	}

	rawThinking := strings.TrimSpace(os.Getenv("MOONSHOT_THINKING"))
	if rawThinking != "" && rawThinking != "enabled" && rawThinking != "disabled" {
		return nil, fmt.Errorf(`config: MOONSHOT_THINKING must be "enabled" or "disabled", got %q`, rawThinking)
	}
	rawReasoning := strings.TrimSpace(os.Getenv("MOONSHOT_REASONING_EFFORT"))
	switch rawReasoning {
	case "", "low", "high", "max":
	default:
		return nil, fmt.Errorf(`config: MOONSHOT_REASONING_EFFORT must be "low", "high" or "max", got %q`, rawReasoning)
	}

	switch {
	case rawThinking != "" && !modelSupportsThinking(cfg.Model):
		log.Printf("config: MOONSHOT_THINKING=%q set but model %q does not support it; ignoring", rawThinking, cfg.Model)
	case rawThinking != "":
		cfg.Thinking = rawThinking
	case modelSupportsThinking(cfg.Model):
		cfg.Thinking = "disabled" // ships with thinking off; this workload needs summarizing, not reasoning
	}

	switch {
	case rawReasoning != "" && !modelSupportsReasoningEffort(cfg.Model):
		log.Printf("config: MOONSHOT_REASONING_EFFORT=%q set but model %q does not support it; ignoring", rawReasoning, cfg.Model)
	case rawReasoning != "":
		cfg.ReasoningEffort = rawReasoning
	}

	info, err := os.Stat(cfg.Workspace)
	if err != nil {
		return nil, fmt.Errorf("config: workspace %q: %w", cfg.Workspace, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("config: workspace %q is not a directory", cfg.Workspace)
	}

	for i, o := range cfg.AllowedOrigins {
		cfg.AllowedOrigins[i] = strings.TrimSpace(o)
	}
	return cfg, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return def
}
