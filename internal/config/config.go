package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration sourced strictly from environment variables.
// The application performs a fail-fast validation at startup — any missing required
// variable causes an immediate, descriptive panic rather than a silent misconfiguration.
type Config struct {
	// Zerodha Kite Connect credentials
	KiteAPIKey      string
	KiteAPISecret   string
	KiteAccessToken string

	// Discord webhook for system/lifecycle alert notifications
	DiscordWebhookURL string

	// Discord Bot credentials for interactive signal messages (buttons + modals).
	// These are distinct from the webhook and require a Discord Application + Bot.
	DiscordBotToken      string
	DiscordApplicationID string
	DiscordPublicKey     string
	DiscordChannelID     string

	// Port on which the Discord interactions HTTP server listens.
	// Discord POSTs button and modal callbacks to this endpoint.
	InteractionsPort string

	// SQLite database file path
	DatabasePath string

	// Worker pool size for the tick processing pipeline
	WorkerPoolSize int

	// Tick channel buffer capacity
	TickChannelCapacity int

	// When true, INSTRUMENT_TOKENS is ignored and all NSE EQ instruments are
	// auto-discovered from the Zerodha API at startup.
	ScanFullNSE bool

	// When true, only Nifty 500 constituents are tracked (downloaded from NSE India
	// at startup and cross-referenced with the Zerodha instrument master).
	// Mutually exclusive with ScanFullNSE — highest priority.
	ScanNifty500 bool

	// Comma-separated list of instrument tokens to track (e.g., "256265,738561").
	// Only used when neither ScanNifty500 nor ScanFullNSE is true.
	InstrumentTokensRaw string
	InstrumentTokens    []uint32
}

// Load reads all required environment variables and returns a validated Config.
// It panics immediately with a descriptive message if any required variable is absent
// or malformed, enforcing the fail-fast principle at process startup.
func Load() *Config {
	scanNifty500 := strings.ToLower(strings.TrimSpace(os.Getenv("SCAN_NIFTY500"))) == "true"
	// FullNSE is only active when Nifty500 mode is off.
	scanFullNSE := !scanNifty500 && strings.ToLower(strings.TrimSpace(os.Getenv("SCAN_FULL_NSE"))) == "true"

	cfg := &Config{
		KiteAPIKey:           mustGetEnv("KITE_API_KEY"),
		KiteAPISecret:        os.Getenv("KITE_API_SECRET"), // optional: not currently used at runtime
		KiteAccessToken:      mustGetEnv("KITE_ACCESS_TOKEN"),
		DiscordWebhookURL:    mustGetEnv("DISCORD_WEBHOOK_URL"),
		DiscordBotToken:      mustGetEnv("DISCORD_BOT_TOKEN"),
		DiscordApplicationID: mustGetEnv("DISCORD_APPLICATION_ID"),
		DiscordPublicKey:     mustGetEnv("DISCORD_PUBLIC_KEY"),
		DiscordChannelID:     mustGetEnv("DISCORD_CHANNEL_ID"),
		InteractionsPort:     getEnvWithDefault("INTERACTIONS_PORT", "8080"),
		DatabasePath:         getEnvWithDefault("DATABASE_PATH", "market_data.db"),
		WorkerPoolSize:       getIntEnvWithDefault("WORKER_POOL_SIZE", 50),
		TickChannelCapacity:  getIntEnvWithDefault("TICK_CHANNEL_CAPACITY", 50000),
		ScanNifty500:         scanNifty500,
		ScanFullNSE:          scanFullNSE,
	}

	if !scanNifty500 && !scanFullNSE {
		// Manual mode: INSTRUMENT_TOKENS is required.
		cfg.InstrumentTokensRaw = mustGetEnv("INSTRUMENT_TOKENS")
		cfg.InstrumentTokens = parseTokens(cfg.InstrumentTokensRaw)
		if len(cfg.InstrumentTokens) == 0 {
			panic("config: INSTRUMENT_TOKENS must contain at least one valid uint32 token")
		}
	}

	return cfg
}

// mustGetEnv retrieves an environment variable and panics if it is not set or empty.
func mustGetEnv(key string) string {
	val := os.Getenv(key)
	if strings.TrimSpace(val) == "" {
		panic(fmt.Sprintf("config: required environment variable %q is not set", key))
	}
	return val
}

// getEnvWithDefault retrieves an environment variable, falling back to a default value.
func getEnvWithDefault(key, defaultVal string) string {
	val := os.Getenv(key)
	if strings.TrimSpace(val) == "" {
		return defaultVal
	}
	return val
}

// getIntEnvWithDefault retrieves an integer environment variable with a fallback default.
func getIntEnvWithDefault(key string, defaultVal int) int {
	raw := os.Getenv(key)
	if strings.TrimSpace(raw) == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		panic(fmt.Sprintf("config: environment variable %q must be a valid integer, got %q", key, raw))
	}
	if val <= 0 {
		panic(fmt.Sprintf("config: environment variable %q must be a positive integer, got %d", key, val))
	}
	return val
}

// parseTokens converts a comma-separated string of instrument tokens into a []uint32 slice.
func parseTokens(raw string) []uint32 {
	parts := strings.Split(raw, ",")
	tokens := make([]uint32, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		val, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			panic(fmt.Sprintf("config: invalid instrument token %q in INSTRUMENT_TOKENS: %v", p, err))
		}
		tokens = append(tokens, uint32(val))
	}
	return tokens
}
