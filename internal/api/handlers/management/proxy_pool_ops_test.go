package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// newFakeProxy starts an HTTP proxy stand-in that answers every request with 204, which is
// all the connectivity probe needs.
func newFakeProxy(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// newDeadProxy returns the address of a proxy that refuses connections.
func newDeadProxy(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()
	return addr
}

func useProbeURL(t *testing.T) {
	t.Helper()
	prev := proxyProbeURL
	proxyProbeURL = "http://probe.invalid/"
	t.Cleanup(func() { proxyProbeURL = prev })
}

func uploadAuthFiles(t *testing.T, h *Handler, query string, files map[string]string, order []string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, name := range order {
		part, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err = part.Write([]byte(files[name])); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files"+query, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req
	h.UploadAuthFile(ctx)
	payload := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	return rec, payload
}

func agFiles(prefix string, n int) (map[string]string, []string) {
	files := make(map[string]string, n)
	order := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s%02d.json", prefix, i)
		files[name] = fmt.Sprintf(`{"type":"antigravity","email":"%s%02d@example.com","access_token":"t","refresh_token":"r","project_id":"p"}`, prefix, i)
		order = append(order, name)
	}
	return files, order
}

func TestPickLeastLoaded(t *testing.T) {
	t.Parallel()

	if got := pickLeastLoaded(nil, nil); got != -1 {
		t.Fatalf("empty = %d, want -1", got)
	}
	urls := []string{"a", "b", "c"}
	if got := pickLeastLoaded(urls, map[string]int{}); got != 0 {
		t.Fatalf("tie = %d, want first", got)
	}
	if got := pickLeastLoaded(urls, map[string]int{"a": 3, "b": 1, "c": 1}); got != 1 {
		t.Fatalf("least loaded = %d, want 1 (earlier of the tied)", got)
	}
}

func TestUploadAuthFile_AutoProxySpreadsEvenlyAndSkipsDeadProxy(t *testing.T) {
	useProbeURL(t)
	t.Setenv("MANAGEMENT_PASSWORD", "")

	pool := []config.ProxyPoolEntry{
		{ID: "a", Name: "A", ProxyURL: newFakeProxy(t)},
		{ID: "dead", Name: "Dead", ProxyURL: newDeadProxy(t)},
		{ID: "b", Name: "B", ProxyURL: newFakeProxy(t)},
		{ID: "c", Name: "C", ProxyURL: newFakeProxy(t)},
	}
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, ProxyPool: pool}, manager)

	files, order := agFiles("first", 4)
	rec, payload := uploadAuthFiles(t, h, "?auto_proxy=1", files, order)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	report, _ := payload["auto_proxy"].(map[string]any)
	if report == nil {
		t.Fatalf("missing auto_proxy report: %s", rec.Body.String())
	}
	if unhealthy, _ := report["unhealthy"].([]any); len(unhealthy) != 1 {
		t.Fatalf("unhealthy = %v, want exactly the dead proxy", report["unhealthy"])
	}

	// A second import tops up the least loaded proxies instead of restarting from the first.
	files2, order2 := agFiles("second", 5)
	if rec, _ = uploadAuthFiles(t, h, "?auto_proxy=1", files2, order2); rec.Code != http.StatusOK {
		t.Fatalf("second upload status = %d; body=%s", rec.Code, rec.Body.String())
	}

	counts := h.proxyBoundCounts()
	for _, entry := range pool {
		want := 3
		if entry.ID == "dead" {
			want = 0
		}
		if got := counts[entry.ProxyURL]; got != want {
			t.Fatalf("proxy %s bound = %d, want %d (all counts: %v)", entry.ID, got, want, counts)
		}
	}

	// The binding is persisted in the file itself, not only in memory.
	data, err := os.ReadFile(filepath.Join(authDir, order[0]))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if !strings.Contains(string(data), `"proxy_url":"`+pool[0].ProxyURL+`"`) {
		t.Fatalf("file %s lacks proxy_url %s: %s", order[0], pool[0].ProxyURL, data)
	}
	if !strings.Contains(string(data), `"refresh_token":"r"`) {
		t.Fatalf("existing fields must survive the injection: %s", data)
	}

	// GetProxyPool reports the same numbers.
	grec := httptest.NewRecorder()
	gctx, _ := gin.CreateTestContext(grec)
	gctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/proxy-pool", nil)
	h.GetProxyPool(gctx)
	var got struct {
		Pool []struct {
			ID    string `json:"id"`
			Bound int    `json:"bound-count"`
		} `json:"proxy-pool"`
	}
	if err = json.Unmarshal(grec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode proxy pool: %v", err)
	}
	for _, entry := range got.Pool {
		want := 3
		if entry.ID == "dead" {
			want = 0
		}
		if entry.Bound != want {
			t.Fatalf("GET proxy-pool %s bound-count = %d, want %d", entry.ID, entry.Bound, want)
		}
	}
}

func TestUploadAuthFile_AutoProxyRespectsExplicitAndExistingBinding(t *testing.T) {
	useProbeURL(t)
	t.Setenv("MANAGEMENT_PASSWORD", "")

	pool := []config.ProxyPoolEntry{
		{ID: "a", ProxyURL: newFakeProxy(t)},
		{ID: "b", ProxyURL: newFakeProxy(t)},
	}
	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, ProxyPool: pool}, coreauth.NewManager(nil, nil, nil))

	files := map[string]string{
		"own.json":   `{"type":"antigravity","email":"own@example.com","proxy_url":"socks5://own.example:1080"}`,
		"plain.json": `{"type":"antigravity","email":"plain@example.com"}`,
	}
	if rec, _ := uploadAuthFiles(t, h, "?auto_proxy=1", files, []string{"own.json", "plain.json"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	own, _ := h.authManager.GetByID("own.json")
	if own == nil || own.ProxyURL != "socks5://own.example:1080" {
		t.Fatalf("explicit proxy_url must be kept, got %+v", own)
	}
	plain, _ := h.authManager.GetByID("plain.json")
	if plain == nil || plain.ProxyURL != pool[0].ProxyURL {
		t.Fatalf("plain credential should take the first idle proxy, got %+v", plain)
	}

	// Re-uploading the same credential without a proxy_url must not move it to another IP.
	pool0Bound := h.proxyBoundCounts()[pool[0].ProxyURL]
	if rec, _ := uploadAuthFiles(t, h, "?auto_proxy=1", map[string]string{"plain.json": files["plain.json"]}, []string{"plain.json"}); rec.Code != http.StatusOK {
		t.Fatalf("re-upload status = %d; body=%s", rec.Code, rec.Body.String())
	}
	plain, _ = h.authManager.GetByID("plain.json")
	if plain == nil || plain.ProxyURL != pool[0].ProxyURL {
		t.Fatalf("re-upload moved the credential to %+v", plain)
	}
	if got := h.proxyBoundCounts()[pool[0].ProxyURL]; got != pool0Bound {
		t.Fatalf("re-upload changed bound count %d -> %d", pool0Bound, got)
	}
}

func TestUploadAuthFile_AutoProxyFailsWithoutUsableProxy(t *testing.T) {
	useProbeURL(t)
	t.Setenv("MANAGEMENT_PASSWORD", "")

	cases := map[string][]config.ProxyPoolEntry{
		"empty pool": nil,
		"all dead":   {{ID: "dead", ProxyURL: newDeadProxy(t)}},
	}
	for name, pool := range cases {
		t.Run(name, func(t *testing.T) {
			authDir := t.TempDir()
			manager := coreauth.NewManager(nil, nil, nil)
			h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, ProxyPool: pool}, manager)

			files, order := agFiles("x", 2)
			rec, _ := uploadAuthFiles(t, h, "?auto_proxy=1", files, order)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusConflict, rec.Body.String())
			}
			if entries, _ := os.ReadDir(authDir); len(entries) != 0 || len(manager.List()) != 0 {
				t.Fatalf("nothing may be stored when auto proxy cannot run, got %d files", len(entries))
			}
		})
	}
}

func TestUploadAuthFile_WithoutAutoProxyLeavesFilesUntouched(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, ProxyPool: []config.ProxyPoolEntry{{ID: "a", ProxyURL: "http://127.0.0.1:1"}}}, coreauth.NewManager(nil, nil, nil))

	files, order := agFiles("n", 1)
	rec, payload := uploadAuthFiles(t, h, "", files, order)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := payload["auto_proxy"]; ok {
		t.Fatalf("auto_proxy report must be absent when not requested: %s", rec.Body.String())
	}
	data, _ := os.ReadFile(filepath.Join(authDir, order[0]))
	if string(data) != files[order[0]] {
		t.Fatalf("file changed: %s", data)
	}
}

func TestTestProxyPool(t *testing.T) {
	useProbeURL(t)

	good, dead := newFakeProxy(t), newDeadProxy(t)
	h := &Handler{cfg: &config.Config{ProxyPool: []config.ProxyPoolEntry{
		{ID: "good", Name: "Good", ProxyURL: good},
		{ID: "dead", Name: "Dead", ProxyURL: dead},
	}}}

	post := func(body string) (*httptest.ResponseRecorder, []proxyProbeResult) {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/proxy-pool/test", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.TestProxyPool(ctx)
		var out struct {
			Results []proxyProbeResult `json:"results"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out.Results
	}

	if rec, res := post(`{"all":true}`); rec.Code != http.StatusOK || len(res) != 2 || !res[0].OK || res[1].OK || res[1].Error == "" {
		t.Fatalf("all: status=%d results=%+v", rec.Code, res)
	}
	if rec, res := post(`{"id":"good"}`); rec.Code != http.StatusOK || len(res) != 1 || !res[0].OK || res[0].StatusCode != http.StatusNoContent {
		t.Fatalf("by id: status=%d results=%+v", rec.Code, res)
	}
	if rec, res := post(`{"proxy-url":"` + dead + `"}`); rec.Code != http.StatusOK || len(res) != 1 || res[0].OK {
		t.Fatalf("ad-hoc: status=%d results=%+v", rec.Code, res)
	}
	if rec, _ := post(`{"id":"missing"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing id status = %d", rec.Code)
	}
	if rec, _ := post(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty selector status = %d", rec.Code)
	}
	if _, res := post(`{"proxy-url":"ftp://x"}`); len(res) != 1 || res[0].OK || res[0].Error == "" {
		t.Fatalf("unsupported scheme must fail: %+v", res)
	}
}

func TestPutAndPatchProxyPool_PrefixBareAddressWithSocks5(t *testing.T) {
	t.Parallel()

	h := &Handler{cfg: &config.Config{}, configFilePath: writeTestConfigFile(t)}

	put := httptest.NewRecorder()
	pc, _ := gin.CreateTestContext(put)
	pc.Request = httptest.NewRequest(http.MethodPut, "/v0/management/proxy-pool", strings.NewReader(`[{"id":"a","proxy-url":"user:pass@1.2.3.4:1080"}]`))
	h.PutProxyPool(pc)
	if got := h.cfg.ProxyPool[0].ProxyURL; got != "socks5://user:pass@1.2.3.4:1080" {
		t.Fatalf("PUT stored %q", got)
	}

	patch := httptest.NewRecorder()
	cc, _ := gin.CreateTestContext(patch)
	cc.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/proxy-pool", strings.NewReader(`{"id":"a","value":{"proxy-url":"9.9.9.9:2080"}}`))
	cc.Request.Header.Set("Content-Type", "application/json")
	h.PatchProxyPoolEntry(cc)
	if got := h.cfg.ProxyPool[0].ProxyURL; got != "socks5://9.9.9.9:2080" {
		t.Fatalf("PATCH stored %q", got)
	}
}
