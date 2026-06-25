package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

type Config struct {
	TelegramBotToken string `json:"telegram_bot_token"`
	GatewayURL       string `json:"gateway_url,omitempty"`
	BotID            string `json:"bot_id,omitempty"`
	OpenAIAPIKey     string `json:"openai_api_key,omitempty"`
}

// resolveHomeDir returns the user's home directory. It prefers os.UserHomeDir()
// (which reads $HOME) but falls back to the OS user database via user.Current()
// when $HOME is unset. launchd does not always seed $HOME into a job's environment,
// and without this fallback config loading fails with EX_CONFIG and the service
// crash-loops at boot until $HOME is manually added to the plist.
func resolveHomeDir() (string, error) {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home, nil
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir, nil
	}
	return "", fmt.Errorf("getting home directory: $HOME unset and user lookup failed")
}

func ConfigPath() (string, error) {
	home, err := resolveHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".telegram-claude-hero.json"), nil
}

func LoadConfig() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Env var override for gateway mode
	if envURL := os.Getenv("GATEWAY_URL"); envURL != "" {
		cfg.GatewayURL = envURL
	}

	// Env var override for bot ID (multi-tenant isolation)
	if envBotID := os.Getenv("BOT_ID"); envBotID != "" {
		cfg.BotID = envBotID
	}

	// Env var override for OpenAI API key
	if envKey := os.Getenv("OPENAI_API_KEY"); envKey != "" {
		cfg.OpenAIAPIKey = envKey
	}

	return &cfg, nil
}

func SaveConfig(cfg *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

func promptInput(prompt string) (string, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	val, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(val), nil
}
