// Package relay implements the stateless, single-upstream Ark relay.
package relay

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen                string
	StatsListen           string
	Key                   string
	AdditionalKeys        []string
	UpstreamKey           string
	UpstreamURL           string
	MaxBodyBytes          int64
	UploadTimeout         time.Duration
	ResponseHeaderTimeout time.Duration
}

func LoadConfig() (Config, error) {
	c := Config{Listen: "0.0.0.0:8080", StatsListen: "127.0.0.1:10800", UpstreamURL: "https://ark.cn-beijing.volces.com/api/v3",
		MaxBodyBytes: 64 << 20, UploadTimeout: 3 * time.Minute, ResponseHeaderTimeout: 5 * time.Minute}
	if v := os.Getenv("RELAY_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("RELAY_STATS_LISTEN"); v != "" {
		c.StatsListen = v
	}
	if v := os.Getenv("RELAY_UPSTREAM_URL"); v != "" {
		c.UpstreamURL = v
	}
	var err error
	if c.Key, err = loadSecret("RELAY_KEY"); err != nil {
		return c, err
	}
	if c.UpstreamKey, err = loadSecret("RELAY_UPSTREAM_KEY"); err != nil {
		return c, err
	}
	additional, err := loadSecret("RELAY_ADDITIONAL_KEYS")
	if err != nil {
		return c, err
	}
	for _, line := range strings.Split(additional, "\n") {
		if key := strings.TrimSpace(line); key != "" {
			c.AdditionalKeys = append(c.AdditionalKeys, key)
		}
	}
	if v := os.Getenv("RELAY_MAX_BODY_BYTES"); v != "" {
		c.MaxBodyBytes, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			return c, errors.New("invalid RELAY_MAX_BODY_BYTES")
		}
	}
	for name, target := range map[string]*time.Duration{"RELAY_UPLOAD_TIMEOUT": &c.UploadTimeout, "RELAY_RESPONSE_HEADER_TIMEOUT": &c.ResponseHeaderTimeout} {
		if v := os.Getenv(name); v != "" {
			*target, err = time.ParseDuration(v)
			if err != nil {
				return c, fmt.Errorf("invalid %s", name)
			}
		}
	}
	return c, c.Validate()
}

func loadSecret(name string) (string, error) {
	if path := os.Getenv(name + "_FILE"); path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // G703: secret path is operator-controlled startup configuration, never request input.
		if err != nil {
			return "", fmt.Errorf("read %s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}

func (c Config) Validate() error {
	if c.StatsListen != "" {
		_, port, err := net.SplitHostPort(c.StatsListen)
		n, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || n < 0 || n > 65535 {
			return errors.New("RELAY_STATS_LISTEN must be a host:port address")
		}
	}
	if c.Key == "" || c.UpstreamKey == "" {
		return errors.New("RELAY_KEY and RELAY_UPSTREAM_KEY are required (or their _FILE variants)")
	}
	if strings.ContainsAny(c.Key+c.UpstreamKey, "\r\n") {
		return errors.New("relay keys must be single-line values")
	}
	if len(c.AdditionalKeys) > 63 {
		return errors.New("at most 63 additional relay keys are supported")
	}
	for _, key := range c.AdditionalKeys {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
			return errors.New("additional relay keys must be nonempty single-line values")
		}
	}
	u, err := url.Parse(c.UpstreamURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("RELAY_UPSTREAM_URL must be an HTTP(S) base URL without credentials, query or fragment")
	}
	if c.MaxBodyBytes <= 0 || c.MaxBodyBytes > 1<<30 {
		return errors.New("RELAY_MAX_BODY_BYTES must be between 1 and 1073741824")
	}
	if c.UploadTimeout <= 0 || c.ResponseHeaderTimeout <= 0 {
		return errors.New("relay transport timeouts must be positive")
	}
	return nil
}
