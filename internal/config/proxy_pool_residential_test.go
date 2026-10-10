package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResidentialSurvivesSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("# keep me\nport: 8317\nproxy-pool:\n  - id: a\n    name: A\n    proxy-url: socks5://u:p@1.1.1.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProxyPool[0].Residential = true
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "residential: true") {
		t.Fatalf("yaml lost the field:\n%s", raw)
	}
	again, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ProxyPool[0].Residential {
		t.Fatalf("reload lost the field:\n%s", raw)
	}
}
