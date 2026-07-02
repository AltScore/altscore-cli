package cmd

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// The analytics command group lets agents manage a tenant's analytics
// configuration: metrics (ClickHouse SQL templates), widgets (visualizations
// bound to a metric), and dashboard layouts (grid arrangements of widgets).
//
// Config lives in MongoDB; the numbers come from ClickHouse. See
// borrower-central/docs/analytics-provisioning.md for the full model.
//
// Endpoints wrapped (all on the borrower_central module):
//
//	GET    /v1/analytics/widgets                       list widgets
//	POST   /v1/analytics/widget                        create widget
//	PUT    /v1/analytics/widget/{id}                   update widget (partial)
//	DELETE /v1/analytics/widget/{id}                   delete widget
//	GET    /v1/analytics/queries                       list metrics
//	POST   /v1/analytics/commands/new-metric           create/overwrite metric
//	POST   /v1/analytics/commands/get-metrics          run a metric
//	DELETE /v1/analytics/query/{key}                   delete metric
//	GET    /v1/analytics/query/unique-field-values     filter dropdown values
//	GET    /v1/analytics/query/get-terms               distinct durations/rates
//	GET    /v1/dashboard-layout[/{id}|/default]        read layouts
//	POST   /v1/dashboard-layout                        create layout
//	PUT    /v1/dashboard-layout/{id}                   update layout
//	DELETE /v1/dashboard-layout/{id}                   delete layout
//	PUT    /v1/dashboard-layout/{id}/set-default       mark default
//	DELETE /v1/dashboard-layout/{id}/unset-default     clear default

const analyticsModule = "borrower_central"

func makeAnalyticsGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "analytics",
		Short: "Manage analytics metrics, widgets, and dashboards",
		Long: `Manage a tenant's analytics: metrics, widgets, and dashboard layouts.

- metrics    ClickHouse SQL templates (the numbers a chart queries)
- widgets    visualizations bound to a metric by queryId
- dashboards grid layouts arranging widgets

Typical provisioning order: create a metric, create a widget whose queryId is
the metric key, then create a dashboard layout that references the widget id
and set it as default. Verify a metric by running it ('metrics run').

See borrower-central/docs/analytics-provisioning.md for the data model,
placeholders, and gotchas.`,
	}

	group.AddCommand(makeAnWidgetsGroupCmd())
	group.AddCommand(makeAnMetricsGroupCmd())
	group.AddCommand(makeAnDashboardsGroupCmd())
	group.AddCommand(makeAnFieldValuesCmd())
	group.AddCommand(makeAnTermsCmd())
	return group
}

// ---------------------------------------------------------------- widgets

func makeAnWidgetsGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "widgets",
		Short: "Manage analytics widgets",
		Long: `Manage analytics widgets (visualizations bound to a metric).

A widget references a metric via queryId (== the metric's metricKey) and picks
a widgetType. Widgets are per-tenant; a widget shared under the "system" tenant
is also visible to every tenant but cannot be edited or deleted per-tenant.`,
	}
	group.AddCommand(makeAnWidgetsListCmd())
	group.AddCommand(makeAnWidgetCreateCmd())
	group.AddCommand(makeAnWidgetUpdateCmd())
	group.AddCommand(makeAnWidgetDeleteCmd())
	return group
}

func makeAnWidgetsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List widgets for the tenant",
		Long: `List the tenant's widgets (includes any shared "system" widgets).

Wraps GET /v1/analytics/widgets. Returns a JSON array.

Response fields: id, queryId, widgetType, displayName, labelAxis, dataAxis,
variants, createdAt, updatedAt.`,
		Example: `  altscore analytics widgets list
  altscore analytics widgets list | jq '.[].id'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", analyticsModule, "/v1/analytics/widgets", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeAnWidgetCreateCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a widget",
		Long: `Create a widget. Pass the JSON body via --body or stdin.

Requires bc.private.write permission. Wraps POST /v1/analytics/widget.
Re-posting with an existing widgetId overwrites it (full replace).

Request body fields:
  widgetId:    string   [required] client-chosen id (unique per tenant)
  queryId:     string   [required] a metric's metricKey
  displayName: string   [required]
  widgetType:  string   [required] pie | kpi | percentage | money | bar-chart | table | waterfall
  labelAxis:   string   [required except widgetType=waterfall]
  dataAxis:    [string] [required except widgetType=waterfall]
  variants:    [ { displayName, querySuffix } ]  optional`,
		Example: `  altscore analytics widgets create --body '{"widgetId":"w_active","queryId":"active_borrowers","displayName":"Active","widgetType":"bar-chart","labelAxis":"status","dataAxis":["count"]}'
  echo '{...}' | altscore analytics widgets create`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", analyticsModule, "/v1/analytics/widget", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin; supports @file)")
	return cmd
}

func makeAnWidgetUpdateCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "update <widget-id>",
		Short: "Update a widget (partial)",
		Long: `Partial-update a widget by id. Only the fields you send are changed.

Requires bc.private.write permission. Wraps PUT /v1/analytics/widget/{id} and
returns the updated widget DTO.

Request body fields (all optional): queryId, displayName, widgetType,
labelAxis, dataAxis, variants.`,
		Example: `  altscore analytics widgets update w_active --body '{"displayName":"Active borrowers"}'`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			data, _, err := c.Do("PUT", analyticsModule, "/v1/analytics/widget/"+url.PathEscape(args[0]), body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin; supports @file)")
	return cmd
}

func makeAnWidgetDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <widget-id>",
		Short:   "Delete a widget",
		Long:    "Delete a widget by id. Requires bc.private.write. Returns empty on success.",
		Example: `  altscore analytics widgets delete w_active`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			_, status, err := c.Do("DELETE", analyticsModule, "/v1/analytics/widget/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Deleted widget (HTTP %d).\n", status)
			return nil
		},
	}
}

// ---------------------------------------------------------------- metrics

func makeAnMetricsGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "metrics",
		Short: "Manage analytics metrics (ClickHouse SQL templates)",
		Long: `Manage analytics metrics: named ClickHouse SQL templates.

The queryTemplate is rendered with str.format placeholders: {tenant},
{BORROWER_WHERE_CLAUSE}, {BORROWER_FIELDS_WHERE_CLAUSE}, {COHORT_DATE_CLAUSE},
{PAYMENT_COHORT_DATE_CLAUSE}, {ANALYSIS_DATE_CLAUSE}, {FILTER_QNT}. Escape any
literal brace as {{ or }}. A malformed template is rejected with 400
INVALID_METRIC_TEMPLATE at create and run time.`,
	}
	group.AddCommand(makeAnMetricsListCmd())
	group.AddCommand(makeAnMetricCreateCmd())
	group.AddCommand(makeAnMetricRunCmd())
	group.AddCommand(makeAnMetricDeleteCmd())
	return group
}

func makeAnMetricsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the tenant's metrics",
		Long: `List the tenant's metric definitions. Requires bc.private.read.

Wraps GET /v1/analytics/queries. Note: metrics shared under the "system"
tenant still run (via get-metrics) but are NOT included in this list.

Response fields: metricKey, queryTemplate, queryMetadata, columns, metadataCols.`,
		Example: `  altscore analytics metrics list | jq '.[].metricKey'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", analyticsModule, "/v1/analytics/queries", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeAnMetricCreateCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create or overwrite a metric",
		Long: `Create (or overwrite) a metric. Pass the JSON body via --body or stdin.

Requires bc.private.write. Wraps POST /v1/analytics/commands/new-metric. The
template is validated on save and the saved metric DTO is returned.

Upsert is keyed on metricKey ONLY (not tenant): re-sending an existing key
overwrites it, even across tenants. Use tenant-unique keys, or share
deliberately under "system".

Request body fields:
  metricKey:     string   [required] unique key
  queryTemplate: string   [required] ClickHouse SQL with the supported placeholders
  columns:       [string] [required] result columns, in SELECT order
  metadataCols:  [string] [required] subset of columns routed into row.metadata
  queryMetadata: object   { dataType, intl: { es: {label,description}, en: {...} } }
  tenant:        string   superuser only; targets another tenant (defaults to yours)`,
		Example: `  altscore analytics metrics create --body @metric.json
  altscore analytics metrics create --body '{"metricKey":"active_borrowers","queryTemplate":"SELECT status, count() FROM borrower_snapshot WHERE tenant=''{tenant}'' AND {BORROWER_WHERE_CLAUSE} GROUP BY status","columns":["status","count"],"metadataCols":["status"]}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", analyticsModule, "/v1/analytics/commands/new-metric", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin; supports @file)")
	return cmd
}

func makeAnMetricRunCmd() *cobra.Command {
	var bodyFlag string
	var metricKey string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a metric and return its result",
		Long: `Execute a metric against ClickHouse and return { result, metadata }.

Requires borrowers.read. Wraps POST /v1/analytics/commands/get-metrics. This
is the real verification that a metric's template is valid and returns rows.

Pass the full command body via --body/stdin, or use --metric-key for a bare
run with no filters. Request body fields:
  metricKey:    string   [required]
  filters:      [ { key, entityType, operator (in|nin|na), value } ]  optional
  cohortDate:   { from, to }   optional ISO dates
  analysisDate: string         optional ISO date`,
		Example: `  altscore analytics metrics run --metric-key active_borrowers
  altscore analytics metrics run --body '{"metricKey":"active_borrowers","cohortDate":{"from":"2025-01-01","to":"2025-06-30"}}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			var body any
			if metricKey != "" {
				if bodyFlag != "" {
					return fmt.Errorf("use either --metric-key or --body, not both")
				}
				body = map[string]string{"metricKey": metricKey}
			} else {
				b, err := readBody(bodyFlag)
				if err != nil {
					return err
				}
				body = b
			}
			data, _, err := c.Do("POST", analyticsModule, "/v1/analytics/commands/get-metrics", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "full get-metrics JSON body (or pipe via stdin; supports @file)")
	cmd.Flags().StringVar(&metricKey, "metric-key", "", "run this metric key with no filters (shorthand for --body)")
	return cmd
}

func makeAnMetricDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <metric-key>",
		Short:   "Delete a metric by key",
		Long:    "Delete a metric definition by its metricKey. Requires bc.private.write.",
		Example: `  altscore analytics metrics delete active_borrowers`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			_, status, err := c.Do("DELETE", analyticsModule, "/v1/analytics/query/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Deleted metric (HTTP %d).\n", status)
			return nil
		},
	}
}

// ------------------------------------------------------------- dashboards

func makeAnDashboardsGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "dashboards",
		Short: "Manage dashboard layouts",
		Long: `Manage dashboard layouts (grid arrangements of widgets).

A layout's layoutSetup has responsive breakpoints (lg/md/sm), each an array of
{ i, x, y, w, h } where i is a widgetId. Layouts are strictly per-tenant.`,
	}
	group.AddCommand(makeAnDashboardsListCmd())
	group.AddCommand(makeAnDashboardGetCmd())
	group.AddCommand(makeAnDashboardDefaultCmd())
	group.AddCommand(makeAnDashboardCreateCmd())
	group.AddCommand(makeAnDashboardUpdateCmd())
	group.AddCommand(makeAnDashboardDeleteCmd())
	group.AddCommand(makeAnDashboardSetDefaultCmd())
	group.AddCommand(makeAnDashboardUnsetDefaultCmd())
	return group
}

func makeAnDashboardsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List dashboard layouts",
		Long:    "List the tenant's dashboard layouts. Wraps GET /v1/dashboard-layout.",
		Example: `  altscore analytics dashboards list`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", analyticsModule, "/v1/dashboard-layout", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeAnDashboardGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "get <layout-id>",
		Short:   "Get a dashboard layout by id",
		Long:    "Retrieve a single dashboard layout. Wraps GET /v1/dashboard-layout/{id}.",
		Example: `  altscore analytics dashboards get <layout-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", analyticsModule, "/v1/dashboard-layout/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeAnDashboardDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "default",
		Short:   "Get the tenant's default dashboard layout",
		Long:    "Fetch the default layout. Wraps GET /v1/dashboard-layout/default (404 if none set).",
		Example: `  altscore analytics dashboards default`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", analyticsModule, "/v1/dashboard-layout/default", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeAnDashboardCreateCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a dashboard layout",
		Long: `Create a dashboard layout. Pass the JSON body via --body or stdin.

Requires borrowers.write. Wraps POST /v1/dashboard-layout; returns { id }.

Request body fields:
  displayName: string  [required]
  layoutSetup: { lg: [ {i,x,y,w,h,minW?,maxW?,minH?,maxH?} ], md: [...], sm: [...] }
               where each i is a widgetId.`,
		Example: `  altscore analytics dashboards create --body @layout.json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			data, _, err := c.Do("POST", analyticsModule, "/v1/dashboard-layout", body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin; supports @file)")
	return cmd
}

func makeAnDashboardUpdateCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "update <layout-id>",
		Short: "Update a dashboard layout",
		Long: `Update a dashboard layout by id. Pass a JSON body via --body or stdin.

Requires borrowers.write. Wraps PUT /v1/dashboard-layout/{id}; returns the DTO.
Mutable fields: displayName, layoutSetup (isPrivate). The default flag is
managed separately via set-default / unset-default.`,
		Example: `  altscore analytics dashboards update <layout-id> --body '{"displayName":"Overview"}'`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			body, err := readBody(bodyFlag)
			if err != nil {
				return err
			}
			data, _, err := c.Do("PUT", analyticsModule, "/v1/dashboard-layout/"+url.PathEscape(args[0]), body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or pipe via stdin; supports @file)")
	return cmd
}

func makeAnDashboardDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <layout-id>",
		Short:   "Delete a dashboard layout",
		Long:    "Delete a dashboard layout by id. Requires borrowers.write.",
		Example: `  altscore analytics dashboards delete <layout-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			_, status, err := c.Do("DELETE", analyticsModule, "/v1/dashboard-layout/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Deleted layout (HTTP %d).\n", status)
			return nil
		},
	}
}

func makeAnDashboardSetDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "set-default <layout-id>",
		Short:   "Make a layout the tenant default",
		Long:    "Mark a layout as the tenant default (unsets any other default). Wraps PUT /v1/dashboard-layout/{id}/set-default.",
		Example: `  altscore analytics dashboards set-default <layout-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			_, status, err := c.Do("PUT", analyticsModule, "/v1/dashboard-layout/"+url.PathEscape(args[0])+"/set-default", nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Set as default (HTTP %d).\n", status)
			return nil
		},
	}
}

func makeAnDashboardUnsetDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "unset-default <layout-id>",
		Short:   "Clear a layout's default flag",
		Long:    "Clear the default flag on a layout. Wraps DELETE /v1/dashboard-layout/{id}/unset-default.",
		Example: `  altscore analytics dashboards unset-default <layout-id>`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			_, status, err := c.Do("DELETE", analyticsModule, "/v1/dashboard-layout/"+url.PathEscape(args[0])+"/unset-default", nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Cleared default flag (HTTP %d).\n", status)
			return nil
		},
	}
}

// ---------------------------------------------------------- filter helpers

func makeAnFieldValuesCmd() *cobra.Command {
	var key string
	cmd := &cobra.Command{
		Use:   "field-values",
		Short: "List distinct values of a borrower field (for filter dropdowns)",
		Long: `Return the distinct values of a borrower field key from ClickHouse.

Requires borrowers.read. Wraps GET /v1/analytics/query/unique-field-values?key=<key>.
Used to populate filter dropdowns for the get-metrics filters.`,
		Example: `  altscore analytics field-values --key riskRating`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("--key is required")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			path := "/v1/analytics/query/unique-field-values?key=" + url.QueryEscape(key)
			data, _, err := c.Do("GET", analyticsModule, path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "borrower field key [required]")
	return cmd
}

func makeAnTermsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "terms",
		Short: "List distinct debt durations and interest rates",
		Long: `Return the distinct debt durations and interest rates for the tenant.

Requires borrowers.read. Wraps GET /v1/analytics/query/get-terms. Used to
populate the duration/rate filter dropdowns.`,
		Example: `  altscore analytics terms`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", analyticsModule, "/v1/analytics/query/get-terms", nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
