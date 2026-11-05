package admission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyConfigurationIsExplicitBoundedAndStrict(t *testing.T) {
	c := policy()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.yaml")
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadConfig(path)
	if err != nil || got != c {
		t.Fatal(got, err)
	}
	for _, data := range []string{"schema_version: 1\nschema_version: 2\n", "schema_version: !!python/object/apply:os.system []\n", `{"unexpected":true}`, string(data) + ` {}`} {
		_ = os.WriteFile(path, []byte(data), 0600)
		if _, err := ReadConfig(path); err == nil {
			t.Fatalf("invalid policy accepted: %s", data)
		}
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.ProtectRatio = c.HealthyRatio }, func(c *Config) { c.MinimumSamples = 0 }, func(c *Config) { c.MaxAgeSeconds = 1 }, func(c *Config) { c.RequestTimeoutSeconds = 60 }, func(c *Config) { c.Check = "../other" }} {
		bad := c
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal(bad)
		}
	}
}
