package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestGetProxyPool_ReturnsAllWithoutQuery(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			ProxyPool: []config.ProxyPoolEntry{
				{ID: "a", Name: "Alpha", ProxyURL: "socks5://1.1.1.1:1080"},
				{ID: "b", Name: "Beta", ProxyURL: "socks5://2.2.2.2:1080"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/proxy-pool", nil)

	h.GetProxyPool(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Alpha") || !strings.Contains(rec.Body.String(), "Beta") {
		t.Fatalf("expected both entries in response, got %s", rec.Body.String())
	}
}

func TestGetProxyPool_FuzzySearchFiltersByFields(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			ProxyPool: []config.ProxyPoolEntry{
				{ID: "a", Name: "Alpha", ProxyURL: "socks5://1.1.1.1:1080", ExitIP: "9.9.9.9", Account: "acct-a"},
				{ID: "b", Name: "Beta", ProxyURL: "socks5://2.2.2.2:1080", ExitIP: "8.8.8.8", Account: "acct-b"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/proxy-pool?q=9.9.9.9", nil)

	h.GetProxyPool(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Alpha") || strings.Contains(body, "Beta") {
		t.Fatalf("expected fuzzy search to match only Alpha, got %s", body)
	}
}

func TestPutProxyPool_ReplacesEntireList(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			ProxyPool: []config.ProxyPoolEntry{
				{ID: "old", Name: "Old", ProxyURL: "socks5://1.1.1.1:1080"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	body := `[{"id":"new","name":"New","proxy-url":"socks5://3.3.3.3:1080"}]`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/proxy-pool", strings.NewReader(body))

	h.PutProxyPool(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(h.cfg.ProxyPool) != 1 || h.cfg.ProxyPool[0].ID != "new" {
		t.Fatalf("expected pool replaced with single 'new' entry, got %+v", h.cfg.ProxyPool)
	}
}

func TestPatchProxyPoolEntry_UpdatesFieldsByID(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			ProxyPool: []config.ProxyPoolEntry{
				{ID: "a", Name: "Alpha", ProxyURL: "socks5://1.1.1.1:1080"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	body := `{"id":"a","value":{"name":"Alpha Renamed","exit-ip":"5.5.5.5"}}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/proxy-pool", strings.NewReader(body))

	h.PatchProxyPoolEntry(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(h.cfg.ProxyPool) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(h.cfg.ProxyPool))
	}
	entry := h.cfg.ProxyPool[0]
	if entry.Name != "Alpha Renamed" || entry.ExitIP != "5.5.5.5" || entry.ProxyURL != "socks5://1.1.1.1:1080" {
		t.Fatalf("unexpected entry after patch: %+v", entry)
	}
}

func TestPatchProxyPoolEntry_BlankProxyURLDeletesEntry(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			ProxyPool: []config.ProxyPoolEntry{
				{ID: "a", Name: "Alpha", ProxyURL: "socks5://1.1.1.1:1080"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	body := `{"id":"a","value":{"proxy-url":""}}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/proxy-pool", strings.NewReader(body))

	h.PatchProxyPoolEntry(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(h.cfg.ProxyPool) != 0 {
		t.Fatalf("expected entry removed, got %+v", h.cfg.ProxyPool)
	}
}

func TestPatchProxyPoolEntry_NotFound(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg:            &config.Config{ProxyPool: []config.ProxyPoolEntry{}},
		configFilePath: writeTestConfigFile(t),
	}

	body := `{"id":"missing","value":{"name":"x"}}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/proxy-pool", strings.NewReader(body))

	h.PatchProxyPoolEntry(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestDeleteProxyPoolEntry_ByID(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			ProxyPool: []config.ProxyPoolEntry{
				{ID: "a", Name: "Alpha", ProxyURL: "socks5://1.1.1.1:1080"},
				{ID: "b", Name: "Beta", ProxyURL: "socks5://2.2.2.2:1080"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/proxy-pool?id=a", nil)

	h.DeleteProxyPoolEntry(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(h.cfg.ProxyPool) != 1 || h.cfg.ProxyPool[0].ID != "b" {
		t.Fatalf("expected only 'b' to remain, got %+v", h.cfg.ProxyPool)
	}
}

func TestDeleteProxyPoolEntry_MissingIDAndIndex(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg:            &config.Config{ProxyPool: []config.ProxyPoolEntry{}},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/proxy-pool", nil)

	h.DeleteProxyPoolEntry(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}
