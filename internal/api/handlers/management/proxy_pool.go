package management

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// GetProxyPool returns proxy pool entries. An optional "q" query parameter fuzzy-matches
// (case-insensitive substring) against name, account, exit-ip, and notes.
func (h *Handler) GetProxyPool(c *gin.Context) {
	h.mu.Lock()
	entries := append([]config.ProxyPoolEntry(nil), h.cfg.ProxyPool...)
	h.mu.Unlock()

	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q == "" {
		c.JSON(200, gin.H{"proxy-pool": entries})
		return
	}
	filtered := make([]config.ProxyPoolEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.Contains(strings.ToLower(entry.Name), q) ||
			strings.Contains(strings.ToLower(entry.Account), q) ||
			strings.Contains(strings.ToLower(entry.ExitIP), q) ||
			strings.Contains(strings.ToLower(entry.Notes), q) {
			filtered = append(filtered, entry)
		}
	}
	c.JSON(200, gin.H{"proxy-pool": filtered})
}

// PutProxyPool replaces the entire proxy pool list. Accepts either a bare array or
// {"items": [...]}.
func (h *Handler) PutProxyPool(c *gin.Context) {
	data, err := c.GetRawData()
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	var arr []config.ProxyPoolEntry
	if err = json.Unmarshal(data, &arr); err != nil {
		var obj struct {
			Items []config.ProxyPoolEntry `json:"items"`
		}
		if err2 := json.Unmarshal(data, &obj); err2 != nil || len(obj.Items) == 0 {
			c.JSON(400, gin.H{"error": "invalid body"})
			return
		}
		arr = obj.Items
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.ProxyPool = append([]config.ProxyPoolEntry(nil), arr...)
	h.cfg.SanitizeProxyPool()
	h.persistLocked(c)
}

type proxyPoolEntryPatch struct {
	Name     *string `json:"name"`
	ProxyURL *string `json:"proxy-url"`
	Account  *string `json:"account"`
	ExitIP   *string `json:"exit-ip"`
	Notes    *string `json:"notes"`
}

// PatchProxyPoolEntry updates a single proxy pool entry, located by "id" (preferred) or
// "index". A blank "proxy-url" in the patch value removes the entry.
func (h *Handler) PatchProxyPoolEntry(c *gin.Context) {
	var body struct {
		ID    *string              `json:"id"`
		Index *int                 `json:"index"`
		Value *proxyPoolEntryPatch `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(400, gin.H{"error": "invalid body"})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	targetIndex := -1
	if body.ID != nil {
		id := strings.TrimSpace(*body.ID)
		if id != "" {
			for i := range h.cfg.ProxyPool {
				if h.cfg.ProxyPool[i].ID == id {
					targetIndex = i
					break
				}
			}
		}
	}
	if targetIndex == -1 && body.Index != nil && *body.Index >= 0 && *body.Index < len(h.cfg.ProxyPool) {
		targetIndex = *body.Index
	}
	if targetIndex == -1 {
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}

	entry := h.cfg.ProxyPool[targetIndex]
	if body.Value.ProxyURL != nil {
		trimmed := strings.TrimSpace(*body.Value.ProxyURL)
		if trimmed == "" {
			h.cfg.ProxyPool = append(h.cfg.ProxyPool[:targetIndex], h.cfg.ProxyPool[targetIndex+1:]...)
			h.cfg.SanitizeProxyPool()
			h.persistLocked(c)
			return
		}
		entry.ProxyURL = trimmed
	}
	if body.Value.Name != nil {
		entry.Name = strings.TrimSpace(*body.Value.Name)
	}
	if body.Value.Account != nil {
		entry.Account = strings.TrimSpace(*body.Value.Account)
	}
	if body.Value.ExitIP != nil {
		entry.ExitIP = strings.TrimSpace(*body.Value.ExitIP)
	}
	if body.Value.Notes != nil {
		entry.Notes = strings.TrimSpace(*body.Value.Notes)
	}
	h.cfg.ProxyPool[targetIndex] = entry
	h.cfg.SanitizeProxyPool()
	h.persistLocked(c)
}

// DeleteProxyPoolEntry removes a single entry by "id" or "index" query parameter.
func (h *Handler) DeleteProxyPoolEntry(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if id := strings.TrimSpace(c.Query("id")); id != "" {
		for i := range h.cfg.ProxyPool {
			if h.cfg.ProxyPool[i].ID == id {
				h.cfg.ProxyPool = append(h.cfg.ProxyPool[:i], h.cfg.ProxyPool[i+1:]...)
				h.cfg.SanitizeProxyPool()
				h.persistLocked(c)
				return
			}
		}
		c.JSON(404, gin.H{"error": "item not found"})
		return
	}
	if idxStr := c.Query("index"); idxStr != "" {
		var idx int
		if _, err := fmt.Sscanf(idxStr, "%d", &idx); err == nil && idx >= 0 && idx < len(h.cfg.ProxyPool) {
			h.cfg.ProxyPool = append(h.cfg.ProxyPool[:idx], h.cfg.ProxyPool[idx+1:]...)
			h.cfg.SanitizeProxyPool()
			h.persistLocked(c)
			return
		}
	}
	c.JSON(400, gin.H{"error": "missing id or index"})
}
