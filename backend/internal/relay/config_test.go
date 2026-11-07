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
