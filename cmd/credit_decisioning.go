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

// normalizeImportBody prepares a bulk-import body for POST /v1/<resource>/import
// and stamps workflowAlias on every record.
//
// All four credit-decisioning import endpoints take an OBJECT wrapper, not a bare
// array -- BulkImportMappingTables{mappingTables}, BulkImportScorecards{scorecards},
// BulkImportEvaluationRules{rules}, BulkImportRuleTrees{ruleTrees}, each plus an
// optional skipExisting. A bare array is rejected with a 400 "value is not a valid
// dict". wrapperKey is the resource's array field name.
//
// Both shapes are accepted here and normalised to the wrapper the endpoint requires:
// a bare array is a natural thing to hand a command called "import", and rejecting it
// only to have the server reject it more cryptically helps nobody.
//
// When alias is empty AND records don't already carry a workflowAlias, prints a
// warning to stderr (mirrors the --workflow-alias missing warning in
// resource.go::makeCreateCmd). entityKind is the resource name in plural form.
func normalizeImportBody(body json.RawMessage, wrapperKey, alias, entityKind string, warnWriter io.Writer) (json.RawMessage, error) {
	var items []map[string]json.RawMessage
	wrapper := map[string]json.RawMessage{}

	if err := json.Unmarshal(body, &items); err != nil {
		// Not a bare array -- expect the object wrapper.
		if err := json.Unmarshal(body, &wrapper); err != nil {
			return nil, fmt.Errorf("import body must be a JSON array of %s or an object with a %q array: %w",
				entityKind, wrapperKey, err)
		}
		raw, ok := wrapper[wrapperKey]
		if !ok {
			return nil, fmt.Errorf("import body object has no %q key; %s import expects {%q: [...]}",
				wrapperKey, entityKind, wrapperKey)
		}
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("%q must be an array of objects: %w", wrapperKey, err)
		}
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
	} else {
		encoded, err := json.Marshal(alias)
		if err != nil {
			return nil, err
		}
		for i := range items {
			items[i]["workflowAlias"] = encoded
		}
	}

	encodedItems, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	wrapper[wrapperKey] = encodedItems
	return json.Marshal(wrapper)
}

// ----- evaluation-rules -----

func makeErImportCmd() *cobra.Command {
	var bodyFlag string
	var workflowAlias string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Bulk-import evaluation rules from a JSON bundle",
		Long: `POST /v1/evaluation-rules/import. The endpoint takes an OBJECT
wrapper (BulkImportEvaluationRules):
  {"rules": [{"label": "Approve", "code": "approve", ...}, ...],
   "skipExisting": true}
A bare top-level array is rejected with 400 "value is not a valid dict".
A bare array is still accepted here and wrapped for you.

Use --workflow-alias <alias> to stamp every imported rule with the same
workflowAlias. Without it the imports will not appear in the workflow's
evaluate-rules picker.`,
		Example: `  altscore evaluation-rules import --body @rules.json --workflow-alias underwriting-v1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			body, err = normalizeImportBody(body, "rules", workflowAlias, "evaluation-rules", cmd.ErrOrStderr())
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

func makeErCopyCmd() *cobra.Command {
	var workflowAlias string
	cmd := &cobra.Command{
		Use:   "copy <id>",
		Short: "Duplicate an evaluation rule into another workflow",
		Long: `POST /v1/evaluation-rules/{id}/copy. Creates an INDEPENDENT copy of the
rule (new id, same code and conditions) scoped to --workflow-alias, leaving
the source rule untouched.

This is the correct way to reuse a rule in another flow. Do NOT use
'update --workflow-alias' for that: update RE-SCOPES (moves) the rule and
makes it disappear from its previous workflow's evaluate-rules picker.

Fails with 409 if a rule with the same code already exists in the target
workflow.`,
		Example: `  altscore evaluation-rules copy <id> --workflow-alias scoring-pn-nuevo-cliente`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]string{"workflowAlias": workflowAlias})
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/v1/evaluation-rules/%s/copy", args[0])
			data, _, err := c.Do("POST", "borrower_central", path, json.RawMessage(body))
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&workflowAlias, "workflow-alias", "", "target workflow to copy the rule into (required)")
	_ = cmd.MarkFlagRequired("workflow-alias")
	return cmd
}

// ----- rule-trees -----

func makeRtImportCmd() *cobra.Command {
	var bodyFlag string
	var workflowAlias string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Bulk-import rule trees from a JSON bundle",
		Long: `POST /v1/rule-trees/import. Takes an object wrapper
{"ruleTrees": [...], "skipExisting": true}; a bare array is wrapped for you.

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
			body, err = normalizeImportBody(body, "ruleTrees", workflowAlias, "rule-trees", cmd.ErrOrStderr())
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
		Long: `POST /v1/mapping-tables/import. Takes an object wrapper
{"mappingTables": [...], "skipExisting": true}; a bare array is wrapped for you.

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
			body, err = normalizeImportBody(body, "mappingTables", workflowAlias, "mapping-tables", cmd.ErrOrStderr())
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
		Long: `POST /v1/scorecards/import. Takes an object wrapper
{"scorecards": [...], "skipExisting": true}; a bare array is wrapped for you.

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
			body, err = normalizeImportBody(body, "scorecards", workflowAlias, "scorecards", cmd.ErrOrStderr())
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
