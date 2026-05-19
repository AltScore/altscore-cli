package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// Custom subcommands for the credit-decisioning resources (mapping-tables,
// scorecards, evaluation-rules, rule-trees) that go beyond standard CRUD.
// Mirrors the factory pattern in cmd/workflow_tasks.go: each makeXxxCmd
// returns a *cobra.Command and is wired in cmd/root.go via .AddCommand on
// the resource group.

// stampWorkflowAliasOnArray walks a JSON array body and sets/overwrites
// workflowAlias on each top-level object. Used by the import commands so a
// single --workflow-alias flag can scope an entire bundle without editing
// the source file.
//
// When alias is empty AND the body's items don't already carry a
// workflowAlias, prints a warning to stderr (mirrors the --workflow-alias
// missing warning in resource.go::makeCreateCmd). entityKind is the resource
// name in plural form, used in the warning text.
func stampWorkflowAliasOnArray(body json.RawMessage, alias, entityKind string, warnWriter io.Writer) (json.RawMessage, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil {
		// Body isn't a JSON array; let the API decide what to do (some import
		// endpoints accept objects with an `items` key). No alias stamping
		// possible without an array shape.
		if alias != "" {
			return nil, fmt.Errorf("--workflow-alias requires the import body to be a JSON array of objects: %w", err)
		}
		return body, nil
	}

	if alias == "" {
		// Warn only if at least one item lacks a workflowAlias of its own; an
		// already-stamped bundle is fine without the flag.
		anyMissing := false
		for _, item := range items {
			v, has := item["workflowAlias"]
			if !has {
				anyMissing = true
				break
			}
			s := strings.TrimSpace(string(v))
			if s == "" || s == `""` || s == "null" {
				anyMissing = true
				break
			}
		}
		if anyMissing && warnWriter != nil {
			fmt.Fprintf(warnWriter,
				"# warning: --workflow-alias not set and at least one imported %s has no \"workflowAlias\"; those records will not appear in any workflow builder.\n",
				entityKind)
		}
		return body, nil
	}

	encoded, err := json.Marshal(alias)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i]["workflowAlias"] = encoded
	}
	return json.Marshal(items)
}

// ----- evaluation-rules -----

func makeErImportCmd() *cobra.Command {
	var bodyFlag string
	var workflowAlias string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Bulk-import evaluation rules from a JSON bundle",
		Long: `POST /v1/evaluation-rules/import. Body must be a top-level JSON
ARRAY of rule definitions -- NOT an object wrapper. e.g.
  [{"label": "Approve", "code": "approve", "conditions": {...}, ...}, ...]
A bundle-shaped body like {"rules": [...]} or {"items": [...]} is rejected
by the endpoint with an opaque parse error.

Use --workflow-alias <alias> to stamp every imported rule with the same
workflowAlias. Without it the imports will not appear in the workflow's
evaluate-rules picker.`,
		Example: `  altscore evaluation-rules import --body @rules-array.json --workflow-alias underwriting-v1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			body, err = stampWorkflowAliasOnArray(body, workflowAlias, "evaluation-rules", cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", "borrower_central", "/v1/evaluation-rules/import", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	cmd.Flags().StringVar(&workflowAlias, "workflow-alias", "", "stamp workflowAlias on every imported rule")
	return cmd
}

func makeErHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "history <id>",
		Short:   "Show the version history for an evaluation rule",
		Long:    `GET /v1/evaluation-rules/{id}/history. Returns chronological revisions.`,
		Example: `  altscore evaluation-rules history <id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v1/evaluation-rules/%s/history", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

// ----- rule-trees -----

func makeRtImportCmd() *cobra.Command {
	var bodyFlag string
	var workflowAlias string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Bulk-import rule trees from a JSON bundle",
		Long: `POST /v1/rule-trees/import.

Use --workflow-alias <alias> to stamp every imported rule tree with the
same workflowAlias. Without it the imports will not appear in the workflow's
rule-tree picker.`,
		Example: `  altscore rule-trees import --body @rule-trees.json --workflow-alias underwriting-v1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			body, err = stampWorkflowAliasOnArray(body, workflowAlias, "rule-trees", cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", "borrower_central", "/v1/rule-trees/import", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	cmd.Flags().StringVar(&workflowAlias, "workflow-alias", "", "stamp workflowAlias on every imported rule tree")
	return cmd
}

// ----- mapping-tables -----

func makeMtImportCmd() *cobra.Command {
	var bodyFlag string
	var workflowAlias string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Bulk-import mapping tables from a JSON bundle",
		Long: `POST /v1/mapping-tables/import.

Use --workflow-alias <alias> to stamp every imported mapping table with
the same workflowAlias. Without it the imports will not appear in the
workflow's mapping-table picker.`,
		Example: `  altscore mapping-tables import --body @mapping-tables.json --workflow-alias underwriting-v1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			body, err = stampWorkflowAliasOnArray(body, workflowAlias, "mapping-tables", cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", "borrower_central", "/v1/mapping-tables/import", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	cmd.Flags().StringVar(&workflowAlias, "workflow-alias", "", "stamp workflowAlias on every imported mapping table")
	return cmd
}

// ----- scorecards -----

func makeScImportCmd() *cobra.Command {
	var bodyFlag string
	var workflowAlias string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Bulk-import scorecards from a JSON bundle",
		Long: `POST /v1/scorecards/import.

Use --workflow-alias <alias> to stamp every imported scorecard with the
same workflowAlias. Without it the imports will not appear in the workflow's
scorecard picker.`,
		Example: `  altscore scorecards import --body @scorecards.json --workflow-alias underwriting-v1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			body, err = stampWorkflowAliasOnArray(body, workflowAlias, "scorecards", cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", "borrower_central", "/v1/scorecards/import", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	cmd.Flags().StringVar(&workflowAlias, "workflow-alias", "", "stamp workflowAlias on every imported scorecard")
	return cmd
}

func makeScUsageCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "usage <id>",
		Short:   "Show how many workflows use this scorecard",
		Long:    `GET /v1/scorecards/{id}/usage. Useful before deleting or modifying.`,
		Example: `  altscore scorecards usage <id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v1/scorecards/%s/usage", args[0])
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
