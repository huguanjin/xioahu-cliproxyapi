package management

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// archiveContents reads a zip body into a name → contents map.
func archiveContents(t *testing.T, body []byte) map[string]string {
	t.Helper()
	reader, errReader := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if errReader != nil {
		t.Fatalf("archive is not a readable zip: %v", errReader)
	}
	out := make(map[string]string, len(reader.File))
	for _, file := range reader.File {
		fh, errOpen := file.Open()
		if errOpen != nil {
			t.Fatalf("failed to open %s in archive: %v", file.Name, errOpen)
		}
		data, errRead := io.ReadAll(fh)
		if errClose := fh.Close(); errClose != nil {
			t.Fatalf("failed to close %s: %v", file.Name, errClose)
		}
		if errRead != nil {
			t.Fatalf("failed to read %s: %v", file.Name, errRead)
		}
		out[file.Name] = string(data)
	}
	return out
}

func callArchive(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/download-archive", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.DownloadAuthFilesArchive(ctx)
	return rec
}

func TestDownloadAuthFilesArchive_PacksFilesAndEmails(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	files := map[string]string{
		"a.json": `{"type":"antigravity","email":"a@example.com"}`,
		"b.json": `{"type":"antigravity","email":"b@example.com"}`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	for name, content := range files {
		var metadata map[string]any
		if err := json.Unmarshal([]byte(content), &metadata); err != nil {
			t.Fatalf("bad fixture: %v", err)
		}
		record := &coreauth.Auth{
			ID:         name,
			FileName:   name,
			Provider:   "antigravity",
			Attributes: map[string]string{"path": filepath.Join(authDir, name)},
			Metadata:   metadata,
		}
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("failed to register %s: %v", name, errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := callArchive(t, h, `{"names":["a.json","b.json"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got := archiveContents(t, rec.Body.Bytes())

	if got["a.json"] != files["a.json"] {
		t.Fatalf("a.json content = %q, want %q", got["a.json"], files["a.json"])
	}
	if got["b.json"] != files["b.json"] {
		t.Fatalf("b.json content = %q, want %q", got["b.json"], files["b.json"])
	}
	// One address per line, no header, and each file's own address present.
	emails := got["emails.txt"]
	if strings.Contains(emails, "email") || strings.Contains(emails, ",") {
		t.Fatalf("emails.txt should be bare addresses, got %q", emails)
	}
	for _, want := range []string{"a@example.com", "b@example.com"} {
		if !strings.Contains(emails, want) {
			t.Fatalf("emails.txt missing %q: %q", want, emails)
		}
	}
}

func TestDownloadAuthFilesArchive_RejectsUnsafeNames(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	// A real, legitimately-named file so the request still has something to pack.
	if err := os.WriteFile(filepath.Join(authDir, "ok.json"), []byte(`{"type":"antigravity"}`), 0o600); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}
	// A file that must never appear in the archive, one level up from authDir.
	// The sentinel is deliberately a string no legitimate entry would contain —
	// checking for the word "secret" would false-positive on skipped.txt, which
	// names the rejected path by design.
	const sentinel = "OUTSIDE-AUTH-DIR-SENTINEL"
	secretPath := filepath.Join(filepath.Dir(authDir), "secret.json")
	if err := os.WriteFile(secretPath, []byte(`{"marker":"`+sentinel+`"}`), 0o600); err != nil {
		t.Fatalf("failed to write secret: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	for _, name := range []string{
		"../secret.json",
		`..\\secret.json`,
		"nested/secret.json",
		`nested\\secret.json`,
		"notjson.txt",
	} {
		body, errMarshal := json.Marshal(map[string]any{"names": []string{name, "ok.json"}})
		if errMarshal != nil {
			t.Fatalf("failed to build body: %v", errMarshal)
		}
		rec := callArchive(t, h, string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("name %q: status = %d, want %d body=%s", name, rec.Code, http.StatusOK, rec.Body.String())
		}
		got := archiveContents(t, rec.Body.Bytes())

		// The traversal target's bytes must not appear under any entry name.
		for entryName, content := range got {
			if strings.Contains(content, sentinel) {
				t.Fatalf("name %q: archive entry %q leaked the outside file", name, entryName)
			}
		}
		if _, present := got[name]; present {
			t.Fatalf("name %q should not be an archive entry", name)
		}
		// The legitimate file is still packed, and the rejection is reported.
		if _, present := got["ok.json"]; !present {
			t.Fatalf("name %q: the valid file was dropped from the archive", name)
		}
		if !strings.Contains(got["skipped.txt"], name) {
			t.Fatalf("name %q: expected it in skipped.txt, got %q", name, got["skipped.txt"])
		}
	}
}

func TestDownloadAuthFilesArchive_ReportsMissingFiles(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "present.json"), []byte(`{"type":"antigravity"}`), 0o600); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	rec := callArchive(t, h, `{"names":["present.json","gone.json"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got := archiveContents(t, rec.Body.Bytes())
	if _, present := got["present.json"]; !present {
		t.Fatal("present.json should be packed even though a sibling was missing")
	}
	if !strings.Contains(got["skipped.txt"], "gone.json") {
		t.Fatalf("expected gone.json in skipped.txt, got %q", got["skipped.txt"])
	}
}

func TestDownloadAuthFilesArchive_EmptyRequestIsRejected(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)

	for _, body := range []string{`{}`, `{"names":[]}`, `{"names":["","  "]}`} {
		rec := callArchive(t, h, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
}

func TestDownloadAuthFilesArchive_NothingReadableIsNotFound(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	rec := callArchive(t, h, `{"names":["a.json","b.json"]}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestDownloadAuthFilesArchive_SkipsEmailManifestWhenDisabled(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "a.json"), []byte(`{"type":"antigravity"}`), 0o600); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	rec := callArchive(t, h, `{"names":["a.json"],"include_emails":false}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	got := archiveContents(t, rec.Body.Bytes())
	if _, present := got["emails.txt"]; present {
		t.Fatal("emails.txt should be absent when include_emails is false")
	}
}

func TestDownloadAuthFilesArchive_FallsBackToIDWhenFileNameUnset(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	const name = "idonly.json"
	if err := os.WriteFile(filepath.Join(authDir, name), []byte(`{"type":"antigravity"}`), 0o600); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}

	// FileName deliberately unset. buildAuthFileEntry derives the entry name from
	// ID in that case, and the archive must resolve the same way or emails.txt
	// comes back empty while everything else looks correct.
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	record := &coreauth.Auth{
		ID:         name,
		Provider:   "antigravity",
		Attributes: map[string]string{"path": filepath.Join(authDir, name)},
		Metadata:   map[string]any{"email": "idonly@example.com"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := callArchive(t, h, `{"names":["idonly.json"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got := archiveContents(t, rec.Body.Bytes())
	if strings.TrimSpace(got["emails.txt"]) != "idonly@example.com" {
		t.Fatalf("emails.txt = %q, want the address resolved via ID fallback", got["emails.txt"])
	}
}

func TestDownloadAuthFilesArchive_MissingEmailAddsNoLine(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	// One credential has an address, the other has none.
	for _, name := range []string{"has.json", "none.json"} {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(`{"type":"antigravity"}`), 0o600); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	withEmail := &coreauth.Auth{
		ID: "has.json", FileName: "has.json", Provider: "antigravity",
		Attributes: map[string]string{"path": filepath.Join(authDir, "has.json")},
		Metadata:   map[string]any{"email": "has@example.com"},
	}
	withoutEmail := &coreauth.Auth{
		ID: "none.json", FileName: "none.json", Provider: "antigravity",
		Attributes: map[string]string{"path": filepath.Join(authDir, "none.json")},
		Metadata:   map[string]any{},
	}
	for _, record := range []*coreauth.Auth{withEmail, withoutEmail} {
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("failed to register %s: %v", record.FileName, errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	rec := callArchive(t, h, `{"names":["has.json","none.json"]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got := archiveContents(t, rec.Body.Bytes())

	// Both files are still packed; only the text list is short one line.
	if _, present := got["none.json"]; !present {
		t.Fatal("a credential with no email must still be packed")
	}
	emails := got["emails.txt"]
	if strings.TrimSpace(emails) != "has@example.com" {
		t.Fatalf("emails.txt = %q, want exactly the one known address", emails)
	}
}
