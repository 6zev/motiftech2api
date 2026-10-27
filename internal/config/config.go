// Package config reads runtime settings from the environment on every call, so
// tests can override individual values without global state.
package config

import (
	"os"
	"strconv"
	"strings"
)

const (
	DefaultPort      = 3000
	DefaultHost      = "0.0.0.0"
	DefaultUpstream  = "https://chat.motiftech.io"
	DefaultTimeoutMS = 120_000
	DefaultTTLHours  = 168
	DefaultStorePath = "./.data/conversations.json"
)

// Config is the resolved runtime configuration.
type Config struct {
	Port                 int
	Host                 string
	UpstreamBase         string
	UpstreamTimeoutMS    int
	GatewayAPIKeys       []string
	ConversationTTLHours int
	StorePath            string
	Debug                bool
}

func str(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func integer(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// Load resolves the configuration from the environment.
func Load() Config {
	keys := []string{}
	for _, k := range strings.Split(str("GATEWAY_API_KEYS", ""), ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}

	return Config{
		Port:                 integer("PORT", DefaultPort),
		Host:                 str("HOST", DefaultHost),
		UpstreamBase:         strings.TrimRight(str("UPSTREAM_BASE", DefaultUpstream), "/"),
		UpstreamTimeoutMS:    integer("UPSTREAM_TIMEOUT_MS", DefaultTimeoutMS),
		GatewayAPIKeys:       keys,
		ConversationTTLHours: integer("CONVERSATION_TTL_HOURS", DefaultTTLHours),
		StorePath:            str("STORE_PATH", DefaultStorePath),
		Debug:                str("DEBUG", "0") == "1",
	}
}
