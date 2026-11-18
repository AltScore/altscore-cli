package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleFeedbackInput() feedbackInput {
	return feedbackInput{
		Title:       "executions state 404s for a v2 execution",
		Body:        "Ran altscore executions state abc.\nExpected outputs, got HTTP 404.",
		Kind:        "bug",
		Area:        "cli",
		Context:     map[string]string{"execution_id": "exec-1", "workflow_alias": "kyc", "note": "a | b"},
		Reporter:    feedbackReporter{Email: "dev@altscore.ai", Name: "Dev"},
		Tenant:      "tenant-1",
		Environment: "production",
		Profile:     "default",
		Agent:       "claude-code",
		CLIVersion:  "v0.47.0",
		OS:          "darwin",
		Arch:        "arm64",
	}
}

func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".c2lnbmF0dXJl"
}

func TestBuildFeedbackTicket_FilesIntoTriageLikeTheHub(t *testing.T) {
	p := buildFeedbackTicket(sampleFeedbackInput())

	want := map[string]any{
		"type":            "issue",
		"priority":        "medium",
		"source":          "api",
		"status":          "triage",
		"is_product_work": true,
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %v, want %v", k, p[k], v)
		}
	}
	for _, k := range []string{"project_id", "assignee_id", "assignee_type"} {
		if _, ok := p[k]; ok {
			t.Errorf("%s must not be set: CLI reports go to triage for a person to route", k)
		}
	}
	labels := strings.Join(p["labels"].([]string), ",")
	if labels != "cli-bug,area:cli,repo-altscore-cli,reported-by-agent" {
		t.Errorf("labels = %s", labels)
	}
	custom := p["custom_fields"].(map[string]any)
	for k, v := range map[string]string{
		"reporter_email": "dev@altscore.ai",
		"tenant_id":      "tenant-1",
		"environment":    "production",
		"product":        "cli",
		"execution_id":   "exec-1",
		"cli_version":    "v0.47.0",
		"agent":          "claude-code",
		"reported_via":   "altscore-cli",
	} {
		if custom[k] != v {
			t.Errorf("custom_fields.%s = %v, want %s", k, custom[k], v)
		}
	}
}

func TestBuildFeedbackTicket_LabelsByKindAndArea(t *testing.T) {
	in := sampleFeedbackInput()
	in.Kind = "suggestion"
	in.Area = "workflow-runtime"
	in.Agent = ""
	got := strings.Join(buildFeedbackTicket(in)["labels"].([]string), ",")
	if got != "cli-enhancement,area:workflow-runtime" {
		t.Errorf("labels = %s (no repo label off the cli area, no agent label without an agent)", got)
	}
	in.Kind = "docs"
	if l := buildFeedbackTicket(in)["labels"].([]string)[0]; l != "cli-docs" {
		t.Errorf("docs label = %s", l)
	}
}

func TestRenderFeedbackDescription(t *testing.T) {
	in := sampleFeedbackInput()
	in.Context["command"] = "altscore api GET `x`"
	d := buildFeedbackTicket(in)["description"].(string)

	for _, want := range []string{
		"- **Reported by:** Dev <dev@altscore.ai>",
		"- **Via:** altscore-cli v0.47.0, filed by an agent (claude-code)",
		"- **Tenant:** tenant-1",
		"- **Area:** cli",
		"- **Workflow:** kyc",
		"- **Execution:** exec-1",
		"- **Command:** `` altscore api GET `x` ``",
		"| note | a \\| b |",
		"| os | darwin/arm64 |",
		"\n\n---\n\nRan altscore executions state abc.",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("description lacks %q:\n%s", want, d)
		}
	}
	for _, notInTable := range []string{"| execution_id |", "| workflow_alias |", "| kind |", "| agent |"} {
		if strings.Contains(d, notInTable) {
			t.Errorf("%s has its own line and must not repeat in the table", notInTable)
		}
	}
}

func TestRenderFeedbackDescription_ReporterFallbacks(t *testing.T) {
	in := sampleFeedbackInput()
	in.Reporter = feedbackReporter{Email: "ana@altscore.ai"}
	if d := renderFeedbackDescription(in, in.mergedContext()); !strings.Contains(d, "**Reported by:** ana <ana@altscore.ai>") {
		t.Errorf("name should fall back to the email's local part:\n%s", d)
	}
	in.Reporter = feedbackReporter{}
	if d := renderFeedbackDescription(in, in.mergedContext()); !strings.Contains(d, "**Reported by:** unknown") {
		t.Errorf("no identity should read unknown:\n%s", d)
	}
}

func TestParseFeedbackContext(t *testing.T) {
	got, err := parseFeedbackContext([]string{"execution_id= e-1 ", "command=altscore x --body a=b"})
	if err != nil {
		t.Fatal(err)
	}
	if got["execution_id"] != "e-1" || got["command"] != "altscore x --body a=b" {
		t.Errorf("got %v", got)
	}
	for _, bad := range []string{"no-equals", "=v", "Execution-Id=1", "tenant_id=other", "reporter_email=x@y"} {
		if _, err := parseFeedbackContext([]string{bad}); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestValidateFeedbackKindAndArea(t *testing.T) {
	if err := validateFeedbackKind("feature"); err == nil {
		t.Error("unknown kind should be refused")
	}
	if err := validateFeedbackArea("workflows"); err == nil {
		t.Error("an area outside the Hub's set should be refused")
	}
	for _, a := range feedbackAreas {
		if err := validateFeedbackArea(a); err != nil {
			t.Error(err)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlLXZhbHVl"
	in := strings.Join([]string{
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz",
		"token " + jwt,
		"ALTSCORE_CLIENT_SECRET=s3cr3t-value",
		`{"client_secret": "another-secret"}`,
		"my profile secret is profile-secret-123",
		"execution_id=exec-1 stays",
	}, "\n")
	out, n := redactSecrets(in, []string{"profile-secret-123", ""})
	for _, leaked := range []string{"abcdefghijklmnopqrstuvwxyz", jwt, "s3cr3t-value", "another-secret", "profile-secret-123"} {
		if strings.Contains(out, leaked) {
			t.Errorf("%q survived redaction:\n%s", leaked, out)
		}
	}
	if !strings.Contains(out, "execution_id=exec-1 stays") {
		t.Errorf("ordinary context was redacted:\n%s", out)
	}
	if n != 5 {
		t.Errorf("redacted %d values, want 5:\n%s", n, out)
	}
	if again, m := redactSecrets(out, nil); m != 0 || again != out {
		t.Errorf("redacting twice must find nothing new (found %d)", m)
	}
}

func TestLoadFeedbackAttachments(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "apply.log")
	dirty := filepath.Join(dir, ".env")
	if err := os.WriteFile(clean, []byte("HTTP 422 APPLY_ALIAS_NEAR_MATCH"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirty, []byte("ALTSCORE_CLIENT_SECRET=abc123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadFeedbackAttachments([]string{clean}, nil)
	if err != nil || len(got) != 1 || got[0].Name != "apply.log" {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := loadFeedbackAttachments([]string{dirty}, nil); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("a file holding a secret should be refused, got %v", err)
	}
	if _, err := loadFeedbackAttachments([]string{clean}, []string{"APPLY_ALIAS_NEAR"}); err == nil {
		t.Error("a file holding the profile's own secret should be refused")
	}
	if _, err := loadFeedbackAttachments([]string{dir}, nil); err == nil {
		t.Error("a directory should be refused")
	}
}

func TestReadFeedbackBody(t *testing.T) {
	file := filepath.Join(t.TempDir(), "r.md")
	if err := os.WriteFile(file, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		flag     string
		stdin    string
		terminal bool
		want     string
	}{
		{"inline", "ignored", false, "inline"},
		{"@" + file, "ignored", false, "from file"},
		{"", "piped", false, "piped"},
		{"", "never read", true, ""},
	}
	for _, c := range cases {
		got, err := readFeedbackBody(c.flag, strings.NewReader(c.stdin), c.terminal)
		if err != nil || got != c.want {
			t.Errorf("readFeedbackBody(%q) = %q, %v; want %q", c.flag, got, err, c.want)
		}
	}
}

func TestTokenIdentity(t *testing.T) {
	got := tokenIdentity(fakeJWT(t, map[string]any{"email": "dev@altscore.ai", "name": "Dev", "tenantId": "t"}))
	if got.Email != "dev@altscore.ai" || got.Name != "Dev" {
		t.Errorf("got %+v", got)
	}
	if got := tokenIdentity("not-a-jwt"); got != (feedbackReporter{}) {
		t.Errorf("garbage token should give no identity, got %+v", got)
	}
}

func TestDetectFeedbackAgent(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if got := detectFeedbackAgent(env(map[string]string{"CLAUDECODE": "1"})); got != "claude-code" {
		t.Errorf("Claude Code not detected: %q", got)
	}
	if got := detectFeedbackAgent(env(map[string]string{"CLAUDECODE": "1", "ALTSCORE_AGENT": "cursor"})); got != "cursor" {
		t.Errorf("ALTSCORE_AGENT should win: %q", got)
	}
	if got := detectFeedbackAgent(env(map[string]string{"ALTSCORE_AGENT": "bad value; rm -rf"})); got != "" {
		t.Errorf("a malformed ALTSCORE_AGENT should be ignored: %q", got)
	}
	if got := detectFeedbackAgent(env(nil)); got != "" {
		t.Errorf("no agent: %q", got)
	}
}

func TestFeedbackFingerprint(t *testing.T) {
	a := sampleFeedbackInput()
	b := sampleFeedbackInput()
	b.Title = "  Executions STATE 404s   for a v2 execution "
	b.Body = "different body"
	if feedbackFingerprint(a) != feedbackFingerprint(b) {
		t.Error("case and spacing of the title must not make a new report")
	}
	b.Tenant = "other"
	if feedbackFingerprint(a) == feedbackFingerprint(b) {
		t.Error("another tenant is another report")
	}
}

func TestFeedbackLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), feedbackStateFile)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	l := loadFeedbackLedger(path)
	if err := l.record("old", feedbackSent{Ticket: "HQ-1", At: now.Add(-40 * 24 * time.Hour)}, now.Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := l.record("fp", feedbackSent{Ticket: "HQ-2", At: now}, now); err != nil {
		t.Fatal(err)
	}

	reloaded := loadFeedbackLedger(path)
	if _, ok := reloaded.entries["old"]; ok {
		t.Error("entries past retention should be pruned on write")
	}
	if s, ok := reloaded.recent("fp", now.Add(6*24*time.Hour)); !ok || s.Ticket != "HQ-2" {
		t.Errorf("within the window: %v %v", s, ok)
	}
	if _, ok := reloaded.recent("fp", now.Add(8*24*time.Hour)); ok {
		t.Error("past the window the same report may be filed again")
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l := loadFeedbackLedger(path); len(l.entries) != 0 {
		t.Error("a corrupt ledger reads as empty")
	}
	if err := loadFeedbackLedger("").record("x", feedbackSent{}, now); err != nil {
		t.Error("a ledger without a path is a no-op")
	}
}

type feedbackServer struct {
	mu          sync.Mutex
	ticketBody  map[string]any
	ticketCalls int
	uploads     []map[string]string
	ticketCode  int
}

func (s *feedbackServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case feedbackTicketsPath:
			s.ticketCalls++
			if err := json.NewDecoder(r.Body).Decode(&s.ticketBody); err != nil {
				t.Errorf("ticket body: %v", err)
			}
			if s.ticketCode != 0 {
				w.WriteHeader(s.ticketCode)
				_, _ = w.Write([]byte(`{"detail":"Forbidden"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"11111111-2222-3333-4444-555555555555","ticket_number":2301,"status":"triage"}`))
		case feedbackAttachmentsPath:
			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil {
				t.Errorf("upload content type: %v", err)
				return
			}
			mr := multipart.NewReader(r.Body, params["boundary"])
			got := map[string]string{}
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Errorf("multipart: %v", err)
					return
				}
				data, _ := io.ReadAll(part)
				if part.FileName() != "" {
					got["filename"] = part.FileName()
					got["file"] = string(data)
					got["file_type"] = part.Header.Get("Content-Type")
				} else {
					got[part.FormName()] = string(data)
				}
			}
			s.uploads = append(s.uploads, got)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"att-1"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}
}

func TestFileFeedback_CreatesTicketThenAttachments(t *testing.T) {
	fs := &feedbackServer{}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()

	in := sampleFeedbackInput()
	in.Attachments = []feedbackAttachment{{Name: "apply.log", Data: []byte("HTTP 422")}}
	ledgerPath := filepath.Join(t.TempDir(), feedbackStateFile)
	var stdout, stderr bytes.Buffer
	run := feedbackRun{
		Input:  in,
		Ledger: loadFeedbackLedger(ledgerPath),
		Now:    time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
		Stdout: &stdout,
		Stderr: &stderr,
	}
	if err := fileFeedback(newTestClient(t, srv.URL), run); err != nil {
		t.Fatal(err)
	}

	if fs.ticketBody["status"] != "triage" || fs.ticketBody["title"] != in.Title {
		t.Errorf("ticket body = %v", fs.ticketBody)
	}
	if len(fs.uploads) != 1 {
		t.Fatalf("uploads = %v", fs.uploads)
	}
	up := fs.uploads[0]
	if up["owner_type"] != "ticket" || up["owner_id"] != "11111111-2222-3333-4444-555555555555" || up["filename"] != "apply.log" || up["file"] != "HTTP 422" {
		t.Errorf("upload = %v", up)
	}

	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if out["ticket"] != "HQ-2301" || out["url"] != "https://hq.altscore.ai/tickets/11111111-2222-3333-4444-555555555555" {
		t.Errorf("output = %v", out)
	}
	if !strings.Contains(stderr.String(), "Filed HQ-2301") {
		t.Errorf("stderr = %s", stderr.String())
	}

	// The same report again: refused locally, nothing sent.
	stdout.Reset()
	run.Ledger = loadFeedbackLedger(ledgerPath)
	run.Now = run.Now.Add(time.Hour)
	err := fileFeedback(newTestClient(t, srv.URL), run)
	if err == nil || !strings.Contains(err.Error(), "already filed as HQ-2301") {
		t.Fatalf("duplicate should be refused, got %v", err)
	}
	if fs.ticketCalls != 1 {
		t.Errorf("a refused duplicate must not reach the server (calls=%d)", fs.ticketCalls)
	}

	// --force files it anyway.
	run.Force = true
	if err := fileFeedback(newTestClient(t, srv.URL), run); err != nil {
		t.Fatal(err)
	}
	if fs.ticketCalls != 2 {
		t.Errorf("--force should file again (calls=%d)", fs.ticketCalls)
	}
}

func TestFileFeedback_DryRunSendsNothing(t *testing.T) {
	fs := &feedbackServer{}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()

	var stdout bytes.Buffer
	err := fileFeedback(newTestClient(t, srv.URL), feedbackRun{
		Input:  sampleFeedbackInput(),
		DryRun: true,
		Ledger: loadFeedbackLedger(""),
		Now:    time.Now(),
		Stdout: &stdout,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fs.ticketCalls != 0 {
		t.Error("--dry-run reached the server")
	}
	var out struct {
		DryRun  bool `json:"dryRun"`
		Request struct {
			Path string         `json:"path"`
			Body map[string]any `json:"body"`
		} `json:"request"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.DryRun || out.Request.Path != feedbackTicketsPath || out.Request.Body["status"] != "triage" {
		t.Errorf("dry run output = %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "<dev@altscore.ai>") {
		t.Error("dry-run output should not HTML-escape the description")
	}
}

func TestFileFeedback_ForbiddenExplainsStaffOnly(t *testing.T) {
	fs := &feedbackServer{ticketCode: http.StatusForbidden}
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()

	in := sampleFeedbackInput()
	in.Reporter.Email = "someone@customer.com"
	ledgerPath := filepath.Join(t.TempDir(), feedbackStateFile)
	err := fileFeedback(newTestClient(t, srv.URL), feedbackRun{
		Input:  in,
		Ledger: loadFeedbackLedger(ledgerPath),
		Now:    time.Now(),
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "AltScore staff") || !strings.Contains(err.Error(), "someone@customer.com") {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Stat(ledgerPath); statErr == nil {
		t.Error("a refused report must not be remembered, or the retry with staff credentials would be blocked")
	}
}
