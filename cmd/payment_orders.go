package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// The payment-orders command group wraps the STP (disbursements) service.
// STP_URL in the Hub is "https://api.altscore.ai/v1", which is the CMS host
// plus the /v1 prefix, so we route through the cms module and prefix paths
// with /v1/payment-orders. See altscore-ai-chat/app/(app)/disbursements/api/
// for the canonical surface.
//
// Endpoints exposed:
//   GET    /v1/payment-orders             list with filters
//   GET    /v1/payment-orders/{id}        single payment order
//   PUT    /v1/payment-orders/{id}/reconcile   mark as reconciled
//   PUT    /v1/payment-orders/{id}/success     mark as successful
//
// 'disbursements' is registered as an alias for the same command group.

const paymentOrdersBasePath = "/v1/payment-orders"

func makePaymentOrdersGroupCmd(use string) *cobra.Command {
	group := &cobra.Command{
		Use:   use,
		Short: "Manage STP payment orders (disbursements)",
		Long: `Manage payment orders served by STP (the disbursements service).

A payment order represents an outbound funds transfer initiated against a
debt / credit account. The Hub disbursements page is the canonical reference
for the filter and search shape this group exposes (sort, status, date range,
borrower / client / debt / reference search).

Mutation endpoints ('reconcile', 'success') flip terminal status on a payment
order. 'disbursements' is an alias for this group; both names target the same
backend.`,
	}
	group.AddCommand(makePOListCmd())
	group.AddCommand(makePOGetCmd())
	group.AddCommand(makePOReconcileCmd())
	group.AddCommand(makePOSuccessCmd())
	return group
}

func makePOListCmd() *cobra.Command {
	var sortBy string
	var sortDir string
	var offset int
	var limit int
	var status string
	var fromDate string
	var toDate string
	var clientIDs string
	var debtID string
	var externalIDs string
	var referenceNum string
	var trackingKey string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List payment orders with filters",
		Long: `List payment orders. Returns a JSON object with "data" (array of orders)
and "count" (x-total-count header).

Filters map to STP query params (kebab-case). Pagination uses offset/limit.
Free-text borrower lookups should be done first via
'credit-accounts search-borrowers' to resolve to --client-ids.`,
		Example: `  # First 50 in-progress
  altscore payment-orders list --status IN_PROGRESS --limit 50

  # Date window
  altscore payment-orders list --from-date 2025-01-01 --to-date 2025-01-31

  # By reference number
  altscore payment-orders list --reference-num ABC-123

  # By client IDs (resolve first via credit-accounts search-borrowers)
  altscore payment-orders list --client-ids "id1,id2"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			params := []string{}
			if sortBy != "" {
				params = append(params, "sort-by="+url.QueryEscape(sortBy))
			}
			if sortDir != "" {
				params = append(params, "sort-dir="+url.QueryEscape(sortDir))
			}
			if offset > 0 {
				params = append(params, fmt.Sprintf("offset=%d", offset))
			}
			if limit > 0 {
				params = append(params, fmt.Sprintf("limit=%d", limit))
			}
			if status != "" {
				params = append(params, "status="+url.QueryEscape(status))
			}
			if fromDate != "" {
				params = append(params, "from-date="+url.QueryEscape(fromDate))
			}
			if toDate != "" {
				params = append(params, "to-date="+url.QueryEscape(toDate))
			}
			if clientIDs != "" {
				params = append(params, "client-ids="+url.QueryEscape(clientIDs))
			}
			if debtID != "" {
				params = append(params, "debt-id="+url.QueryEscape(debtID))
			}
			if externalIDs != "" {
				params = append(params, "external-ids="+url.QueryEscape(externalIDs))
			}
			if referenceNum != "" {
				params = append(params, "reference-num="+url.QueryEscape(referenceNum))
			}
			if trackingKey != "" {
				params = append(params, "tracking-key="+url.QueryEscape(trackingKey))
			}
			path := paymentOrdersBasePath
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
	cmd.Flags().StringVar(&sortBy, "sort-by", "", "sort field")
	cmd.Flags().StringVar(&sortDir, "sort-dir", "", "asc | desc")
	cmd.Flags().IntVar(&offset, "offset", 0, "items to skip (pagination start)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max items to return")
	cmd.Flags().StringVar(&status, "status", "", "filter by payment-order status")
	cmd.Flags().StringVar(&fromDate, "from-date", "", "ISO date lower bound")
	cmd.Flags().StringVar(&toDate, "to-date", "", "ISO date upper bound")
	cmd.Flags().StringVar(&clientIDs, "client-ids", "", "comma-separated CMS client IDs")
	cmd.Flags().StringVar(&debtID, "debt-id", "", "filter by debt ID (UUID)")
	cmd.Flags().StringVar(&externalIDs, "external-ids", "", "comma-separated external IDs")
	cmd.Flags().StringVar(&referenceNum, "reference-num", "", "filter by reference number")
	cmd.Flags().StringVar(&trackingKey, "tracking-key", "", "filter by tracking key")
	return cmd
}

func makePOGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <pay-order-id>",
		Short: "Get a payment order by ID",
		Long: `Retrieve a single payment order by its payOrderId.

Wraps GET /v1/payment-orders/{id}. Returns the payment-order document plus
beneficiary/payer accounts when STP embeds them.`,
		Example: `  altscore payment-orders get <pay-order-id>
  altscore disbursements get <pay-order-id> | jq '.status'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", "cms", paymentOrdersBasePath+"/"+args[0], nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makePOReconcileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile <pay-order-id>",
		Short: "Mark a payment order as reconciled",
		Long: `Mark a payment order as reconciled.

Wraps PUT /v1/payment-orders/{id}/reconcile. Used when an external
settlement file confirms a payment that was previously in flight. No body
required.`,
		Example: `  altscore payment-orders reconcile <pay-order-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			_, status, err := c.Do("PUT", "cms", paymentOrdersBasePath+"/"+args[0]+"/reconcile", nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Reconciled (HTTP %d).\n", status)
			return nil
		},
	}
}

func makePOSuccessCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "success <pay-order-id>",
		Short: "Mark a payment order as successful",
		Long: `Mark a payment order as successful (terminal state).

Wraps PUT /v1/payment-orders/{id}/success. Used for manual confirmation
when an external counterparty has paid but the automated settlement loop
hasn't fired. No body required.`,
		Example: `  altscore payment-orders success <pay-order-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			_, status, err := c.Do("PUT", "cms", paymentOrdersBasePath+"/"+args[0]+"/success", nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Marked success (HTTP %d).\n", status)
			return nil
		},
	}
}
