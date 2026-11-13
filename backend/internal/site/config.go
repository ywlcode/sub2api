// Package site implements the control plane between independently stored sites.
// It deliberately has no dependency on the gateway's protocol or billing code.
package site

import (
	"fmt"
	"net/url"
	"regexp"
	"time"
)

const (
	Standalone      = "standalone"
	Primary         = "primary"
	Secondary       = "secondary"
	SyncInterval    = 5 * time.Second
	ReplicaTTL      = time.Minute
	ProtocolVersion = 5
	SyncPath        = "/internal/sites/sync"
	DeliveryPath    = "/internal/sites/events"
)

type Config struct {
	Mode       string `mapstructure:"mode"`
	ID         string `mapstructure:"id"`
	PrimaryURL string `mapstructure:"primary_url"`
	Token      string `mapstructure:"token"`
	// Plain HTTP is permitted only by explicit operator configuration, e.g. a
	// private encrypted tunnel. Certificates are always verified for HTTPS.
	AllowHTTP bool   `mapstructure:"allow_http"`
	Peers     []Peer `mapstructure:"peers"`
}

type Peer struct {
	ID    string `mapstructure:"id" json:"id"`
	Token string `mapstructure:"token" json:"-"`
	// Account credentials remain maintained on the primary. An override changes
	// only the base URL used by this peer (e.g. the direct domestic provider URL).
	BaseURLs map[int64]string `mapstructure:"base_urls" json:"base_urls,omitempty"`
}

func (c Config) EffectiveMode() string {
	if c.Mode == "" {
		return Standalone
	}
	return c.Mode
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func (c Config) Validate() error {
	switch c.EffectiveMode() {
	case Standalone:
		if c.PrimaryURL != "" || c.Token != "" || len(c.Peers) != 0 {
			return fmt.Errorf("site: standalone cannot configure a primary or peers")
		}
	case Primary, Secondary:
		if !identifier.MatchString(c.ID) {
			return fmt.Errorf("site.id requires 1–64 lowercase identifier characters")
		}
	default:
		return fmt.Errorf("site.mode must be standalone, primary or secondary")
	}
	if c.EffectiveMode() == Secondary {
		if len(c.Token) < 32 {
			return fmt.Errorf("site.token must contain at least 32 characters")
		}
		if len(c.Peers) != 0 {
			return fmt.Errorf("secondary cannot have peers")
		}
		if err := validateOrigin(c.PrimaryURL, c.AllowHTTP); err != nil {
			return fmt.Errorf("site.primary_url: %w", err)
		}
	}
	if c.EffectiveMode() == Primary {
		if c.PrimaryURL != "" || c.Token != "" {
			return fmt.Errorf("primary cannot configure an upstream primary")
		}
		seen := map[string]bool{}
		tokens := map[string]bool{}
		for _, p := range c.Peers {
			if !identifier.MatchString(p.ID) || p.ID == c.ID || seen[p.ID] {
				return fmt.Errorf("site peer IDs must be valid and unique")
			}
			seen[p.ID] = true
			if len(p.Token) < 32 {
				return fmt.Errorf("site peer %s requires a token of at least 32 characters", p.ID)
			}
			if tokens[p.Token] {
				return fmt.Errorf("site peers require different tokens")
			}
			tokens[p.Token] = true
			for id, base := range p.BaseURLs {
				if id <= 0 {
					return fmt.Errorf("base URL override requires a positive account ID")
				}
				u, err := url.Parse(base)
				if err != nil || u.Host == "" || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
					return fmt.Errorf("site base URL overrides require HTTPS URLs")
				}
			}
		}
	}
	return nil
}

func validateOrigin(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && (!allowHTTP || u.Scheme != "http")) {
		return fmt.Errorf("expected an HTTPS origin without credentials, path or query")
	}
	return nil
}
