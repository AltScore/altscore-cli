package cmd

import (
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func makeToolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "Report generation, email, and other platform tools",
	}

	cmd.AddCommand(makeToolsGenerateReportCmd())
	cmd.AddCommand(makeToolsReportComponentsCmd())
	return cmd
}

func makeToolsGenerateReportCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "generate-report",
		Short: "Generate a PDF report and return the signed URL",
		Long: `Generate a PDF report from a structured request.

The request body contains reportTitle, byLine, logoUrl, and a sections array.
Each section has title, subtitle, and a components array. Use
'altscore tools report-components' to discover available component types and
their schemas.

Returns a JSON object with a signed URL to the generated PDF.`,
		Example: `  # Generate a simple report
  altscore tools generate-report --body '{
    "reportTitle": "Credit Report",
    "byLine": "Generated 2026-03-07",
    "logoUrl": "",
    "sections": [{
      "title": "Summary",
      "subtitle": "",
      "components": [
        {"name": "keyValueTable", "title": "Results", "subtitle": "",
         "items": [{"label": "Score", "value": "750"}]}
      ]
    }]
  }'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}

			data, _, err := c.Do("POST", "borrower_central", "/v1/tools/generate-report", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin)")
	return cmd
}

func makeToolsReportComponentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "report-components [name]",
		Short: "List available report components or show schema for one",
		Long: `Query the report generator for available PDF report components.

Without arguments, lists all components grouped by type (report, source, entity).
With a component name, returns the full JSON Schema for that component's options.

Source components are auto-matched by AltData source slug (e.g. ECU-PUB-0002_v1).
They accept an altdataPackage dict as input.`,
		Example: `  # List all components
  altscore tools report-components

  # Show schema for a specific component
  altscore tools report-components subjectInfo
  altscore tools report-components evaluatorResult
  altscore tools report-components reportOptions`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := "/v1/meta/report-components"
			if len(args) > 0 {
				path = fmt.Sprintf("/v1/meta/report-components?component=%s", args[0])
			}

			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	return cmd
}
