package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// The credit-accounts command group surfaces the Borrower Central LMS
// (Loan Management System) read commands. The backend exposes three GET
// endpoints under /credit-accounts/commands/* (see
// borrower-central/app/api/credit_accounts/handler.py). All return JSON.
// These are read-only views layered on top of CMS client data; mutations
// happen via the underlying CMS / DPA services (see 'dpas' and
// 'payment-orders' groups).

func makeCreditAccountsGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "credit-accounts",
		Short: "Query credit-account limits and borrower lookups (LMS)",
		Long: `Query LMS (Loan Management System) views on credit accounts.

A credit account is the Borrower Central projection of a CMS client's credit
line: total limit, consumed amount, available balance, and history. Mutations
live in the CMS / DPA surface ('dpas' and 'payment-orders' groups); this group
is read-only.

Backed by GET /credit-accounts/commands/* on borrower_central. Requires the
profile to have bc.private.read.`,
	}
	group.AddCommand(makeCALimitsSummaryCmd())
	group.AddCommand(makeCASearchBorrowersCmd())
	group.AddCommand(makeCALimitsHistoryCmd())
	return group
}

func makeCALimitsSummaryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "limits-summary",
		Short: "Tenant-wide aggregate of credit-account limits",
		Long: `Return the tenant-wide aggregate of credit-account limits.

Wraps GET /credit-accounts/commands/limits-summary. Output is a JSON object
with the tenant's total limit, consumed amount, available amount, and any
aggregate breakdowns the backend reports.`,
		Example: `  altscore credit-accounts limits-summary
  altscore credit-accounts limits-summary | jq '.totalLimit'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", "borrower_central", "/v1/credit-accounts/commands/limits-summary", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeCASearchBorrowersCmd() *cobra.Command {
	var search string

	cmd := &cobra.Command{
		Use:   "search-borrowers",
		Short: "Find CMS client IDs by borrower search term",
		Long: `Resolve a free-text borrower search term to a list of CMS client IDs.

Wraps GET /credit-accounts/commands/search-borrowers?search=<term>. Used by
the Hub disbursements / DPA pages to translate a borrower name or tax-id into
the client-ids filter that STP and CMS endpoints accept.`,
		Example: `  altscore credit-accounts search-borrowers --search "Acme"
  altscore credit-accounts search-borrowers --search "$TAX_ID" | jq -r '.id'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(search) == "" {
				return fmt.Errorf("--search is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := "/v1/credit-accounts/commands/search-borrowers?search=" + url.QueryEscape(search)
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&search, "search", "", "free-text borrower search term [required]")
	return cmd
}

func makeCALimitsHistoryCmd() *cobra.Command {
	var page int
	var perPage int
	var sortBy string
	var sortDirection string
	var search string
	var dateFrom string
	var dateTo string
	var timezoneOffset int

	cmd := &cobra.Command{
		Use:   "limits-history",
		Short: "Paginated history of credit-limit changes",
		Long: `Return a paginated history of credit-limit changes across the tenant.

Wraps GET /credit-accounts/commands/limits-history. Supports pagination,
sort, date range, and a free-text search that auto-resolves to client-ids
under the hood. The backend aliases query params in camelCase (perPage,
sortBy, sortDirection, dateFrom, dateTo, timezoneOffset); this CLI uses
kebab-case flags and maps them.`,
		Example: `  altscore credit-accounts limits-history --per-page 50
  altscore credit-accounts limits-history --date-from 2025-01-01 --date-to 2025-01-31
  altscore credit-accounts limits-history --search "Acme" --sort-by createdAt --sort-direction desc`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			params := []string{}
			if page > 0 {
				params = append(params, fmt.Sprintf("page=%d", page))
			}
			if perPage > 0 {
				params = append(params, fmt.Sprintf("perPage=%d", perPage))
			} else if c.Config.Defaults.PerPage > 0 {
				params = append(params, fmt.Sprintf("perPage=%d", c.Config.Defaults.PerPage))
			}
			if sortBy != "" {
				params = append(params, "sortBy="+url.QueryEscape(sortBy))
			}
			if sortDirection != "" {
				params = append(params, "sortDirection="+url.QueryEscape(sortDirection))
			}
			if search != "" {
				params = append(params, "search="+url.QueryEscape(search))
			}
			if dateFrom != "" {
				params = append(params, "dateFrom="+url.QueryEscape(dateFrom))
			}
			if dateTo != "" {
				params = append(params, "dateTo="+url.QueryEscape(dateTo))
			}
			if timezoneOffset != 0 {
				params = append(params, fmt.Sprintf("timezoneOffset=%d", timezoneOffset))
			}
			path := "/v1/credit-accounts/commands/limits-history"
			if len(params) > 0 {
				path += "?" + strings.Join(params, "&")
			}
			data, _, err := c.Do("GET", "borrower_central", path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().IntVar(&page, "page", 0, "page number (default: 1)")
	cmd.Flags().IntVar(&perPage, "per-page", 0, "items per page (default: from config; max 100)")
	cmd.Flags().StringVar(&sortBy, "sort-by", "", "sort field (default: createdAt)")
	cmd.Flags().StringVar(&sortDirection, "sort-direction", "", "asc | desc (default: desc)")
	cmd.Flags().StringVar(&search, "search", "", "free-text borrower search (resolves to client-ids filter)")
	cmd.Flags().StringVar(&dateFrom, "date-from", "", "ISO date lower bound (inclusive)")
	cmd.Flags().StringVar(&dateTo, "date-to", "", "ISO date upper bound (inclusive)")
	cmd.Flags().IntVar(&timezoneOffset, "timezone-offset", 0, "timezone offset in minutes for date range interpretation")
	return cmd
}
