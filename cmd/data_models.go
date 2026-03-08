package cmd

import (
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

func makeDmMakeSensitiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "make-sensitive <id>",
		Short: "Enable field-level encryption on a data-model",
		Long: `Mark an identity data-model as sensitive, enabling field-level encryption
for all identity values under this key.

This is a one-way operation: once a data-model is marked as sensitive, it cannot
be reverted. The data-model must have entityType=identity.

Calls PUT /v1/data-models/{id}/make-sensitive.`,
		Example: `  altscore data-models make-sensitive <data-model-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := fmt.Sprintf("/v1/data-models/%s/make-sensitive", args[0])
			data, _, err := c.Do("PUT", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeDmGuideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guide [entity-type]",
		Short: "Show the data-models best-practices guide",
		Long: `Query the data-models guide for entity type documentation, required fields,
validation rules, and create examples.

Without arguments, returns the full guide covering all 16 entity types,
common field definitions, and category groupings.

With an entity type argument, returns documentation for that specific type
plus the common fields reference.

Available entity types:
  identity, contact, document, borrower, borrower_field, point_of_contact,
  authorization, step, metric, deal_field, deal_step, deal_role, decision,
  accounting_document, asset_field, asset_group`,
		Example: `  # Full guide
  altscore data-models guide

  # Guide for a specific entity type
  altscore data-models guide identity
  altscore data-models guide step
  altscore data-models guide borrower_field`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}

			path := "/v1/meta/data-models"
			if len(args) > 0 {
				path = fmt.Sprintf("/v1/meta/data-models?entity-type=%s", args[0])
			}

			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
