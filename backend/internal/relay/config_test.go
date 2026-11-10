package relay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAdditionalFixedKeys(t *testing.T) {
	t.Setenv("RELAY_KEY_FILE", "")
	t.Setenv("RELAY_KEY", "primary-fixture")
	t.Setenv("RELAY_UPSTREAM_KEY_FILE", "")
	t.Setenv("RELAY_UPSTREAM_KEY", "upstream-fixture")
	t.Setenv("RELAY_ADDITIONAL_KEYS", "ignored-fixture")
	path := filepath.Join(t.TempDir(), "keys")
	if err := os.WriteFile(path, []byte("ecos-fixture\r\n\nthird-fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAY_ADDITIONAL_KEYS_FILE", path)
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AdditionalKeys) != 2 || c.AdditionalKeys[0] != "ecos-fixture" || c.AdditionalKeys[1] != "third-fixture" {
		t.Fatal("additional key file was not parsed correctly")
	}
	c.AdditionalKeys = []string{""}
	if c.Validate() == nil {
		t.Fatal("empty additional key accepted")
	}
	c.AdditionalKeys = []string{"key\ninjected"}
	if c.Validate() == nil {
		t.Fatal("multiline additional key accepted")
	}
	c.AdditionalKeys = make([]string, 64)
	if c.Validate() == nil {
		t.Fatal("unbounded additional key list accepted")
	}
	t.Setenv("RELAY_ADDITIONAL_KEYS_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := LoadConfig(); err == nil {
		t.Fatal("missing configured key file was ignored")
	}
}

func TestStatsListenerConfiguration(t *testing.T) {
	t.Setenv("RELAY_KEY_FILE", "")
	t.Setenv("RELAY_KEY", "relay-fixture")
	t.Setenv("RELAY_UPSTREAM_KEY_FILE", "")
	t.Setenv("RELAY_UPSTREAM_KEY", "upstream-fixture")
	t.Setenv("RELAY_ADDITIONAL_KEYS_FILE", "")
	t.Setenv("RELAY_ADDITIONAL_KEYS", "")
	t.Setenv("RELAY_STATS_LISTEN", "")
	config, err := LoadConfig()
	if err != nil || config.StatsListen != "127.0.0.1:10800" {
		t.Fatalf("incorrect default stats listener: %+v, %v", config, err)
	}
	t.Setenv("RELAY_STATS_LISTEN", "0.0.0.0:10800")
	config, err = LoadConfig()
	if err != nil || config.StatsListen != "0.0.0.0:10800" {
		t.Fatal("container stats listener override ignored")
	}
	t.Setenv("RELAY_STATS_LISTEN", "invalid")
	if _, err = LoadConfig(); err == nil {
		t.Fatal("invalid stats listener accepted")
	}
}

func TestLegacyCompatibilitySettingIsIgnored(t *testing.T) {
	t.Setenv("RELAY_KEY_FILE", "")
	t.Setenv("RELAY_KEY", "relay-fixture")
	t.Setenv("RELAY_UPSTREAM_KEY_FILE", "")
	t.Setenv("RELAY_UPSTREAM_KEY", "upstream-fixture")
	t.Setenv("RELAY_ADDITIONAL_KEYS_FILE", "")
	t.Setenv("RELAY_ADDITIONAL_KEYS", "")
	for _, value := range []string{"true", "false", "invalid"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("RELAY_VOLCENGINE_COMPATIBILITY", value)
			if _, err := LoadConfig(); err != nil {
				t.Fatalf("removed compatibility setting affected startup: %v", err)
			}
		})
	}
}
