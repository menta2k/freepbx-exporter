// Package config loads exporter configuration from flags + environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved runtime configuration.
type Config struct {
	ListenAddress string
	MetricsPath   string

	AMIAddress  string
	AMIUsername string
	AMISecret   string
	AMITimeout  time.Duration

	DisableSIP    bool
	DisablePJSIP  bool
	DisableQueues bool

	EnableEvents   bool // run a persistent AMI event-stream consumer
	RTCPPerChannel bool // emit per-channel packet_loss_ratio (high cardinality)

	LogLevel  string // debug|info|warn|error
	LogFormat string // text|json
}

// Defaults are applied where neither flag nor env set a value.
var Defaults = Config{
	ListenAddress:  ":9810",
	MetricsPath:    "/metrics",
	AMIAddress:     "127.0.0.1:5038",
	AMIUsername:    "",
	AMISecret:      "",
	AMITimeout:     10 * time.Second,
	EnableEvents:   true,
	RTCPPerChannel: false,
	LogLevel:       "info",
	LogFormat:      "text",
}

// Load parses argv (excluding program name) and returns a validated Config.
// Environment variables (FREEPBX_EXPORTER_*) provide defaults that flags
// override.
func Load(args []string) (Config, error) {
	cfg := Defaults
	applyEnv(&cfg)

	fs := flag.NewFlagSet("freepbx-exporter", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&cfg.ListenAddress, "web.listen-address", cfg.ListenAddress, "Address on which to expose metrics.")
	fs.StringVar(&cfg.MetricsPath, "web.telemetry-path", cfg.MetricsPath, "URL path under which to expose metrics.")
	fs.StringVar(&cfg.AMIAddress, "ami.address", cfg.AMIAddress, "Asterisk Manager Interface host:port.")
	fs.StringVar(&cfg.AMIUsername, "ami.username", cfg.AMIUsername, "AMI manager username.")
	fs.StringVar(&cfg.AMISecret, "ami.secret", cfg.AMISecret, "AMI manager secret. Prefer FREEPBX_EXPORTER_AMI_SECRET env var.")
	fs.DurationVar(&cfg.AMITimeout, "ami.timeout", cfg.AMITimeout, "Timeout for AMI dial/login/action.")
	fs.BoolVar(&cfg.DisableSIP, "no-sip", cfg.DisableSIP, "Disable chan_sip peer scraping.")
	fs.BoolVar(&cfg.DisablePJSIP, "no-pjsip", cfg.DisablePJSIP, "Disable PJSIP endpoint scraping.")
	fs.BoolVar(&cfg.DisableQueues, "no-queues", cfg.DisableQueues, "Disable queue scraping.")
	fs.BoolVar(&cfg.EnableEvents, "enable-events", cfg.EnableEvents, "Run a persistent AMI event-stream consumer for call-quality metrics.")
	fs.BoolVar(&cfg.RTCPPerChannel, "rtcp-per-channel", cfg.RTCPPerChannel, "Emit per-channel asterisk_rtcp_packet_loss_ratio (high cardinality).")
	fs.StringVar(&cfg.LogLevel, "log.level", cfg.LogLevel, "Log level (debug|info|warn|error).")
	fs.StringVar(&cfg.LogFormat, "log.format", cfg.LogFormat, "Log format (text|json).")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.AMIAddress == "" {
		return errors.New("ami.address is required")
	}
	if c.AMIUsername == "" {
		return errors.New("ami.username is required (or FREEPBX_EXPORTER_AMI_USERNAME)")
	}
	if c.AMISecret == "" {
		return errors.New("ami.secret is required (or FREEPBX_EXPORTER_AMI_SECRET)")
	}
	if c.AMITimeout <= 0 {
		return errors.New("ami.timeout must be positive")
	}
	if !strings.HasPrefix(c.MetricsPath, "/") {
		return fmt.Errorf("web.telemetry-path %q must start with /", c.MetricsPath)
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q invalid", c.LogLevel)
	}
	switch strings.ToLower(c.LogFormat) {
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q invalid", c.LogFormat)
	}
	return nil
}

// Redact returns the config with secrets replaced. Suitable for logging.
func (c Config) Redact() Config {
	if c.AMISecret != "" {
		c.AMISecret = "***"
	}
	return c
}

func applyEnv(c *Config) {
	if v := os.Getenv("FREEPBX_EXPORTER_LISTEN"); v != "" {
		c.ListenAddress = v
	}
	if v := os.Getenv("FREEPBX_EXPORTER_METRICS_PATH"); v != "" {
		c.MetricsPath = v
	}
	if v := os.Getenv("FREEPBX_EXPORTER_AMI_ADDRESS"); v != "" {
		c.AMIAddress = v
	}
	if v := os.Getenv("FREEPBX_EXPORTER_AMI_USERNAME"); v != "" {
		c.AMIUsername = v
	}
	if v := os.Getenv("FREEPBX_EXPORTER_AMI_SECRET"); v != "" {
		c.AMISecret = v
	}
	if v := os.Getenv("FREEPBX_EXPORTER_AMI_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.AMITimeout = d
		}
	}
	c.DisableSIP = parseBoolEnv("FREEPBX_EXPORTER_NO_SIP", c.DisableSIP)
	c.DisablePJSIP = parseBoolEnv("FREEPBX_EXPORTER_NO_PJSIP", c.DisablePJSIP)
	c.DisableQueues = parseBoolEnv("FREEPBX_EXPORTER_NO_QUEUES", c.DisableQueues)
	c.EnableEvents = parseBoolEnv("FREEPBX_EXPORTER_ENABLE_EVENTS", c.EnableEvents)
	c.RTCPPerChannel = parseBoolEnv("FREEPBX_EXPORTER_RTCP_PER_CHANNEL", c.RTCPPerChannel)
	if v := os.Getenv("FREEPBX_EXPORTER_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("FREEPBX_EXPORTER_LOG_FORMAT"); v != "" {
		c.LogFormat = v
	}
}

func parseBoolEnv(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}
