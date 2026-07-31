package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// article returns "an" if the word starts with a vowel, "a" otherwise.
func article(word string) string {
	if len(word) == 0 {
		return "a"
	}
	switch word[0] {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return "an"
	default:
		return "a"
	}
}

// ResourceDef defines a REST resource that can be registered as a set of Cobra subcommands.
type ResourceDef struct {
	Name           string                                // plural: "borrowers"
	Singular       string                                // "borrower"
	BasePath       string                                // "/v1/borrowers"
	Module         string                                // "borrower_central"
	Actions        []string                              // subset of: "list", "get", "create", "update", "delete"
	Description    string                                // long description of the resource
	CreateSchema   string                                // documents the JSON body for create
	UpdateSchema   string                                // documents the JSON body for update
	ResponseSchema string                                // documents the fields in GET responses
	FilterHelp     string                                // documents query parameters for list
	HasTestMode    bool                                  // adds set-test command + --include-tests/--test-only on list + --is-test on create
	HasTestFilter  bool                                  // adds only --include-tests/--test-only on list (no set-test, no create flag)
	WorkflowAlias  bool                                  // adds --workflow-alias <alias> on create/update; injects "workflowAlias" into the body. Without it the entity will not appear in the workflow builder's pickers.
	BodyValidator  func(body *json.RawMessage) error     // optional hook called before POST/PATCH; may normalize the body in place; aborts the request if it returns an error
}

// registerResource creates a Cobra command group for the resource and adds
// subcommands for each action.
func registerResource(def ResourceDef) *cobra.Command {
	groupCmd := &cobra.Command{
		Use:   def.Name,
		Short: fmt.Sprintf("Manage %s", def.Name),
		Long:  def.Description,
	}

	for _, action := range def.Actions {
		switch action {
		case "list":
			groupCmd.AddCommand(makeListCmd(def))
		case "get":
			groupCmd.AddCommand(makeGetCmd(def))
		case "create":
			groupCmd.AddCommand(makeCreateCmd(def))
		case "update":
			groupCmd.AddCommand(makeUpdateCmd(def))
		case "delete":
			groupCmd.AddCommand(makeDeleteCmd(def))
		}
	}

	if def.HasTestMode {
		groupCmd.AddCommand(makeSetTestCmd(def))
	}

	rootCmd.AddCommand(groupCmd)
	return groupCmd
}

func makeListCmd(def ResourceDef) *cobra.Command {
	var filters []string
	var perPage int
	var page int
	var includeTests bool
	var testOnly bool

	hasTestFlags := def.HasTestMode || def.HasTestFilter

	long := fmt.Sprintf(`Query %s. Returns a paginated JSON array.

Use --filter for field-based filters, --per-page and --page for pagination.`, def.Name)

	if hasTestFlags {
		long += "\n\nTest records are excluded by default. Use --include-tests or --test-only to see them."
	}
	if def.FilterHelp != "" {
		long += "\n\nAvailable filters (pass via --filter key=value):\n" + def.FilterHelp
	}
	if def.ResponseSchema != "" {
		long += "\n\nResponse fields:\n" + def.ResponseSchema
	}

	cmd := &cobra.Command{
		Use:   "list",
		Short: fmt.Sprintf("List %s", def.Name),
		Long:  long,
		Example: fmt.Sprintf(`  # List first 10 %s
  altscore %s list --per-page 10

  # With filter
  altscore %s list --filter status=active

  # Pipe to jq
  altscore %s list | jq '.[].id'`, def.Name, def.Name, def.Name, def.Name),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := def.BasePath
			params := []string{}

			if hasTestFlags {
				if includeTests && testOnly {
					return fmt.Errorf("--include-tests and --test-only are mutually exclusive")
				}
				if includeTests {
					params = append(params, "include-tests=true")
				}
				if testOnly {
					params = append(params, "test-only=true")
				}
			}

			if perPage > 0 {
				params = append(params, fmt.Sprintf("per-page=%d", perPage))
			} else if c.Config.Defaults.PerPage > 0 {
				params = append(params, fmt.Sprintf("per-page=%d", c.Config.Defaults.PerPage))
			}
			if page > 0 {
				params = append(params, fmt.Sprintf("page=%d", page))
			}

			for _, f := range filters {
				// Auto-translate the most common test-mode filter mistake. Agents
				// reach for --filter is-test=true before discovering --test-only,
				// and the backend silently drops unknown filter keys -- so this
				// would return non-test records without complaint. On resources
				// that support test mode we translate to the canonical query
				// params; on resources that don't, we let the filter fall through
				// (it might be a legitimate backend filter we don't know about).
				if hasTestFlags {
					norm := strings.ToLower(strings.TrimSpace(f))
					switch norm {
					case "is-test=true", "istest=true", "is_test=true":
						if includeTests || testOnly {
							return fmt.Errorf("--filter %s conflicts with --include-tests/--test-only; use only one", f)
						}
						params = append(params, "test-only=true")
						fmt.Fprintf(cmd.OutOrStderr(), "# auto-translating --filter %s to --test-only (the canonical form for this resource)\n", f)
						continue
					case "is-test=false", "istest=false", "is_test=false":
						// Default behavior already excludes test records; nothing to add.
						fmt.Fprintf(cmd.OutOrStderr(), "# --filter %s is a no-op (test records are excluded by default)\n", f)
						continue
					}
				}
				params = append(params, f)
			}

			if len(params) > 0 {
				path += "?" + strings.Join(params, "&")
			}

			data, _, err := c.Do("GET", def.Module, path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringArrayVar(&filters, "filter", nil, "field filter in key=value format (repeatable)")
	cmd.Flags().IntVar(&perPage, "per-page", 0, "items per page (default: from config)")
	cmd.Flags().IntVar(&page, "page", 0, "page number (default: 1)")
	if hasTestFlags {
		cmd.Flags().BoolVar(&includeTests, "include-tests", false, "include test records in results")
		cmd.Flags().BoolVar(&testOnly, "test-only", false, "return only test records")
	}

	return cmd
}

func makeGetCmd(def ResourceDef) *cobra.Command {
	long := fmt.Sprintf("Retrieve a single %s by its ID. Returns a JSON object.", def.Singular)
	if def.ResponseSchema != "" {
		long += "\n\nResponse fields:\n" + def.ResponseSchema
	}

	return &cobra.Command{
		Use:   "get <id>",
		Short: fmt.Sprintf("Get %s %s by ID", article(def.Singular), def.Singular),
		Long:  long,
		Example: fmt.Sprintf(`  altscore %s get <id>
  altscore %s get <id> | jq '.status'`, def.Name, def.Name),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", def.Module, def.BasePath+"/"+args[0], nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeCreateCmd(def ResourceDef) *cobra.Command {
	var bodyFlag string
	var isTest bool
	var workflowAlias string

	long := fmt.Sprintf(`Create a new %s. Pass the JSON body via --body or stdin.

When --body is omitted and stdin is not a terminal, the body is read from stdin.
This allows piping JSON: echo '{"key":"value"}' | altscore %s create`, def.Singular, def.Name)

	if def.HasTestMode {
		long += "\n\nUse --is-test to create the record as a test entity."
	}
	if def.WorkflowAlias {
		long += "\n\nUse --workflow-alias <alias> to scope this entity to a workflow.\nWithout it the entity will NOT appear in that workflow's builder pickers\n(rule trees / scorecards / mapping tables / evaluation rules are surfaced\nin the v2 builder by matching workflowAlias)."
	}
	if def.CreateSchema != "" {
		long += "\n\nRequest body fields:\n" + def.CreateSchema
	}
	if def.ResponseSchema != "" {
		long += "\n\nResponse fields:\n" + def.ResponseSchema
	}

	cmd := &cobra.Command{
		Use:   "create",
		Short: fmt.Sprintf("Create %s %s", article(def.Singular), def.Singular),
		Long:  long,
		Example: fmt.Sprintf(`  # Inline JSON
  altscore %s create --body '{"label": "test"}'

  # From stdin
  echo '{"label": "test"}' | altscore %s create

  # From file
  altscore %s create --body "$(cat data.json)"`, def.Name, def.Name, def.Name),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			if def.HasTestMode && isTest {
				body, err = jsonSetBool(body, "isTest", true)
				if err != nil {
					return err
				}
			}

			if def.WorkflowAlias && workflowAlias != "" {
				body, err = jsonSetString(body, "workflowAlias", workflowAlias)
				if err != nil {
					return err
				}
			} else if def.WorkflowAlias && !bodyHasKey(body, "workflowAlias") {
				fmt.Fprintf(cmd.OutOrStderr(),
					"# warning: --workflow-alias not set and body has no \"workflowAlias\"; this %s will not appear in any workflow builder.\n",
					def.Singular)
			}

			if def.BodyValidator != nil {
				if err := def.BodyValidator(&body); err != nil {
					return err
				}
			}

			path := def.BasePath

			data, _, err := c.Do("POST", def.Module, path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	if def.HasTestMode {
		cmd.Flags().BoolVar(&isTest, "is-test", false, "create as a test record")
	}
	if def.WorkflowAlias {
		cmd.Flags().StringVar(&workflowAlias, "workflow-alias", "", "scope this entity to a workflow alias (required for it to appear in that workflow's builder pickers)")
	}

	return cmd
}

func makeUpdateCmd(def ResourceDef) *cobra.Command {
	var bodyFlag string
	var workflowAlias string

	long := fmt.Sprintf(`Update %s %s by ID. Pass a partial JSON body via --body or stdin.

When --body is omitted and stdin is not a terminal, the body is read from stdin.`, article(def.Singular), def.Singular)

	if def.WorkflowAlias {
		long += "\n\nUse --workflow-alias <alias> to (re)scope this entity to a workflow.\nThe value is merged into the JSON body as workflowAlias."
	}
	if def.UpdateSchema != "" {
		long += "\n\nRequest body fields:\n" + def.UpdateSchema
	}
	if def.ResponseSchema != "" {
		long += "\n\nResponse fields:\n" + def.ResponseSchema
	}

	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: fmt.Sprintf("Update %s %s", article(def.Singular), def.Singular),
		Long:  long,
		Example: fmt.Sprintf(`  altscore %s update <id> --body '{"status": "active"}'
  echo '{"status": "active"}' | altscore %s update <id>`, def.Name, def.Name),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			if def.WorkflowAlias && workflowAlias != "" {
				body, err = jsonSetString(body, "workflowAlias", workflowAlias)
				if err != nil {
					return err
				}
			}

			if def.BodyValidator != nil {
				if err := def.BodyValidator(&body); err != nil {
					return err
				}
			}

			data, _, err := c.Do("PATCH", def.Module, def.BasePath+"/"+args[0], body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	if def.WorkflowAlias {
		cmd.Flags().StringVar(&workflowAlias, "workflow-alias", "", "scope (or re-scope) this entity to a workflow alias")
	}
	return cmd
}

func makeDeleteCmd(def ResourceDef) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: fmt.Sprintf("Delete %s %s", article(def.Singular), def.Singular),
		Long:  fmt.Sprintf("Delete %s %s by ID. Returns empty on success (HTTP 204).", article(def.Singular), def.Singular),
		Example: fmt.Sprintf(`  altscore %s delete <id>`, def.Name),
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			_, status, err := c.Do("DELETE", def.Module, def.BasePath+"/"+args[0], nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Deleted (HTTP %d).\n", status)
			return nil
		},
	}
}

// makeDocUploadCmd creates the "documents upload" command for file attachments.
func makeDocUploadCmd() *cobra.Command {
	var filePath string

	cmd := &cobra.Command{
		Use:   "upload <document-id>",
		Short: "Upload a file attachment to a document",
		Long: `Upload a file attachment to an existing document by its ID.

The file is sent as a multipart form upload to the document's attachment endpoint.`,
		Example: `  altscore documents upload <doc-id> --file ./invoice.pdf`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if filePath == "" {
				return fmt.Errorf("--file is required")
			}

			c, err := loadClient()
			if err != nil {
				return err
			}

			f, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("cannot open file: %w", err)
			}
			defer f.Close()

			filename := filepath.Base(filePath)

			pr, pw := io.Pipe()
			w := multipart.NewWriter(pw)

			go func() {
				part, err := w.CreateFormFile("file", filename)
				if err != nil {
					pw.CloseWithError(err)
					return
				}
				if _, err := io.Copy(part, f); err != nil {
					pw.CloseWithError(err)
					return
				}
				pw.CloseWithError(w.Close())
			}()

			// POST .../attachments/upload -- the singular PUT .../attachment this
			// used to send has never existed in borrower-central, so the command
			// always failed. See app/api/documents/handler.py:172, which takes the
			// same multipart "file" field.
			path := "/v1/documents/" + args[0] + "/attachments/upload"
			contentType := w.FormDataContentType()

			respBody, _, err := c.DoRaw("POST", "borrower_central", path, pr, contentType)
			if err != nil {
				return err
			}

			if len(respBody) > 0 {
				return output.RawJSON(json.RawMessage(respBody))
			}
			fmt.Fprintln(os.Stderr, "Upload complete.")
			return nil
		},
	}

	cmd.Flags().StringVar(&filePath, "file", "", "path to the file to upload [required]")
	return cmd
}

// readBody reads JSON from --body flag or stdin.
// Supports curl-style @filename for the --body flag (e.g. --body @spec.json).
func readBody(bodyFlag string) (json.RawMessage, error) {
	if bodyFlag != "" {
		if strings.HasPrefix(bodyFlag, "@") {
			path := strings.TrimPrefix(bodyFlag, "@")
			fileBytes, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("cannot read --body file %q: %w", path, err)
			}
			var raw json.RawMessage
			if err := json.Unmarshal(fileBytes, &raw); err != nil {
				return nil, fmt.Errorf("invalid JSON in %s: %w", path, err)
			}
			return raw, nil
		}
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(bodyFlag), &raw); err != nil {
			return nil, fmt.Errorf("invalid JSON in --body: %w", err)
		}
		return raw, nil
	}

	// Check if stdin has data (not a terminal)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("cannot read stdin: %w", err)
		}
		data = []byte(strings.TrimSpace(string(data)))
		if len(data) == 0 {
			return nil, fmt.Errorf("no JSON body provided (use --body or pipe via stdin)")
		}
		var raw json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("invalid JSON from stdin: %w", err)
		}
		return raw, nil
	}

	return nil, fmt.Errorf("no JSON body provided (use --body or pipe via stdin)")
}

func makeSetTestCmd(def ResourceDef) *cobra.Command {
	var enable bool
	var disable bool

	cmd := &cobra.Command{
		Use:   "set-test <id>",
		Short: fmt.Sprintf("Toggle test mode on %s %s", article(def.Singular), def.Singular),
		Long: fmt.Sprintf(`Set or clear the isTest flag on %s %s.

Use --enable to mark as test, --disable to clear the test flag.
When toggling a parent entity (borrower, deal, asset), the change
cascades to child records automatically.`, article(def.Singular), def.Singular),
		Example: fmt.Sprintf(`  # Mark as test
  altscore %s set-test <id> --enable

  # Clear test flag
  altscore %s set-test <id> --disable`, def.Name, def.Name),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !enable && !disable {
				return fmt.Errorf("specify --enable or --disable")
			}
			if enable && disable {
				return fmt.Errorf("cannot use both --enable and --disable")
			}

			c, err := loadClient()
			if err != nil {
				return err
			}

			body := json.RawMessage(fmt.Sprintf(`{"isTest":%t}`, enable))
			path := def.BasePath + "/" + args[0] + "/is-test"

			_, status, err := c.Do("PUT", def.Module, path, body)
			if err != nil {
				return err
			}
			if enable {
				fmt.Fprintf(os.Stderr, "Marked as test (HTTP %d).\n", status)
			} else {
				fmt.Fprintf(os.Stderr, "Cleared test flag (HTTP %d).\n", status)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&enable, "enable", false, "mark as test record")
	cmd.Flags().BoolVar(&disable, "disable", false, "clear test flag")
	return cmd
}

// jsonSetBool merges a boolean field into a JSON object.
func jsonSetBool(raw json.RawMessage, key string, value bool) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("body must be a JSON object to set %s: %w", key, err)
	}
	if value {
		obj[key] = json.RawMessage("true")
	} else {
		obj[key] = json.RawMessage("false")
	}
	return json.Marshal(obj)
}

// jsonSetString merges a string field into a JSON object. Caller-supplied keys
// in the body win on collision -- if the body already has the key, we leave it
// alone (so a flag never silently overrides an explicit body value).
func jsonSetString(raw json.RawMessage, key, value string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("body must be a JSON object to set %s: %w", key, err)
	}
	if _, ok := obj[key]; ok {
		return raw, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	obj[key] = encoded
	return json.Marshal(obj)
}

// bodyHasKey reports whether a top-level JSON-object key is present and non-empty.
func bodyHasKey(raw json.RawMessage, key string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	v, ok := obj[key]
	if !ok {
		return false
	}
	s := strings.TrimSpace(string(v))
	return s != "" && s != `""` && s != "null"
}
