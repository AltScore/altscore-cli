package cmd

import (
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// Custom subcommands for the credit-decisioning resources (mapping-tables,
// scorecards, evaluation-rules, rule-trees) that go beyond standard CRUD.
// Mirrors the factory pattern in cmd/workflow_tasks.go: each makeXxxCmd
// returns a *cobra.Command and is wired in cmd/root.go via .AddCommand on
// the resource group.

// ----- evaluation-rules -----

func makeErImportCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Bulk-import evaluation rules from a JSON bundle",
		Long: `POST /v1/evaluation-rules/import. Body should be the array of rule
definitions as produced by an export.`,
		Example: `  altscore evaluation-rules import --body @rules-bundle.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
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
	cmd := &cobra.Command{
		Use:     "import",
		Short:   "Bulk-import rule trees from a JSON bundle",
		Example: `  altscore rule-trees import --body @rule-trees.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
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
	return cmd
}

// ----- mapping-tables -----

func makeMtImportCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:     "import",
		Short:   "Bulk-import mapping tables from a JSON bundle",
		Example: `  altscore mapping-tables import --body @mapping-tables.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
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
	return cmd
}

// ----- scorecards -----

func makeScImportCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:     "import",
		Short:   "Bulk-import scorecards from a JSON bundle",
		Example: `  altscore scorecards import --body @scorecards.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
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
