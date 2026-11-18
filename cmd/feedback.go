package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/AltScore/altscore-cli/internal/version"
	"github.com/spf13/cobra"
)

// `altscore feedback` is the CLI's twin of the Hub's bug-report widget: it files
// an HQ ticket through the same Borrower Central route the Hub uses
// (POST /v1/support/tickets, which forwards the body to HQ's POST /v1/tickets).
// That route only accepts AltScore staff tokens (an @altscore.ai email claim),
// which is what CLI client credentials are.
//
// Every report lands in HQ's triage queue (status "triage", no project) for a
// human to route. The Hub sends workflow-area bugs straight to the Workflow
// Builder project; the CLI does not, because most reports come from agents and
// a person should look at them before they reach someone's board.

const (
	feedbackTicketsPath     = "/v1/support/tickets"
	feedbackAttachmentsPath = "/v1/support/attachments"
	hqTicketURLPrefix       = "https://hq.altscore.ai/tickets/"

	feedbackDefaultArea = "cli"
	feedbackRepoLabel   = "repo-altscore-cli"
	feedbackAgentLabel  = "reported-by-agent"

	feedbackMaxTitle        = 200
	feedbackMaxBody         = 20000
	feedbackMaxAttachment   = 25 << 20 // what HQ accepts; the Hub caps it the same way
	feedbackMaxContextValue = 500

	feedbackStateFile      = "feedback-sent.json"
	feedbackDedupWindow    = 7 * 24 * time.Hour
	feedbackDedupRetention = 30 * 24 * time.Hour
)

// feedbackKindLabels maps --kind to the HQ label that says where the ticket came
// from and what it is. The Hub files everything as `hub-bug` and uses
// `hub-enhancement` for suggestions; these are the CLI's parallel slugs.
var feedbackKindLabels = map[string]string{
	"bug":        "cli-bug",
	"suggestion": "cli-enhancement",
	"docs":       "cli-docs",
}

// feedbackAreas is the closed set of --area values: `cli` plus the Hub's product
// areas (altscore-ai-chat lib/support/product-areas.ts), so a CLI ticket carries
// the same `area:<slug>` label a Hub ticket about the same surface would.
var feedbackAreas = []string{
	"cli",
	"workflow-builder",
	"workflow-runtime",
	"altdata",
	"customers",
	"deals",
	"altcard",
	"conversational",
	"analytics",
	"geodata",
	"settings",
	"hub-platform",
}

// feedbackReservedContextKeys are filled in by the CLI. A --context of the same
// name is refused so a report cannot misstate who filed it or for which tenant.
var feedbackReservedContextKeys = map[string]bool{
	"reporter_email": true,
	"reporter_name":  true,
	"tenant_id":      true,
	"environment":    true,
	"product":        true,
	"reported_via":   true,
	"cli_version":    true,
	"os":             true,
	"profile":        true,
	"kind":           true,
	"agent":          true,
}

// feedbackMetaKeys are context keys rendered as their own line at the top of the
// description, so they are left out of the Context table.
var feedbackMetaKeys = map[string]bool{
	"reported_via":     true,
	"cli_version":      true,
	"agent":            true,
	"kind":             true,
	"workflow_alias":   true,
	"workflow_id":      true,
	"workflow_version": true,
	"execution_id":     true,
	"command":          true,
}

var (
	feedbackContextKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	feedbackAgentPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

	// Credentials an agent might paste from a debug session. The literal
	// client secret and access token of the active profile are scrubbed too.
	feedbackJWTPattern    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+`)
	feedbackBearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`)
	feedbackAssignPattern = regexp.MustCompile(`(?i)\b(client_secret|clientsecret|altscore_client_secret|altscore_user_token|access_token|api[_-]?key)(["']?\s*[:=]\s*["']?)([^\s"',\[][^\s"',]*)`)
)

type feedbackReporter struct {
	Email string
	Name  string
}

type feedbackAttachment struct {
	Name string
	Data []byte
}

// feedbackInput is everything a report is built from, already validated and
// scrubbed. buildFeedbackTicket is a pure function of it.
type feedbackInput struct {
	Title       string
	Body        string
	Kind        string
	Area        string
	Context     map[string]string // --context pairs
	Attachments []feedbackAttachment
	Reporter    feedbackReporter
	Tenant      string
	Environment string
	Profile     string
	Agent       string
	CLIVersion  string
	OS          string
	Arch        string
}

// mergedContext is the --context pairs plus what the CLI knows on its own.
func (in feedbackInput) mergedContext() map[string]string {
	ctx := make(map[string]string, len(in.Context)+6)
	for k, v := range in.Context {
		ctx[k] = v
	}
	ctx["reported_via"] = "altscore-cli"
	ctx["cli_version"] = in.CLIVersion
	ctx["os"] = in.OS + "/" + in.Arch
	ctx["profile"] = in.Profile
	ctx["kind"] = in.Kind
	if in.Agent != "" {
		ctx["agent"] = in.Agent
	}
	return ctx
}

func (in feedbackInput) labels() []string {
	labels := []string{feedbackKindLabels[in.Kind], "area:" + in.Area}
	if in.Area == feedbackDefaultArea {
		labels = append(labels, feedbackRepoLabel)
	}
	if in.Agent != "" {
		labels = append(labels, feedbackAgentLabel)
	}
	return labels
}

// buildFeedbackTicket is the body POSTed to /v1/support/tickets. It follows the
// Hub's createBugTicket (altscore-ai-chat app/(app)/support/hq.ts) field for
// field, except that it always files into triage.
func buildFeedbackTicket(in feedbackInput) map[string]any {
	ctx := in.mergedContext()
	custom := make(map[string]any, len(ctx)+5)
	for k, v := range ctx {
		custom[k] = v
	}
	custom["reporter_email"] = in.Reporter.Email
	custom["reporter_name"] = in.Reporter.Name
	custom["tenant_id"] = in.Tenant
	custom["environment"] = in.Environment
	custom["product"] = in.Area

	return map[string]any{
		"title":           in.Title,
		"description":     renderFeedbackDescription(in, ctx),
		"type":            "issue",
		"priority":        "medium",
		"source":          "api",
		"status":          "triage",
		"is_product_work": true,
		"labels":          in.labels(),
		"custom_fields":   custom,
	}
}

// renderFeedbackDescription is the ticket's markdown body, in the Hub's layout:
// a meta list, a Context table for everything else, a rule, then the report.
// HQ neither renders nor filters custom_fields, so anything left out of the
// description is invisible to whoever picks the ticket up.
func renderFeedbackDescription(in feedbackInput, ctx map[string]string) string {
	reporter := in.Reporter.Name
	if reporter == "" && in.Reporter.Email != "" {
		reporter = strings.SplitN(in.Reporter.Email, "@", 2)[0]
	}
	if in.Reporter.Email != "" {
		reporter = fmt.Sprintf("%s <%s>", reporter, in.Reporter.Email)
	}
	if reporter == "" {
		reporter = "unknown"
	}
	via := "altscore-cli " + in.CLIVersion
	if in.Agent != "" {
		via += ", filed by an agent (" + in.Agent + ")"
	}
	tenant := in.Tenant
	if tenant == "" {
		tenant = "unknown"
	}

	meta := []string{
		"- **Reported by:** " + reporter,
		"- **Via:** " + via,
		"- **Tenant:** " + tenant,
		"- **Environment:** " + in.Environment,
		"- **Area:** " + in.Area,
		"- **Kind:** " + in.Kind,
	}
	workflow := ctx["workflow_alias"]
	if workflow == "" {
		workflow = ctx["workflow_id"]
	}
	if workflow != "" {
		if v := ctx["workflow_version"]; v != "" {
			workflow += " v" + v
		}
		meta = append(meta, "- **Workflow:** "+workflow)
	}
	if id := ctx["execution_id"]; id != "" {
		meta = append(meta, "- **Execution:** "+id)
	}
	if command := ctx["command"]; command != "" {
		meta = append(meta, "- **Command:** "+markdownCodeSpan(command))
	}

	var keys []string
	for k, v := range ctx {
		if !feedbackMetaKeys[k] && v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	table := ""
	if len(keys) > 0 {
		rows := make([]string, len(keys))
		for i, k := range keys {
			rows[i] = fmt.Sprintf("| %s | %s |", k, feedbackTableValue(ctx[k]))
		}
		table = "\n\n**Context**\n\n| Field | Value |\n| --- | --- |\n" + strings.Join(rows, "\n")
	}

	return strings.Join(meta, "\n") + table + "\n\n---\n\n" + in.Body
}

func feedbackTableValue(v string) string {
	flat := strings.Join(strings.Fields(v), " ")
	flat = strings.ReplaceAll(flat, "|", `\|`)
	if len(flat) > feedbackMaxContextValue {
		flat = flat[:feedbackMaxContextValue] + "..."
	}
	return flat
}

// markdownCodeSpan wraps s in a code span that survives backticks inside it.
func markdownCodeSpan(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if !strings.Contains(s, "`") {
		return "`" + s + "`"
	}
	return "`` " + s + " ``"
}

func parseFeedbackContext(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("--context %q: expected key=value", p)
		}
		if !feedbackContextKeyPattern.MatchString(k) {
			return nil, fmt.Errorf("--context key %q: use lower_snake_case, e.g. execution_id", k)
		}
		if feedbackReservedContextKeys[k] {
			return nil, fmt.Errorf("--context %s is filled in by the CLI; drop it", k)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

func validateFeedbackArea(area string) error {
	for _, a := range feedbackAreas {
		if a == area {
			return nil
		}
	}
	return fmt.Errorf("--area %q is not one of: %s", area, strings.Join(feedbackAreas, ", "))
}

func validateFeedbackKind(kind string) error {
	if _, ok := feedbackKindLabels[kind]; ok {
		return nil
	}
	return fmt.Errorf("--kind %q is not one of: bug, suggestion, docs", kind)
}

// redactSecrets scrubs credentials out of free text and says how many it found.
// literals are the active profile's own secret and token, matched verbatim.
func redactSecrets(s string, literals []string) (string, int) {
	n := 0
	for _, lit := range literals {
		if len(lit) < 8 {
			continue
		}
		if c := strings.Count(s, lit); c > 0 {
			n += c
			s = strings.ReplaceAll(s, lit, "[REDACTED]")
		}
	}
	s = feedbackBearerPattern.ReplaceAllStringFunc(s, func(string) string {
		n++
		return "Bearer [REDACTED]"
	})
	s = feedbackJWTPattern.ReplaceAllStringFunc(s, func(string) string {
		n++
		return "[REDACTED]"
	})
	n += len(feedbackAssignPattern.FindAllStringIndex(s, -1))
	s = feedbackAssignPattern.ReplaceAllString(s, "${1}${2}[REDACTED]")
	return s, n
}

// containsSecret is redactSecrets' check for attachments, which are sent as
// they are: a file that holds a credential is refused rather than altered.
func containsSecret(data []byte, literals []string) bool {
	for _, lit := range literals {
		if len(lit) >= 8 && bytes.Contains(data, []byte(lit)) {
			return true
		}
	}
	return feedbackJWTPattern.Match(data) || feedbackBearerPattern.Match(data) || feedbackAssignPattern.Match(data)
}

func loadFeedbackAttachments(paths []string, literals []string) ([]feedbackAttachment, error) {
	out := make([]feedbackAttachment, 0, len(paths))
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("--attach %s: %w", p, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("--attach %s is a directory; attach files", p)
		}
		if info.Size() > feedbackMaxAttachment {
			return nil, fmt.Errorf("--attach %s is %d MB; the limit is 25 MB", p, info.Size()>>20)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("--attach %s: %w", p, err)
		}
		if containsSecret(data, literals) {
			return nil, fmt.Errorf("--attach %s looks like it holds a credential (a token, client secret or API key); remove it from the file and retry", p)
		}
		out = append(out, feedbackAttachment{Name: filepath.Base(p), Data: data})
	}
	return out, nil
}

// readFeedbackBody takes --body text, --body @file, or stdin when it is piped.
func readFeedbackBody(flag string, stdin io.Reader, stdinIsTerminal bool) (string, error) {
	if strings.HasPrefix(flag, "@") {
		path := strings.TrimPrefix(flag, "@")
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read --body file %q: %w", path, err)
		}
		return string(data), nil
	}
	if flag != "" {
		return flag, nil
	}
	if stdinIsTerminal {
		return "", nil
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("cannot read the report from stdin: %w", err)
	}
	return string(data), nil
}

// tokenIdentity reads who the token belongs to. The signature is not checked:
// the claims only label the report, and Borrower Central re-reads the token.
func tokenIdentity(token string) feedbackReporter {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return feedbackReporter{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return feedbackReporter{}
	}
	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return feedbackReporter{}
	}
	return feedbackReporter{Email: claims.Email, Name: claims.Name}
}

// detectFeedbackAgent names the agent running the CLI, if any. ALTSCORE_AGENT
// lets any agent say what it is; Claude Code is recognized on its own.
func detectFeedbackAgent(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv("ALTSCORE_AGENT")); v != "" && feedbackAgentPattern.MatchString(v) {
		return v
	}
	if getenv("CLAUDECODE") == "1" {
		return "claude-code"
	}
	return ""
}

// feedbackFingerprint identifies "the same report" for duplicate detection:
// same kind, area, backend and tenant, and the same title up to case and spacing.
func feedbackFingerprint(in feedbackInput) string {
	title := strings.Join(strings.Fields(strings.ToLower(in.Title)), " ")
	sum := sha256.Sum256([]byte(strings.Join([]string{in.Kind, in.Area, in.Environment, in.Tenant, title}, "\x00")))
	return hex.EncodeToString(sum[:])
}

type feedbackSent struct {
	Ticket string    `json:"ticket"`
	URL    string    `json:"url"`
	At     time.Time `json:"at"`
}

// feedbackLedger remembers recent reports so an agent stuck in a loop files one
// ticket, not twenty. A missing or unreadable file is an empty ledger, and a
// ledger with no path remembers nothing.
type feedbackLedger struct {
	path    string
	entries map[string]feedbackSent
}

func loadFeedbackLedger(path string) *feedbackLedger {
	l := &feedbackLedger{path: path, entries: map[string]feedbackSent{}}
	if path == "" {
		return l
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return l
	}
	_ = json.Unmarshal(data, &l.entries)
	if l.entries == nil {
		l.entries = map[string]feedbackSent{}
	}
	return l
}

func (l *feedbackLedger) recent(fingerprint string, now time.Time) (feedbackSent, bool) {
	s, ok := l.entries[fingerprint]
	if !ok || now.Sub(s.At) > feedbackDedupWindow {
		return feedbackSent{}, false
	}
	return s, true
}

func (l *feedbackLedger) record(fingerprint string, s feedbackSent, now time.Time) error {
	if l.path == "" {
		return nil
	}
	for k, v := range l.entries {
		if now.Sub(v.At) > feedbackDedupRetention {
			delete(l.entries, k)
		}
	}
	l.entries[fingerprint] = s
	data, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.path, data, 0o600)
}

type feedbackRun struct {
	Input  feedbackInput
	DryRun bool
	Force  bool
	Ledger *feedbackLedger
	Now    time.Time
	Stdout io.Writer
	Stderr io.Writer
}

func writeFeedbackJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("cannot encode output: %w", err)
	}
	return nil
}

func feedbackAttachmentNames(atts []feedbackAttachment) []string {
	names := make([]string, len(atts))
	for i, a := range atts {
		names[i] = a.Name
	}
	return names
}

// fileFeedback sends the report: the ticket first, then each attachment against
// the new ticket id, which is how the Hub does it.
func fileFeedback(c *client.Client, r feedbackRun) error {
	in := r.Input
	payload := buildFeedbackTicket(in)
	fingerprint := feedbackFingerprint(in)
	prior, duplicate := r.Ledger.recent(fingerprint, r.Now)

	if r.DryRun {
		out := map[string]any{
			"dryRun":      true,
			"request":     map[string]any{"method": "POST", "path": feedbackTicketsPath, "body": payload},
			"attachments": feedbackAttachmentNames(in.Attachments),
		}
		if duplicate {
			out["duplicateOf"] = prior
		}
		return writeFeedbackJSON(r.Stdout, out)
	}

	if duplicate && !r.Force {
		return fmt.Errorf("this report was already filed as %s on %s (%s). Add what is new as a comment on that ticket, or pass --force to file a second one",
			prior.Ticket, prior.At.Format("2006-01-02"), prior.URL)
	}

	raw, status, err := c.DoKeepBody("POST", "borrower_central", feedbackTicketsPath, payload)
	if err != nil {
		return err
	}
	if status >= 400 {
		return describeFeedbackError(status, raw, in.Reporter.Email)
	}
	var ticket struct {
		ID           string `json:"id"`
		TicketNumber int    `json:"ticket_number"`
		Status       string `json:"status"`
	}
	if err := json.Unmarshal(raw, &ticket); err != nil || ticket.ID == "" {
		return fmt.Errorf("HQ answered HTTP %d without a ticket id: %s", status, truncateForError(string(raw)))
	}
	key := ticket.ID
	if ticket.TicketNumber > 0 {
		key = fmt.Sprintf("HQ-%d", ticket.TicketNumber)
	}
	url := hqTicketURLPrefix + ticket.ID

	// Remembered before the uploads: the ticket exists now, so a failed
	// attachment must not make a retry file the same report twice.
	if err := r.Ledger.record(fingerprint, feedbackSent{Ticket: key, URL: url, At: r.Now}, r.Now); err != nil {
		fmt.Fprintf(r.Stderr, "warning: could not remember this report for duplicate detection: %v\n", err)
	}

	uploaded := []string{}
	failed := []string{}
	for _, a := range in.Attachments {
		if err := uploadFeedbackAttachment(c, ticket.ID, a); err != nil {
			fmt.Fprintf(r.Stderr, "warning: attachment %s was not uploaded: %v\n", a.Name, err)
			failed = append(failed, a.Name)
			continue
		}
		uploaded = append(uploaded, a.Name)
	}

	fmt.Fprintf(r.Stderr, "Filed %s in the HQ triage queue: %s\n", key, url)
	return writeFeedbackJSON(r.Stdout, map[string]any{
		"ticket":            key,
		"id":                ticket.ID,
		"url":               url,
		"status":            ticket.Status,
		"labels":            in.labels(),
		"attachments":       uploaded,
		"attachmentsFailed": failed,
	})
}

func describeFeedbackError(status int, raw []byte, email string) error {
	if status == http.StatusForbidden {
		who := "carries no email"
		if email != "" {
			who = "belongs to " + email
		}
		return fmt.Errorf("Borrower Central refused the report (HTTP 403). It files HQ tickets only for AltScore staff, and this profile's token %s. Log in with your own @altscore.ai client credentials (altscore login --profile <alias>) or report it from the Hub", who)
	}
	return fmt.Errorf("HTTP %d filing the report: %s", status, truncateForError(string(raw)))
}

func uploadFeedbackAttachment(c *client.Client, ticketID string, a feedbackAttachment) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("owner_type", "ticket"); err != nil {
		return err
	}
	if err := w.WriteField("owner_id", ticketID); err != nil {
		return err
	}
	contentType := mime.TypeByExtension(filepath.Ext(a.Name))
	if contentType == "" {
		contentType = http.DetectContentType(a.Data)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, quoteFormValue(a.Name)))
	header.Set("Content-Type", contentType)
	part, err := w.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(a.Data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	body := buf.Bytes()
	// Buffered, so every replay (401 refresh, connect-phase retry) sends the
	// same bytes under the boundary already named in the Content-Type.
	_, _, err = c.DoRaw("POST", "borrower_central", feedbackAttachmentsPath, func() (io.Reader, error) {
		return bytes.NewReader(body), nil
	}, w.FormDataContentType())
	return err
}

func quoteFormValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

func makeFeedbackCmd() *cobra.Command {
	var (
		title, body, kind, area string
		contextPairs, attach    []string
		dryRun, force           bool
	)

	cmd := &cobra.Command{
		Use:     "feedback",
		Aliases: []string{"report-bug"},
		Short:   "Report a bug, suggestion or docs problem to the AltScore team (files an HQ ticket)",
		Long: `Report a problem with the CLI, its skill and docs, or the platform behind it.

The report becomes a ticket in HQ's triage queue, the same way the Hub's
"report a problem" widget files one, and a person routes it from there.
It goes through Borrower Central (POST /v1/support/tickets), which accepts
AltScore staff credentials only: the profile's token must carry an
@altscore.ai email.

The CLI fills in on its own: who reported it (from the token), the tenant,
the environment, the profile, the CLI version, the OS, and whether an agent
filed it (Claude Code is detected; any other agent can set ALTSCORE_AGENT).
Add what only you know with --context: execution_id, workflow_alias,
workflow_id, workflow_version and command get their own line in the ticket,
anything else goes in its Context table.

Kinds (label):
  bug          something does not work as documented    (cli-bug)
  suggestion   a missing command, flag or capability    (cli-enhancement)
  docs         the skill, --help or schema-guide is wrong or misleading (cli-docs)

Areas (label area:<area>): cli (default: the CLI itself, its skill and docs),
  workflow-builder, workflow-runtime, altdata, customers, deals, altcard,
  conversational, analytics, geodata, settings, hub-platform

A good report says what you ran, what you expected, what happened instead
(the exact error), and how to reproduce it. Long logs go in --attach.

Safety:
  - Tokens, client secrets and API keys in the title, body or context are
    replaced with [REDACTED] before sending; an attachment that holds one is
    refused.
  - The same report (kind, area, tenant, environment and title) is filed once
    per 7 days. A repeat names the earlier ticket instead; --force files it again.
  - --dry-run prints the exact request and sends nothing.

Output: {"ticket": "HQ-1234", "id", "url", "status", "labels",
"attachments", "attachmentsFailed"} on stdout.`,
		Example: `  # A CLI bug, with the failing command and the execution it touched
  altscore feedback --title "executions state returns 404 for a v2 execution" \
    --body "Ran ... Expected the node outputs, got HTTP 404 ..." \
    --context command="altscore executions state 6f2c..." --context execution_id=6f2c...

  # A docs problem, body from a file, with a log attached
  altscore feedback --kind docs --title "schema-guide lists a field apply rejects" \
    --body @report.md --attach apply-output.txt

  # A platform problem seen through the CLI
  altscore feedback --area workflow-runtime --title "AltData node succeeds with an empty payload" \
    --body @report.md --context workflow_alias=onboarding-kyc

  # Check what would be sent
  altscore feedback --title "..." --body "..." --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			kind = strings.ToLower(strings.TrimSpace(kind))
			area = strings.ToLower(strings.TrimSpace(area))
			if err := validateFeedbackKind(kind); err != nil {
				return err
			}
			if err := validateFeedbackArea(area); err != nil {
				return err
			}
			title = strings.Join(strings.Fields(title), " ")
			if title == "" {
				return fmt.Errorf("--title is required: one line that says what is wrong")
			}
			if len([]rune(title)) > feedbackMaxTitle {
				return fmt.Errorf("--title is %d characters; keep it under %d and put the detail in --body", len([]rune(title)), feedbackMaxTitle)
			}
			text, err := readFeedbackBody(body, os.Stdin, isStdinTerminal())
			if err != nil {
				return err
			}
			text = strings.TrimSpace(text)
			if text == "" {
				return fmt.Errorf("--body is required (text, @file, or piped on stdin): what you ran, what you expected, what happened")
			}
			context, err := parseFeedbackContext(contextPairs)
			if err != nil {
				return err
			}

			c, err := loadClient()
			if err != nil {
				return err
			}
			literals := []string{c.Profile.ClientSecret, c.Profile.AccessToken}

			redacted := 0
			var n int
			title, n = redactSecrets(title, literals)
			redacted += n
			text, n = redactSecrets(text, literals)
			redacted += n
			for k, v := range context {
				context[k], n = redactSecrets(v, literals)
				redacted += n
			}
			if redacted > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "Replaced %d credential-looking value(s) with [REDACTED] before sending.\n", redacted)
			}
			if len([]rune(text)) > feedbackMaxBody {
				return fmt.Errorf("--body is %d characters; keep it under %d and put logs in --attach", len([]rune(text)), feedbackMaxBody)
			}

			attachments, err := loadFeedbackAttachments(attach, literals)
			if err != nil {
				return err
			}

			tenant := c.Profile.TenantID
			if tenant == "" {
				tenant = tenantFromToken(c.Profile.AccessToken)
			}

			ledgerPath, err := config.StateFilePath(feedbackStateFile)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: duplicate detection is off: %v\n", err)
				ledgerPath = ""
			}

			return fileFeedback(c, feedbackRun{
				Input: feedbackInput{
					Title:       title,
					Body:        text,
					Kind:        kind,
					Area:        area,
					Context:     context,
					Attachments: attachments,
					Reporter:    tokenIdentity(c.Profile.AccessToken),
					Tenant:      tenant,
					Environment: c.Profile.Environment,
					Profile:     c.ProfileName,
					Agent:       detectFeedbackAgent(os.Getenv),
					CLIVersion:  version.Version,
					OS:          runtime.GOOS,
					Arch:        runtime.GOARCH,
				},
				DryRun: dryRun,
				Force:  force,
				Ledger: loadFeedbackLedger(ledgerPath),
				Now:    time.Now().UTC(),
				Stdout: cmd.OutOrStdout(),
				Stderr: cmd.ErrOrStderr(),
			})
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "one-line summary [required]")
	cmd.Flags().StringVar(&body, "body", "", "the report: text, @file, or piped on stdin [required]")
	cmd.Flags().StringVar(&kind, "kind", "bug", "bug, suggestion or docs")
	cmd.Flags().StringVar(&area, "area", feedbackDefaultArea, "product area the report is about (see --help)")
	cmd.Flags().StringArrayVar(&contextPairs, "context", nil, "extra context as key=value, e.g. execution_id=... (repeatable)")
	cmd.Flags().StringArrayVar(&attach, "attach", nil, "file to attach to the ticket, max 25 MB (repeatable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the request and send nothing")
	cmd.Flags().BoolVar(&force, "force", false, "file it even if the same report was filed in the last 7 days")
	return cmd
}
