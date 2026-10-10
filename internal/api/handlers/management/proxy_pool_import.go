package management

import (
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

// maxProxyImportLines bounds one import so a pasted file cannot grow the config without limit.
const maxProxyImportLines = 5000

type proxyImportInvalid struct {
	Line  int    `json:"line"`
	Error string `json:"error"`
}

type proxyImportReport struct {
	Added     int                  `json:"added"`
	Duplicate int                  `json:"duplicate"`
	Invalid   []proxyImportInvalid `json:"invalid,omitempty"`
}

// parseProxyImport turns a block of text, one proxy per line, into pool entries. Blank lines and
// lines starting with "#" are skipped. Each address goes through config.NormalizeProxyPoolURL, so a
// bare "user:pass@host:port" becomes socks5; IPv6 hosts must be bracketed ("[2001:db8::1]:1080").
// seen holds proxy URLs that already exist and is extended with every accepted one, so duplicates
// inside the text and against the pool are both reported as duplicates.
func parseProxyImport(text, namePrefix string, residential bool, seen map[string]struct{}) (entries []config.ProxyPoolEntry, rep proxyImportReport) {
	namePrefix = strings.TrimSpace(namePrefix)
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proxyURL := config.NormalizeProxyPoolURL(line)
		setting, err := proxyutil.Parse(proxyURL)
		if err != nil || setting.Mode != proxyutil.ModeProxy || setting.URL.Port() == "" {
			msg := "invalid proxy address (IPv6 hosts need brackets, e.g. user:pass@[2001:db8::1]:1080)"
			if err == nil && setting.Mode == proxyutil.ModeProxy {
				msg = "missing port"
			}
			rep.Invalid = append(rep.Invalid, proxyImportInvalid{Line: i + 1, Error: msg})
			continue
		}
		if _, dup := seen[proxyURL]; dup {
			rep.Duplicate++
			continue
		}
		seen[proxyURL] = struct{}{}
		// The label never contains the credentials: host:port only.
		name := net.JoinHostPort(setting.URL.Hostname(), setting.URL.Port())
		if namePrefix != "" {
			name = namePrefix + " " + name
		}
		entries = append(entries, config.ProxyPoolEntry{Name: name, ProxyURL: proxyURL, Residential: residential})
		rep.Added++
	}
	return entries, rep
}

// ImportProxyPool appends proxies from text, one per line ({"text": "...", "name-prefix": "..."}).
// Existing entries are kept; addresses already in the pool are skipped. Invalid lines are reported
// with their line number and do not stop the rest from being imported.
func (h *Handler) ImportProxyPool(c *gin.Context) {
	var body struct {
		Text       string `json:"text"`
		NamePrefix string `json:"name-prefix"`
		// Residential tags every imported entry as a residential IP.
		Residential bool `json:"residential"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text is required"})
		return
	}
	if strings.Count(body.Text, "\n") >= maxProxyImportLines {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "too many lines"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	seen := make(map[string]struct{}, len(h.cfg.ProxyPool))
	for _, entry := range h.cfg.ProxyPool {
		seen[strings.TrimSpace(entry.ProxyURL)] = struct{}{}
	}
	entries, rep := parseProxyImport(body.Text, body.NamePrefix, body.Residential, seen)
	if len(entries) == 0 {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "report": rep})
		return
	}
	h.cfg.ProxyPool = append(h.cfg.ProxyPool, entries...)
	h.cfg.SanitizeProxyPool()
	h.persistLockedWithBody(c, gin.H{"status": "ok", "report": rep})
}
