package cmd

import (
	"archive/zip"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sbUploadSeen struct {
	method, path, field, filename, content string
	requests                               int
}

func sbUploadServer(t *testing.T, status int, response string) (*httptest.Server, *sbUploadSeen) {
	t.Helper()
	seen := &sbUploadSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.requests++
		seen.method, seen.path = r.Method, r.URL.Path
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			raw, _ := io.ReadAll(part)
			seen.field, seen.filename, seen.content = part.FormName(), part.FileName(), string(raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func writeTempFile(t *testing.T, name string, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	return path
}

// writeZip writes a zip holding the named parts: an .xlsx when one is xl/workbook.xml.
func writeZip(t *testing.T, name string, parts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(f)
	for _, part := range parts {
		w, err := archive.Create(part)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("<x/>"))
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return path
}

func TestUploadSpreadsheetTemplate_PostsTheFileAsMultipart(t *testing.T) {
	path := writeZip(t, "Plantilla Ventas.xlsx", "[Content_Types].xml", "xl/workbook.xml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, seen := sbUploadServer(t, 200,
		`{"attachmentId":"abc","fileName":"attachments/tenant/abc.xlsx","contentType":"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}`)
	ref, err := uploadSpreadsheetTemplate(newTestClient(t, srv.URL), path)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if seen.method != "POST" || seen.path != "/v1/stores/packages/commands/attachments/upload" {
		t.Errorf("request %s %s", seen.method, seen.path)
	}
	if seen.field != "file" || seen.filename != "Plantilla Ventas.xlsx" || seen.content != string(raw) {
		t.Errorf("multipart part: field %q, filename %q, content %q", seen.field, seen.filename, seen.content)
	}
	if ref.ObjectName != "attachments/tenant/abc.xlsx" || ref.FileLabel != "Plantilla Ventas.xlsx" {
		t.Errorf("ref = %+v", ref)
	}
}

func TestUploadSpreadsheetTemplate_RefusesBeforeUploading(t *testing.T) {
	srv, seen := sbUploadServer(t, 200, `{"fileName":"attachments/tenant/x.xlsx"}`)
	c := newTestClient(t, srv.URL)
	for name, tc := range map[string]struct{ path, want string }{
		"xls":        {writeTempFile(t, "old.xls", 10), "not an .xlsx"},
		"xlsm":       {writeTempFile(t, "macro.xlsm", 10), "not an .xlsx"},
		"csv":        {writeTempFile(t, "data.csv", 10), "not an .xlsx"},
		"no ext":     {writeTempFile(t, "plantilla", 10), "not an .xlsx"},
		"over 25 MB": {writeTempFile(t, "big.xlsx", spreadsheetTemplateMaxBytes+1), "at most 25 MB"},
		"missing":    {filepath.Join(t.TempDir(), "nope.xlsx"), "cannot read"},
		"empty":      {writeTempFile(t, "empty.xlsx", 0), "is empty"},
		"not a zip":  {writeTempFile(t, "renamed.xlsx", 10), "not a readable .xlsx"},
		"other zip":  {writeZip(t, "docs.xlsx", "word/document.xml"), "not an Excel workbook"},
	} {
		_, err := uploadSpreadsheetTemplate(c, tc.path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error mentioning %q, got: %v", name, tc.want, err)
		}
	}
	if seen.requests != 0 {
		t.Errorf("a refused file must send no request, sent %d", seen.requests)
	}
	if err := checkSpreadsheetTemplateFile(writeTempFile(t, "exact.XLSX", spreadsheetTemplateMaxBytes)); err == nil ||
		strings.Contains(err.Error(), "25 MB") {
		t.Errorf("exactly 25 MB passes the size check (then fails as no zip), got: %v", err)
	}
	if err := checkSpreadsheetTemplateFile(writeZip(t, "ok.XLSX", "xl/workbook.xml")); err != nil {
		t.Errorf("a workbook with an upper-case extension is fine, got: %v", err)
	}
}

func TestUploadSpreadsheetTemplate_RefusesAnObjectOutsideTheAttachmentsPrefix(t *testing.T) {
	srv, _ := sbUploadServer(t, 200, `{"attachmentId":"abc","fileName":"abc.xlsx"}`)
	_, err := uploadSpreadsheetTemplate(newTestClient(t, srv.URL), writeZip(t, "t.xlsx", "xl/workbook.xml"))
	if err == nil || !strings.Contains(err.Error(), "would refuse it") {
		t.Fatalf("want the prefix refusal, got: %v", err)
	}
	srvErr, _ := sbUploadServer(t, 400, `{"message":"Unsupported file extension"}`)
	if _, err := uploadSpreadsheetTemplate(newTestClient(t, srvErr.URL), writeZip(t, "t.xlsx", "xl/workbook.xml")); err == nil {
		t.Fatal("a 400 must be an error")
	}
}

func TestSpreadsheetUploadTemplateIsRegistered(t *testing.T) {
	cmd, rest, err := rootCmd.Find([]string{"workflows-v2", "spreadsheet", "upload-template"})
	if err != nil || len(rest) != 0 || cmd.Name() != "upload-template" {
		t.Fatalf("not registered: %v %v", rest, err)
	}
}
