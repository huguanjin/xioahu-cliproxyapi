package management

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestParseProxyImport(t *testing.T) {
	t.Parallel()

	text := "\ufeff# gateway list\n" +
		"\n" +
		"u:p@91.132.137.186:10003\r\n" +
		"u:p@91.132.137.186:10003\n" + // duplicate inside the text
		"http://u:p@1.2.3.4:8080\n" +
		"u:p@[2001:db8::1]:1080\n" +
		"u:p@2001:db8::1:1080\n" + // IPv6 without brackets is ambiguous
		"u:p@1.2.3.4\n" + // no port
		"ftp://1.2.3.4:21\n" +
		"already:x@5.5.5.5:1\n" // exists in the pool

	seen := map[string]struct{}{"socks5://already:x@5.5.5.5:1": {}}
	entries, rep := parseProxyImport(text, "v6", true, seen)

	if rep.Added != 3 || rep.Duplicate != 2 || len(rep.Invalid) != 3 {
		t.Fatalf("report = %+v", rep)
	}
	wantURLs := []string{"socks5://u:p@91.132.137.186:10003", "http://u:p@1.2.3.4:8080", "socks5://u:p@[2001:db8::1]:1080"}
	wantNames := []string{"v6 91.132.137.186:10003", "v6 1.2.3.4:8080", "v6 [2001:db8::1]:1080"}
	for i, e := range entries {
		if e.ProxyURL != wantURLs[i] || e.Name != wantNames[i] {
			t.Fatalf("entry %d = %+v", i, e)
		}
		if strings.Contains(e.Name, "u:p") {
			t.Fatalf("name must not leak credentials: %q", e.Name)
		}
	}
	// Line numbers refer to the original text, blank and comment lines included.
	if got := []int{rep.Invalid[0].Line, rep.Invalid[1].Line, rep.Invalid[2].Line}; got[0] != 7 || got[1] != 8 || got[2] != 9 {
		t.Fatalf("invalid lines = %v", got)
	}
}

func TestImportProxyPool_AppendsAndSkipsDuplicates(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg:            &config.Config{ProxyPool: []config.ProxyPoolEntry{{ID: "old", ProxyURL: "socks5://u:p@1.1.1.1:1"}}},
		configFilePath: writeTestConfigFile(t),
	}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/proxy-pool/import", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.ImportProxyPool(c)
		return rec
	}

	rec := post(`{"text":"u:p@1.1.1.1:1\nu:p@2.2.2.2:2\nu:p@[2001:db8::2]:3\nbad"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"added":2`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(h.cfg.ProxyPool) != 3 || h.cfg.ProxyPool[0].ID != "old" {
		t.Fatalf("pool = %+v", h.cfg.ProxyPool)
	}
	for _, e := range h.cfg.ProxyPool {
		if e.ID == "" {
			t.Fatalf("imported entry without id: %+v", e)
		}
	}
	// Importing the same text again adds nothing.
	if rec = post(`{"text":"u:p@2.2.2.2:2"}`); !strings.Contains(rec.Body.String(), `"added":0`) || len(h.cfg.ProxyPool) != 3 {
		t.Fatalf("re-import: body=%s pool=%d", rec.Body.String(), len(h.cfg.ProxyPool))
	}
	if rec = post(`{"text":"  \n"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text status = %d", rec.Code)
	}
}

// An IPv6 proxy address must work end to end: bracketed host, probed through the real transport.
func TestTestProxyPool_IPv6ProxyAddress(t *testing.T) {
	useProbeURL(t)

	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	res := probeProxy(t.Context(), "http://"+ln.Addr().String())
	if !res.OK {
		t.Fatalf("probe through %s failed: %s", ln.Addr(), res.Error)
	}
}

func TestResidentialTag_ImportPatchAndListing(t *testing.T) {
	t.Parallel()

	h := &Handler{cfg: &config.Config{}, configFilePath: writeTestConfigFile(t)}
	do := func(method, body string, handler func(*gin.Context)) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(method, "/v0/management/proxy-pool", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		handler(c)
		return rec
	}

	// Imported with the tag, and a second batch without it.
	if rec := do(http.MethodPost, `{"text":"u:p@1.1.1.1:1\nu:p@2.2.2.2:2","residential":true}`, h.ImportProxyPool); rec.Code != http.StatusOK {
		t.Fatalf("import status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPost, `{"text":"u:p@3.3.3.3:3"}`, h.ImportProxyPool); rec.Code != http.StatusOK {
		t.Fatalf("import status = %d; body=%s", rec.Code, rec.Body.String())
	}
	got := map[string]bool{}
	for _, e := range h.cfg.ProxyPool {
		got[e.Name] = e.Residential
	}
	if !got["1.1.1.1:1"] || !got["2.2.2.2:2"] || got["3.3.3.3:3"] {
		t.Fatalf("residential after import = %v", got)
	}

	// PATCH flips it both ways and leaves other fields alone.
	id := h.cfg.ProxyPool[2].ID
	if rec := do(http.MethodPatch, `{"id":"`+id+`","value":{"residential":true}}`, h.PatchProxyPoolEntry); rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if e := h.cfg.ProxyPool[2]; !e.Residential || e.Name != "3.3.3.3:3" {
		t.Fatalf("after patch on: %+v", e)
	}
	if rec := do(http.MethodPatch, `{"id":"`+id+`","value":{"residential":false}}`, h.PatchProxyPoolEntry); rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d", rec.Code)
	}
	if h.cfg.ProxyPool[2].Residential {
		t.Fatalf("patch off did not clear the tag")
	}

	// A patch that does not mention the field must not clear it.
	first := h.cfg.ProxyPool[0].ID
	if rec := do(http.MethodPatch, `{"id":"`+first+`","value":{"notes":"x"}}`, h.PatchProxyPoolEntry); rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d", rec.Code)
	}
	if !h.cfg.ProxyPool[0].Residential {
		t.Fatalf("unrelated patch cleared the residential tag")
	}

	// The listing reports it, and a whole-list PUT round-trips it.
	rec := do(http.MethodGet, ``, h.GetProxyPool)
	var listed struct {
		Pool []struct {
			Name        string `json:"name"`
			Residential bool   `json:"residential"`
		} `json:"proxy-pool"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed.Pool) != 3 || !listed.Pool[0].Residential || listed.Pool[2].Residential {
		t.Fatalf("listing = %+v", listed.Pool)
	}
	if rec = do(http.MethodPut, rec.Body.String()[len(`{"proxy-pool":`):len(rec.Body.String())-1], h.PutProxyPool); rec.Code != http.StatusOK {
		t.Fatalf("put status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if !h.cfg.ProxyPool[0].Residential || !h.cfg.ProxyPool[1].Residential || h.cfg.ProxyPool[2].Residential {
		t.Fatalf("PUT round trip lost the tag: %+v", h.cfg.ProxyPool)
	}
}
