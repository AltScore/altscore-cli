package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// The dpas command group wraps the CMS Credits API DPA (Dynamic Payment
// Allowance) surface. CREDITS_API_BASE_URL in the Hub is the CMS host, and
// all DPA paths sit under /v2/clients/.../credit-accounts/dpa. We route
// through the cms module.
//
// Endpoints exposed (from altscore-ai-chat/app/(app)/clients/credit-accounts/dpa/route.ts
// and altscore-ai-chat/app/(app)/clients/[id]/segmentations/dpa/route.ts):
//
//   GET  /v2/clients/credit-accounts/dpa                      list all DPA credit accounts
//   GET  /v2/clients/credit-accounts/dpa?external-id=<uuid>   lookup by external id
//   GET  /v2/clients/{clientId}/credit-accounts/dpa           lookup by CMS client id
//   GET  /v2/clients/{clientId}/segmentations/dpa             DPA segmentations for a client

const dpaListBasePath = "/v2/clients/credit-accounts/dpa"

func makeDpasGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "dpas",
		Short: "Manage DPA (Dynamic Payment Allowance) credit accounts",
		Long: `Manage DPA (Dynamic Payment Allowance) credit accounts on the CMS Credits API.

A DPA credit account represents a client's revolving credit line: total
limit, consumed, available, and the segmentation rules that gate how the
limit can be spent. This group wraps the CMS endpoints under
/v2/clients/.../credit-accounts/dpa.

Use 'get <client-id>' to fetch a single client's DPA account, 'list' to
page through all tenant DPAs (with search), 'by-external-id' to look up by
external system id, and 'segmentations' to read the segmentation rules
that gate a client's DPA.`,
	}
	group.AddCommand(makeDpaListCmd())
	group.AddCommand(makeDpaGetCmd())
	group.AddCommand(makeDpaByExternalIDCmd())
	group.AddCommand(makeDpaSegmentationsCmd())
	return group
}

func makeDpaListCmd() *cobra.Command {
	var search string
	var perPage int
	var page int
	var updatedFrom string
	var updatedTo string
	var status string
	var sortBy string
	var sortDir string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List DPA credit accounts (with search and pagination)",
		Long: `List DPA credit accounts across the tenant.

Wraps GET /v2/clients/credit-accounts/dpa. Supports free-text search (which
the Hub layer resolves to client-ids via borrower lookup), pagination,
status filter, date range on updatedAt, and sort.`,
		Example: `  altscore dpas list --per-page 25
  altscore dpas list --search "Acme" --sort-by "percentages.consumed" --sort-direction desc
  altscore dpas list --updated-date-from 2025-01-01 --updated-date-to 2025-01-31`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			params := []string{}
			if search != "" {
				params = append(params, "search="+url.QueryEscape(search))
			}
			if perPage > 0 {
				params = append(params, fmt.Sprintf("per-page=%d", perPage))
			} else if c.Config.Defaults.PerPage > 0 {
				params = append(params, fmt.Sprintf("per-page=%d", c.Config.Defaults.PerPage))
			}
			if page > 0 {
				params = append(params, fmt.Sprintf("page=%d", page))
			}
			if updatedFrom != "" {
				params = append(params, "updated-date-from="+url.QueryEscape(updatedFrom))
			}
			if updatedTo != "" {
				params = append(params, "updated-date-to="+url.QueryEscape(updatedTo))
			}
			if status != "" {
				params = append(params, "status="+url.QueryEscape(status))
			}
			if sortBy != "" {
				params = append(params, "sort-by="+url.QueryEscape(sortBy))
			}
			if sortDir != "" {
				params = append(params, "sort-direction="+url.QueryEscape(sortDir))
			}
			path := dpaListBasePath
			if len(params) > 0 {
				path += "?" + strings.Join(params, "&")
			}
			data, _, err := c.Do("GET", "cms", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&search, "search", "", "free-text search (resolved to client-ids by the backend)")
	cmd.Flags().IntVar(&perPage, "per-page", 0, "items per page (default: from config, or 10)")
	cmd.Flags().IntVar(&page, "page", 0, "page number (default: 1)")
	cmd.Flags().StringVar(&updatedFrom, "updated-date-from", "", "ISO date lower bound on updatedAt")
	cmd.Flags().StringVar(&updatedTo, "updated-date-to", "", "ISO date upper bound on updatedAt")
	cmd.Flags().StringVar(&status, "status", "", "filter by DPA status")
	cmd.Flags().StringVar(&sortBy, "sort-by", "", "sort field (e.g. percentages.consumed)")
	cmd.Flags().StringVar(&sortDir, "sort-direction", "", "asc | desc")
	return cmd
}

func makeDpaGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <client-id>",
		Short: "Get the DPA credit account for a CMS client by ID",
		Long: `Retrieve the DPA credit account attached to a single CMS client.

Wraps GET /v2/clients/{clientId}/credit-accounts/dpa. The argument is the
CMS clientId (a UUID), not the Borrower Central borrower id. Use
'altscore borrowers get' to find a borrower's cmsClientIds first.`,
		Example: `  altscore dpas get <cms-client-id>
  altscore dpas get <cms-client-id> | jq '.totalLimit'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := "/v2/clients/" + url.PathEscape(args[0]) + "/credit-accounts/dpa"
			data, _, err := c.Do("GET", "cms", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeDpaByExternalIDCmd() *cobra.Command {
	var externalID string

	cmd := &cobra.Command{
		Use:   "by-external-id",
		Short: "Look up a DPA credit account by external ID",
		Long: `Look up a DPA credit account by its external-id query parameter.

Wraps GET /v2/clients/credit-accounts/dpa?external-id=<id>. Useful when the
caller only knows the partner's external identifier and not the CMS
clientId.`,
		Example: `  altscore dpas by-external-id --external-id <id>`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(externalID) == "" {
				return fmt.Errorf("--external-id is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := dpaListBasePath + "?external-id=" + url.QueryEscape(externalID)
			data, _, err := c.Do("GET", "cms", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&externalID, "external-id", "", "external system identifier [required]")
	return cmd
}

func makeDpaSegmentationsCmd() *cobra.Command {
	var disbursementDate string
	var partnerID string

	cmd := &cobra.Command{
		Use:   "segmentations <client-id>",
		Short: "Read DPA segmentation rules for a client",
		Long: `Read the DPA segmentation rules attached to a CMS client.

Wraps GET /v2/clients/{clientId}/segmentations/dpa. Segmentations are the
rules that gate how a DPA's total limit can be split across debt purposes,
products, or counterparties on a given disbursement date.

--disbursement-date is forwarded as a query param. --partner-id is sent as
the X-Partner-Id header (the Hub does the same).`,
		Example: `  altscore dpas segmentations <cms-client-id>
  altscore dpas segmentations <cms-client-id> --disbursement-date 2025-02-15
  altscore dpas segmentations <cms-client-id> --partner-id <partner>`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := "/v2/clients/" + url.PathEscape(args[0]) + "/segmentations/dpa"
			if disbursementDate != "" {
				path += "?disbursement-date=" + url.QueryEscape(disbursementDate)
			}
			headers := map[string]string{}
			if partnerID != "" {
				headers["X-Partner-Id"] = partnerID
			}
			data, _, err := c.DoWithHeaders("GET", "cms", path, nil, headers)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&disbursementDate, "disbursement-date", "", "ISO date used to pick the active segmentation")
	cmd.Flags().StringVar(&partnerID, "partner-id", "", "X-Partner-Id header value (when segmentations differ per partner)")
	return cmd
}
