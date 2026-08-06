// Package config reads the agent's settings from the environment.
package config

import (
	"fmt"
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
	AllowedOrigins  []string
	MaxTurns        int
	MaxTokens       int
	MaxToolRounds   int
	SessionTTL      time.Duration
	PerIPPerHour    int
	MaxConcurrent   int
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
		MaxTurns:      envInt("BARRAHOME_MAX_TURNS", 20),
		MaxTokens:     envInt("BARRAHOME_MAX_TOKENS", 1024),
		MaxToolRounds: envInt("BARRAHOME_MAX_TOOL_ROUNDS", 4),
		SessionTTL:    time.Duration(envInt("BARRAHOME_SESSION_TTL_MIN", 30)) * time.Minute,
		PerIPPerHour:  envInt("BARRAHOME_PER_IP_PER_HOUR", 20),
		MaxConcurrent: envInt("BARRAHOME_MAX_CONCURRENT", 10),
	}

	if cfg.MoonshotAPIKey == "" {
		return nil, fmt.Errorf("config: MOONSHOT_API_KEY is required")
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
