// Package config loads and validates the gateway's JSON configuration.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"

	"github.com/mtgban/mtgban-website/timeseries"
	"github.com/mtgban/simplecloud"
)

// gameName is the router's game segment pattern.
var gameName = regexp.MustCompile(`^[a-z0-9]+$`)

// Game is one backend the gateway can forward to.
type Game struct {
	Upstream string `json:"upstream"`
	Secret   string `json:"secret"`
}

// Config is the whole configuration file.
type Config struct {
	Port                   string                `json:"port"`
	InstanceName           string                `json:"instance_name"`
	Link                   string                `json:"link"`
	ClientIPHeader         string                `json:"client_ip_header"`
	GatewayEmail           string                `json:"gateway_email"`
	APIAccess              *timeseries.SQLConfig `json:"apiaccess_config"`
	Observability          *timeseries.SQLConfig `json:"observability_config"`
	DiscordHook            string                `json:"discord_api_notif_hook"`
	Games                  map[string]Game       `json:"games"`
	KnownStores            []string              `json:"known_stores"`
	CacheTTLSeconds        int                   `json:"cache_ttl_seconds"`
	StaleGraceSeconds      int                   `json:"stale_grace_seconds"`
	PerKeyRequestsPerSec   float64               `json:"per_key_requests_per_sec"`
	PerKeyBurst            int                   `json:"per_key_burst"`
	UpstreamTimeoutSeconds int                   `json:"upstream_timeout_seconds"`
	ShutdownGraceSeconds   int                   `json:"shutdown_grace_seconds"`
	UsageRetentionDays     int                   `json:"usage_retention_days"`
}

// DefaultClientIPHeader is the header DigitalOcean App Platform's ingress
// sets to the real client address.
const DefaultClientIPHeader = "DO-Connecting-IP"

// Load reads the config from path, or from $BAN_CONFIG_PATH when path is empty.
func Load(ctx context.Context, path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("BAN_CONFIG_PATH")
	}
	if path == "" {
		return nil, errors.New("no config path given; pass -config or set BAN_CONFIG_PATH")
	}
	r, err := simplecloud.Open(ctx, path,
		simplecloud.WithB2Credentials(os.Getenv("BAN_CONFIG_KEY"), os.Getenv("BAN_CONFIG_SECRET")))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	c, err := Parse(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes, applies defaults, and validates.
func Parse(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	// An explicit empty client_ip_header means trust only the peer address.
	var given struct {
		ClientIPHeader *string `json:"client_ip_header"`
	}
	_ = json.Unmarshal(data, &given)
	c.applyDefaults(given.ClientIPHeader == nil)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults(defaultClientIPHeader bool) {
	if defaultClientIPHeader {
		c.ClientIPHeader = DefaultClientIPHeader
	}
	if c.Port == "" {
		c.Port = "8080"
	}
	if c.InstanceName == "" {
		c.InstanceName = "api-gatewahy"
	}
	if c.Link == "" {
		c.Link = "http://www.mtgban.com"
	}
	if c.CacheTTLSeconds <= 0 {
		c.CacheTTLSeconds = 60
	}
	if c.StaleGraceSeconds <= 0 {
		c.StaleGraceSeconds = 600
	}
	if c.PerKeyRequestsPerSec <= 0 {
		c.PerKeyRequestsPerSec = 10
	}
	if c.PerKeyBurst <= 0 {
		c.PerKeyBurst = 5
	}
	if c.UpstreamTimeoutSeconds <= 0 {
		c.UpstreamTimeoutSeconds = 300
	}
	if c.ShutdownGraceSeconds <= 0 {
		c.ShutdownGraceSeconds = 60
	}
	if c.UsageRetentionDays <= 0 {
		c.UsageRetentionDays = 395
	}
}

// Validate reports the first configuration error, games in name order.
func (c *Config) Validate() error {
	if c.GatewayEmail == "" {
		return errors.New("gateway_email is required")
	}
	if c.APIAccess == nil {
		return errors.New("apiaccess_config is required")
	}
	if len(c.Games) == 0 {
		return errors.New("no games configured")
	}
	names := make([]string, 0, len(c.Games))
	for name := range c.Games {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		g := c.Games[name]
		// The router only ever matches this shape, so anything else is unreachable.
		if !gameName.MatchString(name) {
			return fmt.Errorf("games.%s: name must be lowercase letters and digits", name)
		}
		if g.Secret == "" {
			return fmt.Errorf("games.%s: empty secret", name)
		}
		u, err := url.Parse(g.Upstream)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("games.%s: upstream must be an absolute URL", name)
		}
	}
	return nil
}

// GameNames returns the configured games sorted by name.
func (c *Config) GameNames() []string {
	names := make([]string, 0, len(c.Games))
	for name := range c.Games {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
