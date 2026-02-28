package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Profile holds credentials and settings for a named environment context.
type Profile struct {
	Environment  string `toml:"environment"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`
	AccessToken  string `toml:"access_token,omitempty"`
	TenantID     string `toml:"tenant_id"`
}

// Defaults holds global default settings.
type Defaults struct {
	PerPage int `toml:"per_page,omitempty"`
}

// Config is the top-level TOML config file structure.
type Config struct {
	DefaultProfile string             `toml:"default_profile"`
	Profiles       map[string]Profile `toml:"profiles"`
	Defaults       Defaults           `toml:"defaults"`
}

// configDir returns ~/.config/altscore, creating it if needed.
func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	dir := filepath.Join(home, ".config", "altscore")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("cannot create config directory: %w", err)
	}
	return dir, nil
}

// Path returns the path to the config file.
func Path() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// Load reads the config file. Returns an empty config if the file does not exist.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Profiles: make(map[string]Profile),
		Defaults: Defaults{PerPage: 100},
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read config: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("cannot parse config: %w", err)
	}

	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]Profile)
	}
	if cfg.Defaults.PerPage == 0 {
		cfg.Defaults.PerPage = 100
	}

	return cfg, nil
}

// Save writes the config back to disk atomically.
func Save(cfg *Config) error {
	path, err := Path()
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("cannot write config: %w", err)
	}

	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("cannot encode config: %w", err)
	}
	f.Close()

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot save config: %w", err)
	}
	return nil
}

// ResolveProfile determines which profile name to use, following priority:
// 1. Explicit flag value (flagProfile)
// 2. ALTSCORE_PROFILE env var
// 3. default_profile in config
// 4. "default"
func ResolveProfile(cfg *Config, flagProfile string) string {
	if flagProfile != "" {
		return flagProfile
	}
	if env := os.Getenv("ALTSCORE_PROFILE"); env != "" {
		return env
	}
	if cfg.DefaultProfile != "" {
		return cfg.DefaultProfile
	}
	return "default"
}

// GetProfile returns the resolved profile with environment variable overrides applied.
func GetProfile(cfg *Config, profileName string) Profile {
	p := cfg.Profiles[profileName]

	if v := os.Getenv("ALTSCORE_CLIENT_ID"); v != "" {
		p.ClientID = v
	}
	if v := os.Getenv("ALTSCORE_CLIENT_SECRET"); v != "" {
		p.ClientSecret = v
	}
	if v := os.Getenv("ALTSCORE_ENVIRONMENT"); v != "" {
		p.Environment = v
	}

	return p
}
