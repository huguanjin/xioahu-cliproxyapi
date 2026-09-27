package management

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// authFileArchiveRequest is the body of a bulk download.
//
// It is a POST rather than a GET because the caller's name list is the payload:
// a sweep over a large pool asks for hundreds of files at once, and the encoded
// names run far past any URL length limit long before the pool is exhausted.
type authFileArchiveRequest struct {
	Name  string   `json:"name"`
	Names []string `json:"names"`
	// IncludeEmails writes an emails.txt beside the credential files. Defaults to
	// true; a caller that only wants the raw files can turn it off.
	IncludeEmails *bool `json:"include_emails"`
}

// DownloadAuthFilesArchive streams a zip of the requested auth files.
//
// Why a server endpoint at all, when the single-file download already exists:
// a browser cannot assemble a zip without a library the frontend does not carry,
// and downloading hundreds of individual files makes the browser either block on
// a multi-download prompt or bury the operator in a pile of separate saves. The
// Go standard library already ships archive/zip, so the archive costs no new
// dependency and one round trip.
//
// The file list is untrusted input — it arrives from the client — so every name
// goes through the same isUnsafeAuthFileName guard the single-file download uses
// before it is joined to the auth directory or written as a zip entry. A name
// that fails the guard, or that has since been deleted, is reported in a skipped
// manifest rather than failing the whole archive: on a pool this size, a handful
// of stale names is normal and dropping the other hundreds would be worse.
func (h *Handler) DownloadAuthFilesArchive(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}

	var req authFileArchiveRequest
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	names := uniqueAuthFileNames(append([]string{req.Name}, req.Names...))
	if len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or names is required"})
		return
	}
	includeEmails := req.IncludeEmails == nil || *req.IncludeEmails

	// Guard every name before it reaches the filesystem or becomes a zip entry.
	// The same check the single-file download uses: no separators, no volume, and
	// a .json suffix. A caller can name any string here, so this is the one place
	// that keeps a crafted name from writing outside the auth directory or
	// producing an entry that extracts there.
	skippedByName := make(map[string]string, len(names))
	accepted := make([]string, 0, len(names))
	for _, name := range names {
		if isUnsafeAuthFileName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			skippedByName[name] = "invalid name"
			continue
		}
		accepted = append(accepted, name)
	}

	// Resolve emails up front. Once the zip stream starts the status line is
	// already sent, so a lookup failure can no longer be reported as an error.
	emails := h.collectArchiveEmails(accepted)

	// Read the payloads before responding for the same reason: an entirely
	// unusable request should still be able to answer with a status code instead
	// of an empty archive.
	readable := make(map[string][]byte, len(accepted))
	for _, name := range accepted {
		data, errRead := os.ReadFile(filepath.Join(h.cfg.AuthDir, name))
		if errRead != nil {
			if os.IsNotExist(errRead) {
				skippedByName[name] = "file not found"
			} else {
				skippedByName[name] = "read failed"
			}
			continue
		}
		readable[name] = data
	}
	if len(readable) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no readable auth files in the request"})
		return
	}

	manifest := buildArchiveSkipManifest(skippedByName)

	fileName := fmt.Sprintf("auth-files-%s.zip", time.Now().UTC().Format("20060102-150405"))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
	c.Header("Content-Type", "application/zip")

	zw := zip.NewWriter(c.Writer)
	// Written in a fixed order so two archives of the same set are byte-identical
	// up to their contents, which makes diffing two downloads meaningful.
	ordered := make([]string, 0, len(readable))
	for name := range readable {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	for _, name := range ordered {
		writer, errCreate := zw.Create(name)
		if errCreate != nil {
			// The response is already streaming; the best available signal is to
			// stop and let the truncated archive show the failure.
			_ = zw.Close()
			return
		}
		if _, errWrite := writer.Write(readable[name]); errWrite != nil {
			_ = zw.Close()
			return
		}
	}

	if includeEmails {
		if writer, errCreate := zw.Create("emails.txt"); errCreate == nil {
			_, _ = writer.Write([]byte(renderArchiveEmails(emails)))
		}
	}
	if len(manifest) > 0 {
		if writer, errCreate := zw.Create("skipped.txt"); errCreate == nil {
			_, _ = writer.Write([]byte(manifest))
		}
	}

	if errClose := zw.Close(); errClose != nil {
		// Nothing left to answer with — the status line is long gone.
		return
	}
}

type archiveSkip struct {
	name   string
	reason string
}

// collectArchiveEmails resolves the account email for each requested name.
//
// The email is read from the in-memory auth record rather than from the file on
// disk: buildAuthFileEntry already resolves it from metadata and attributes, and
// the same three fallbacks are wanted here.
//
// Names are matched the same way buildAuthFileEntry derives its own `name` —
// FileName, falling back to ID. Matching only FileName would silently produce an
// empty address list for any credential whose FileName is unset, which is the
// case on some load paths; the caller sees a perfectly good archive with a blank
// emails.txt rather than an error.
//
// A name that resolves to no record, or to a record without an email, simply
// contributes no line. That is deliberate and is not reported as a skip: the
// archive is about the files, which are read from disk and may well exist for a
// credential this process has not loaded. An address the operator never had is
// not an error, and a placeholder line would corrupt a list that gets pasted
// straight into another tool.
func (h *Handler) collectArchiveEmails(names []string) []string {
	if h == nil || h.authManager == nil || len(names) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}

	byName := make(map[string]string, len(names))
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		name := strings.TrimSpace(auth.FileName)
		if name == "" {
			name = strings.TrimSpace(auth.ID)
		}
		if _, ok := wanted[name]; !ok {
			continue
		}
		if email := strings.TrimSpace(authEmail(auth)); email != "" {
			byName[name] = email
		}
	}

	// Ordered like the archive itself so the text list lines up with the files a
	// reader sees when they open the zip.
	ordered := make([]string, 0, len(byName))
	for name := range byName {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	emails := make([]string, 0, len(ordered))
	for _, name := range ordered {
		emails = append(emails, byName[name])
	}
	return emails
}

// renderArchiveEmails builds the text list.
//
// One address per line and nothing else: no header, no index, no delimiter. The
// file exists to be pasted into whatever the operator runs next, and any framing
// added here would have to be stripped back out there. An empty final line is
// omitted so tools that count lines do not report one address too many.
func renderArchiveEmails(emails []string) string {
	if len(emails) == 0 {
		return ""
	}
	return strings.Join(emails, "\n") + "\n"
}

// buildArchiveSkipManifest describes what was left out, in name-sorted order.
//
// It is written into the archive instead of the response body because the body is
// the zip itself. An archive that quietly holds fewer files than were asked for
// is the failure this prevents: the count in the manifest is what tells the
// operator whether a short archive means "some names were stale" or "the download
// was cut off".
func buildArchiveSkipManifest(skipped map[string]string) string {
	if len(skipped) == 0 {
		return ""
	}
	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, name)
	}
	sort.Strings(names)

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d file(s) were requested but not included:\n\n", len(names))
	for _, name := range names {
		fmt.Fprintf(&sb, "%s\t%s\n", name, skipped[name])
	}
	return sb.String()
}
