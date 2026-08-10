package config

import "testing"

func TestSanitizeProxyPool_DropsEntriesWithoutProxyURL(t *testing.T) {
	cfg := &Config{
		ProxyPool: []ProxyPoolEntry{
			{ID: "keep", Name: "Keep", ProxyURL: "socks5://1.2.3.4:1080"},
			{ID: "drop", Name: "Drop", ProxyURL: "   "},
		},
	}

	cfg.SanitizeProxyPool()

	if len(cfg.ProxyPool) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(cfg.ProxyPool), cfg.ProxyPool)
	}
	if cfg.ProxyPool[0].ID != "keep" {
		t.Fatalf("expected surviving entry id 'keep', got %q", cfg.ProxyPool[0].ID)
	}
}

func TestSanitizeProxyPool_AssignsMissingID(t *testing.T) {
	cfg := &Config{
		ProxyPool: []ProxyPoolEntry{
			{Name: "No ID", ProxyURL: "socks5://1.2.3.4:1080"},
		},
	}

	cfg.SanitizeProxyPool()

	if len(cfg.ProxyPool) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(cfg.ProxyPool))
	}
	if cfg.ProxyPool[0].ID == "" {
		t.Fatalf("expected auto-generated id, got empty string")
	}
}

func TestSanitizeProxyPool_DedupesByID_LastWins(t *testing.T) {
	cfg := &Config{
		ProxyPool: []ProxyPoolEntry{
			{ID: "dup", Name: "First", ProxyURL: "socks5://1.1.1.1:1080"},
			{ID: "dup", Name: "Second", ProxyURL: "socks5://2.2.2.2:1080"},
		},
	}

	cfg.SanitizeProxyPool()

	if len(cfg.ProxyPool) != 1 {
		t.Fatalf("expected 1 deduped entry, got %d: %+v", len(cfg.ProxyPool), cfg.ProxyPool)
	}
	if cfg.ProxyPool[0].Name != "Second" || cfg.ProxyPool[0].ProxyURL != "socks5://2.2.2.2:1080" {
		t.Fatalf("expected last entry to win, got %+v", cfg.ProxyPool[0])
	}
}

func TestSanitizeProxyPool_TrimsFields(t *testing.T) {
	cfg := &Config{
		ProxyPool: []ProxyPoolEntry{
			{
				ID:       " id-1 ",
				Name:     " Name ",
				ProxyURL: " socks5://1.2.3.4:1080 ",
				Account:  " acct ",
				ExitIP:   " 1.2.3.4 ",
				Notes:    " note ",
			},
		},
	}

	cfg.SanitizeProxyPool()

	if len(cfg.ProxyPool) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(cfg.ProxyPool))
	}
	entry := cfg.ProxyPool[0]
	if entry.ID != "id-1" || entry.Name != "Name" || entry.ProxyURL != "socks5://1.2.3.4:1080" ||
		entry.Account != "acct" || entry.ExitIP != "1.2.3.4" || entry.Notes != "note" {
		t.Fatalf("expected trimmed fields, got %+v", entry)
	}
}

func TestSanitizeProxyPool_NilConfigNoop(t *testing.T) {
	var cfg *Config
	cfg.SanitizeProxyPool()
}
