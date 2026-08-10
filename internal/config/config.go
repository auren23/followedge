// Package config loads the YAML runtime configuration.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Mode  string `yaml:"mode"`
	Chain string `yaml:"chain"`

	DBPath string `yaml:"db_path"`

	GMGN GMGN `yaml:"gmgn"`

	Cluster struct {
		Windows []string `yaml:"windows"` // durations, e.g. ["30s","60s","5m","15m"]
	} `yaml:"cluster"`

	Markout struct {
		Tick       time.Duration `yaml:"tick"`
		Grace      time.Duration `yaml:"grace"` // wait this long past horizon before sampling
		Horizons   []string      `yaml:"horizons"`
		Resolution string        `yaml:"resolution"`
	} `yaml:"markout"`
}

type GMGN struct {
	BaseURL string `yaml:"base_url"`

	SmartMoney SourceConfig `yaml:"smart_money"`
	KOL        SourceConfig `yaml:"kol"`

	// Limiter is a token bucket measured in request weights per second;
	// GMGN's own bucket is rate=20 capacity=20, so staying below ~10 is safe.
	Limiter struct {
		WeightPerSecond float64 `yaml:"weight_per_second"`
		Burst           int     `yaml:"burst"`
	} `yaml:"limiter"`
}

type SourceConfig struct {
	Enabled      bool          `yaml:"enabled"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Limit        int           `yaml:"limit"` // page size, 1-200
}

// Defaults returns a conservative observe-mode config. GMGN documents a
// leaky bucket of rate=20/capacity=20 (RPS = 20 ÷ weight), but it bans the
// IP for minutes when exceeded and extends the ban by 5s per retry inside
// the cooldown — so we run well under the documented limit and rely on the
// shared cooldown gate to never touch a ban window.
func Defaults() *Config {
	c := &Config{
		Mode:   "observe",
		Chain:  "sol",
		DBPath: "data/followedge.db",
	}
	c.GMGN.BaseURL = "https://openapi.gmgn.ai"
	c.GMGN.SmartMoney = SourceConfig{Enabled: true, PollInterval: 1 * time.Second, Limit: 200}
	c.GMGN.KOL = SourceConfig{Enabled: true, PollInterval: 2 * time.Second, Limit: 200}
	c.GMGN.Limiter.WeightPerSecond = 3
	c.GMGN.Limiter.Burst = 6
	c.Cluster.Windows = []string{"30s", "60s", "5m", "15m"}
	c.Markout.Tick = 30 * time.Second
	c.Markout.Grace = 30 * time.Second
	c.Markout.Horizons = []string{"30s", "1m", "3m", "5m", "15m", "1h"}
	c.Markout.Resolution = "30s"
	return c
}

func Load(path string) (*Config, error) {
	c := Defaults()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return c, nil
}

// GMGNKey resolves the API key: $GMGN_API_KEY first, then the gmgn-cli config
// file (~/.config/gmgn/.env) so the CLI and FollowEdge share one key.
func GMGNKey() string {
	if k := os.Getenv("GMGN_API_KEY"); k != "" {
		return k
	}
	f := filepath.Join(os.Getenv("HOME"), ".config", "gmgn", ".env")
	fh, err := os.Open(f)
	if err != nil {
		return ""
	}
	defer fh.Close()
	s := bufio.NewScanner(fh)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "GMGN_API_KEY=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "GMGN_API_KEY="))
		}
	}
	return ""
}
