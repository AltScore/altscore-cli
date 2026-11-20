package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// Batch post-processing: a workflow's batchPolicy.postProcessing names another PUBLISHED
// flow of the tenant, which Borrower Central starts with one input (`batch`) once a
// batch has finished. Everything here is a thin wrapper: the rules live in BC.

// The policy routes are id-keyed and per version. An alias resolves the way apply's
// target does: the ACTIVE version, else the latest DRAFT.
func resolveWorkflowIDForBatchPolicy(c *client.Client, aliasOrID string, errOut io.Writer) (string, error) {
	if uuidPattern.MatchString(aliasOrID) {
		return aliasOrID, nil
	}
	wf, status, err := findWorkflowByAlias(c, aliasOrID)
	if err != nil {
		return "", fmt.Errorf("could not resolve workflow alias %q: %w", aliasOrID, err)
	}
	if wf == nil {
		return "", fmt.Errorf("no ACTIVE or DRAFT workflow has alias %q (pass the workflow id to target another version)", aliasOrID)
	}
	id, _ := wf["id"].(string)
	if id == "" {
		id, _ = wf["workflowId"].(string)
	}
	if id == "" {
		return "", fmt.Errorf("workflow %q has no id field", aliasOrID)
	}
	fmt.Fprintf(errOut, "# %s -> %s version %v (%s)\n", aliasOrID, status, wf["version"], id)
	return id, nil
}

// A backend that predates the feature answers the new routes with a bare 404/405 (or a
// 4xx from an older two-segment route that happens to match). Name that possibility
// instead of leaving a "not found" that reads like a wrong id.
func postProcessingRouteError(err error, status int, route string) error {
	if err == nil {
		return nil
	}
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		if !strings.Contains(err.Error(), "errorSubCode=") {
			return fmt.Errorf("%w -- check the id; if it is right, this Borrower Central does not serve %s yet "+
				"(batch post-processing is not deployed there)", err, route)
		}
	}
	return err
}

func batchPolicySection(policy map[string]any, camel string) map[string]any {
	v, _ := sbValue(policy, camel)
	m, _ := v.(map[string]any)
	return m
}

// The two rules that need no flow lookup, checked before the request so the message can
// say what to change. BC enforces both again, plus the flow's compatibility.
func preflightBatchPolicyBody(body json.RawMessage) (map[string]any, error) {
	var policy map[string]any
	if err := json.Unmarshal(body, &policy); err != nil || policy == nil {
		return nil, fmt.Errorf("batch policy body must be a JSON object {autoRetry?, delivery?, postProcessing?} ('{}' clears the policy)")
	}
	post := batchPolicySection(policy, "postProcessing")
	if enabled, _ := post["enabled"].(bool); !enabled {
		return policy, nil
	}
	if alias := sbString(post, "workflowAlias"); strings.TrimSpace(alias) == "" {
		return nil, fmt.Errorf("postProcessing.enabled is true but postProcessing.workflowAlias is empty: name the PUBLISHED flow to run " +
			"(the backend refuses this with BATCH_POST_PROCESSING_WORKFLOW_REQUIRED). " +
			"'altscore workflows-v2 post-processing candidates <workflow>' lists the flows that qualify")
	}
	if enabled, _ := batchPolicySection(policy, "delivery")["enabled"].(bool); enabled {
		return nil, fmt.Errorf("delivery.enabled and postProcessing.enabled are mutually exclusive: with a post-processing flow in charge " +
			"of the results the fixed results email is off, and the flow sends what it decides to " +
			"(the backend refuses this with BATCH_POST_PROCESSING_EXCLUDES_DELIVERY). Set delivery.enabled=false or drop postProcessing")
	}
	return policy, nil
}

func makeWfv2BatchPolicyGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "batch-policy",
		Short: "Read or replace what happens when a batch of a v2 workflow finishes",
		Long: `A workflow's batch policy is operational config kept off the graph: 'apply' never
sends it and a revert keeps it. It belongs to ONE workflow version.

  autoRetry       {enabled, trigger, delayMinutes, maxRounds, sourceIds?}
                  re-runs the matching rows after the batch, in place
  delivery        {enabled, to, cc?, bcc?, subject?, body?, reportFilename?, ...}
                  one results email with the default results file
  postProcessing  {enabled, workflowAlias}
                  run another PUBLISHED flow of the tenant once the batch (rows and
                  automatic retries) is done; it receives ONE input, 'batch'

delivery and postProcessing cannot both be enabled. Field-by-field reference:
'altscore workflows-v2 schema-guide batchPolicy'.`,
	}
	group.AddCommand(makeWfv2BatchPolicyGetCmd())
	group.AddCommand(makeWfv2BatchPolicySetCmd())
	return group
}

func makeWfv2BatchPolicyGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <alias-or-id>",
		Short: "Show a v2 workflow's batch policy",
		Long: `Prints {workflowId, batchPolicy}, the shape 'batch-policy set' answers with.
batchPolicy is null when the workflow has none. There is no dedicated read route: the
policy is read off the workflow document (GET /v2/workflows/{id}).

An alias resolves to the ACTIVE version, else the latest DRAFT; pass an id to read
another version.`,
		Example: `  altscore workflows-v2 batch-policy get my-wf
  altscore workflows-v2 batch-policy get my-wf | jq .batchPolicy > policy.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			out, err := getWorkflowBatchPolicy(c, args[0], cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return output.JSON(out)
		},
	}
}

func getWorkflowBatchPolicy(c *client.Client, aliasOrID string, errOut io.Writer) (map[string]any, error) {
	id, err := resolveWorkflowIDForBatchPolicy(c, aliasOrID, errOut)
	if err != nil {
		return nil, err
	}
	data, _, err := c.Do("GET", "borrower_central", "/v2/workflows/"+id, nil)
	if err != nil {
		return nil, err
	}
	var wf map[string]any
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("could not parse workflow %s: %w", id, err)
	}
	return map[string]any{"workflowId": id, "batchPolicy": wf["batchPolicy"]}, nil
}

func makeWfv2BatchPolicySetCmd() *cobra.Command {
	var bodyFlag string
	cmd := &cobra.Command{
		Use:   "set <alias-or-id>",
		Short: "Replace a v2 workflow's batch policy (the WHOLE policy)",
		Long: `PUT /v2/workflows/{id}/batch-policy. The body is the ENTIRE policy and replaces
what is stored: a section you leave out is removed, and '{}' clears the policy. To change
one section, read the policy, edit it and write it back:

    altscore workflows-v2 batch-policy get my-wf | jq .batchPolicy > policy.json
    # edit policy.json
    altscore workflows-v2 batch-policy set my-wf --body @policy.json

postProcessing {enabled, workflowAlias} runs another PUBLISHED flow when a batch
finishes. Rules (checked here first, enforced by the backend):
  - not together with delivery.enabled (BATCH_POST_PROCESSING_EXCLUDES_DELIVERY)
  - enabled needs workflowAlias       (BATCH_POST_PROCESSING_WORKFLOW_REQUIRED)
  - the flow must qualify             (BATCH_POST_PROCESSING_WORKFLOW_INCOMPATIBLE):
    published, not this same workflow, and requiring no input other than 'batch'.
    'post-processing candidates <workflow>' lists which flows do and why the rest do not.

The policy applies to batches launched AFTER it is saved: a batch runs the flow that
was named when it was launched. An alias resolves to the ACTIVE version, else the
latest DRAFT.`,
		Example: `  altscore workflows-v2 batch-policy set my-wf --body '{"postProcessing":{"enabled":true,"workflowAlias":"post-my-wf"}}'
  altscore workflows-v2 batch-policy set my-wf --body @policy.json
  altscore workflows-v2 batch-policy set my-wf --body '{}'      # clear it`,
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
			data, err := setWorkflowBatchPolicy(c, args[0], body, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
	cmd.Flags().StringVar(&bodyFlag, "body", "", "the whole policy as JSON, @file.json, or piped via stdin")
	return cmd
}

func setWorkflowBatchPolicy(c *client.Client, aliasOrID string, body json.RawMessage, errOut io.Writer) (json.RawMessage, error) {
	policy, err := preflightBatchPolicyBody(body)
	if err != nil {
		return nil, err
	}
	id, err := resolveWorkflowIDForBatchPolicy(c, aliasOrID, errOut)
	if err != nil {
		return nil, err
	}
	data, _, err := c.Do("PUT", "borrower_central", fmt.Sprintf("/v2/workflows/%s/batch-policy", id), body)
	if err != nil {
		return nil, err
	}
	// A backend that predates postProcessing accepts the body and drops the unknown
	// section without a word: the write "worked" and nothing will run.
	if sent, _ := batchPolicySection(policy, "postProcessing")["enabled"].(bool); sent {
		var res struct {
			BatchPolicy map[string]any `json:"batchPolicy"`
		}
		_ = json.Unmarshal(data, &res)
		if stored, _ := batchPolicySection(res.BatchPolicy, "postProcessing")["enabled"].(bool); !stored {
			fmt.Fprintln(errOut, "# WARNING: the stored policy has no enabled postProcessing: this Borrower Central ignored the section "+
				"(batch post-processing is not deployed there). No flow will run when a batch finishes.")
		}
	}
	return data, nil
}

type postProcessingReason struct {
	Code    string   `json:"code"`
	Inputs  []string `json:"inputs"`
	Message string   `json:"message"`
}

type postProcessingCandidate struct {
	WorkflowID string                `json:"workflowId"`
	Alias      string                `json:"alias"`
	Label      string                `json:"label"`
	Version    any                   `json:"version"`
	Compatible bool                  `json:"compatible"`
	Reason     *postProcessingReason `json:"reason"`
}

type postProcessingCandidates struct {
	WorkflowID string                    `json:"workflowId"`
	Candidates []postProcessingCandidate `json:"candidates"`
}

func (c postProcessingCandidate) reasonText() string {
	if c.Compatible || c.Reason == nil {
		return ""
	}
	if c.Reason.Message != "" {
		return c.Reason.Message
	}
	if len(c.Reason.Inputs) > 0 {
		return fmt.Sprintf("%s: %s", c.Reason.Code, strings.Join(c.Reason.Inputs, ", "))
	}
	return c.Reason.Code
}

func fetchPostProcessingCandidates(c *client.Client, workflowID string) (*postProcessingCandidates, error) {
	path := fmt.Sprintf("/v2/workflows/%s/post-processing-candidates", workflowID)
	data, status, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, postProcessingRouteError(err, status, "GET /v2/workflows/{id}/post-processing-candidates")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil || probe["candidates"] == nil {
		return nil, fmt.Errorf("unexpected response from GET %s (no 'candidates' list): this Borrower Central "+
			"does not serve the route yet (batch post-processing is not deployed there)", path)
	}
	var res postProcessingCandidates
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("could not parse the candidates response: %w", err)
	}
	if res.Candidates == nil {
		res.Candidates = []postProcessingCandidate{}
	}
	return &res, nil
}

func filterCompatibleCandidates(in []postProcessingCandidate) []postProcessingCandidate {
	out := make([]postProcessingCandidate, 0, len(in))
	for _, cand := range in {
		if cand.Compatible {
			out = append(out, cand)
		}
	}
	return out
}

func renderPostProcessingCandidates(w io.Writer, candidates []postProcessingCandidate) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ALIAS\tLABEL\tVERSION\tCOMPATIBLE\tREASON")
	for _, cand := range candidates {
		version := "-"
		if cand.Version != nil {
			version = fmt.Sprintf("%v", cand.Version)
		}
		compatible := "no"
		if cand.Compatible {
			compatible = "yes"
		}
		reason := cand.reasonText()
		if reason == "" {
			reason = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", cand.Alias, cand.Label, version, compatible, reason)
	}
	return tw.Flush()
}

func makeWfv2PostProcessingGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "post-processing",
		Short: "Flows that run when a batch of a v2 workflow finishes",
		Long: `A post-processing flow is an ordinary published workflow named in another
workflow's batch policy (batchPolicy.postProcessing). When a batch finishes, rows and
automatic retries included, it is started with ONE input, 'batch' (id, label, status,
createdByEmail, total, successful, failed, ...), and decides what to do with the
results: typically a spreadsheet-builder node with a 'batch-results' block, then a
contact node mailing the file.

  candidates        which published flows can be one, and why the others cannot
  create-template   create the usual one, publish it and select it
  row-fields        the fields the rows of the batches it receives carry
  (select your own) altscore workflows-v2 batch-policy set <workflow> --body ...
  (re-run it)       altscore workflows-v2 batch post-process <batch-id>

Reference: 'altscore workflows-v2 schema-guide batchPolicy'.`,
	}
	group.AddCommand(makeWfv2PostProcessingCandidatesCmd())
	group.AddCommand(makeWfv2PostProcessingCreateTemplateCmd())
	group.AddCommand(makeWfv2PostProcessingRowFieldsCmd())
	return group
}

func makeWfv2PostProcessingCandidatesCmd() *cobra.Command {
	var compatibleOnly, asTable bool
	cmd := &cobra.Command{
		Use:   "candidates <alias-or-id>",
		Short: "List the published flows that can run as this workflow's batch post-processing",
		Long: `GET /v2/workflows/{id}/post-processing-candidates: every PUBLISHED flow of the
tenant, with whether it can run when a batch of <alias-or-id> finishes and, when not,
the reason. A flow qualifies when it is published, is not this same workflow, and
requires no input other than 'batch' (an input with a default is not required).

Reason codes: REQUIRES_INPUT, REQUIRES_ONE_OF, BATCH_INPUT_TYPE, SAME_WORKFLOW,
NOT_PUBLISHED.

Prints the response as JSON ({workflowId, candidates: [{workflowId, alias, label,
version, compatible, reason}]}); --table prints an aligned table for a person.
--compatible-only filters both.`,
		Example: `  altscore workflows-v2 post-processing candidates my-wf | jq -r '.candidates[] | select(.compatible) | .alias'
  altscore workflows-v2 post-processing candidates my-wf --compatible-only
  altscore workflows-v2 post-processing candidates my-wf --table`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			id, err := resolveWorkflowIDForBatchPolicy(c, args[0], cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := fetchPostProcessingCandidates(c, id)
			if err != nil {
				return err
			}
			total := len(res.Candidates)
			if compatibleOnly {
				res.Candidates = filterCompatibleCandidates(res.Candidates)
			}
			// JSON by default, like every other command: this CLI is read by agents.
			if !asTable {
				return output.JSON(res)
			}
			if len(res.Candidates) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "# no candidates (%d published flow(s) in the tenant). "+
					"'altscore workflows-v2 post-processing create-template %s' creates the usual one.\n", total, args[0])
				return nil
			}
			return renderPostProcessingCandidates(cmd.OutOrStdout(), res.Candidates)
		},
	}
	cmd.Flags().BoolVar(&compatibleOnly, "compatible-only", false, "show only the flows that can run as post-processing")
	cmd.Flags().BoolVar(&asTable, "table", false, "print an aligned table for a person instead of JSON")
	return cmd
}

type batchRowFieldsSource struct {
	Alias  string   `json:"alias"`
	Label  string   `json:"label"`
	Fields []string `json:"fields"`
}

type batchRowFields struct {
	Sources        []batchRowFieldsSource `json:"sources"`
	Fields         []string               `json:"fields"`
	StandardFields []string               `json:"standardFields"`
}

func fetchBatchRowFields(c *client.Client, workflowID string) (*batchRowFields, error) {
	path := fmt.Sprintf("/v2/workflows/%s/batch-row-fields", workflowID)
	data, status, err := c.Do("GET", "borrower_central", path, nil)
	if err != nil {
		return nil, postProcessingRouteError(err, status, "GET /v2/workflows/{id}/batch-row-fields")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil || probe["fields"] == nil {
		return nil, fmt.Errorf("unexpected response from GET %s (no 'fields' list): this Borrower Central "+
			"does not serve the route yet", path)
	}
	var res batchRowFields
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("could not parse the batch-row-fields response: %w", err)
	}
	if res.Sources == nil {
		res.Sources = []batchRowFieldsSource{}
	}
	if res.Fields == nil {
		res.Fields = []string{}
	}
	if res.StandardFields == nil {
		res.StandardFields = []string{}
	}
	return &res, nil
}

// One line per field: the union first, in the backend's order, with the source flows
// that produce it; then the standard fields every row has.
func renderBatchRowFields(w io.Writer, res *batchRowFields) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tFROM")
	for _, field := range res.Fields {
		var from []string
		for _, src := range res.Sources {
			for _, f := range src.Fields {
				if f == field {
					from = append(from, src.Alias)
					break
				}
			}
		}
		if len(from) == 0 {
			from = []string{"-"}
		}
		fmt.Fprintf(tw, "%s\t%s\n", field, strings.Join(from, ", "))
	}
	for _, field := range res.StandardFields {
		fmt.Fprintf(tw, "%s\t(standard)\n", field)
	}
	return tw.Flush()
}

func makeWfv2PostProcessingRowFieldsCmd() *cobra.Command {
	var asTable bool
	cmd := &cobra.Command{
		Use:   "row-fields <alias-or-id>",
		Short: "List the fields the batch rows a post-processing flow receives carry",
		Long: `GET /v2/workflows/{id}/batch-row-fields. <alias-or-id> is the POST-PROCESSING flow
(the one being authored), not the workflow whose batches it receives.

The sources are the workflows whose batchPolicy.postProcessing names this flow's alias;
each one's fields are what its End node outputs (outputJson / inputMappings), i.e. the
columns of every row of its batches. 'fields' is their union, in order;
'standardFields' are on every row whatever the workflow (execution_id, item_index,
is_success, error_message).

Use them as the dotted paths of a batch-results node (batchResultsConfig.fields, for
mode 'counts' or 'rows') or of a spreadsheet column's 'field'. No source yet (no
workflow names this flow) answers empty lists: a path you know is still accepted.

Prints the response as JSON ({sources: [{alias, label, fields}], fields,
standardFields}); --table prints one line per field with the flows it comes from.`,
		Example: `  altscore workflows-v2 post-processing row-fields post-scoring | jq -r '.fields[]'
  altscore workflows-v2 post-processing row-fields post-scoring --table`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			id, err := resolveWorkflowIDForBatchPolicy(c, args[0], cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			res, err := fetchBatchRowFields(c, id)
			if err != nil {
				return err
			}
			if !asTable {
				return output.JSON(res)
			}
			if len(res.Sources) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "# no workflow names %s as its batch post-processing yet: only the "+
					"standard fields are known. Select it with 'altscore workflows-v2 batch-policy set <workflow>'.\n", args[0])
			}
			return renderBatchRowFields(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().BoolVar(&asTable, "table", false, "print an aligned table for a person instead of JSON")
	return cmd
}

func makeWfv2PostProcessingCreateTemplateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create-template <alias-or-id>",
		Short: "Create the default post-processing flow for a workflow, publish it and select it",
		Long: `POST /v2/workflows/{id}/post-processing-template (no body). Creates a new flow
'post-<alias>':

    Start(batch) -> spreadsheet-builder (sheet "Resultados", one 'batch-results' block
                    with every row of the batch)
                 -> has the launcher an email? -> contact: mail the file to them -> End
                                               -> otherwise a notice, no email   -> End

publishes it, and names it in <alias-or-id>'s batch policy. This WRITES: it creates and
publishes a workflow and it turns the policy's results email off (delivery.enabled =
false; automatic retries are kept). The new flow is an ordinary one: export it with
'workflows-v2 export <id> --format apply-spec', edit and 'apply' it like any other.

Prints {workflowId, workflowAlias, published, batchPolicy}.`,
		Example: `  altscore workflows-v2 post-processing create-template my-wf`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			id, err := resolveWorkflowIDForBatchPolicy(c, args[0], cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			data, err := createPostProcessingTemplate(c, id)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func createPostProcessingTemplate(c *client.Client, workflowID string) (json.RawMessage, error) {
	path := fmt.Sprintf("/v2/workflows/%s/post-processing-template", workflowID)
	data, status, err := c.Do("POST", "borrower_central", path, nil)
	if err != nil {
		return nil, postProcessingRouteError(err, status, "POST /v2/workflows/{id}/post-processing-template")
	}
	return data, nil
}

func makeWfv2BatchPostProcessCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "post-process <batch-id>",
		Short: "Run a finished batch's post-processing flow again (no row is re-run)",
		Long: `POST /v2/workflows/{batchId}/post-process. For a batch launched with
batchPolicy.postProcessing whose flow failed (batch status post_processing_failed), or
to produce its files again. Rows are NOT re-run. Answers 202 {batchId, status:
"post_processing", runId}.

Refusals:
  409 BATCH_POST_PROCESSING_NOT_RETRYABLE    the batch has not finished; it must be
                                             complete or post_processing_failed
  409 BATCH_POST_PROCESSING_ALREADY_RUNNING  the flow is running right now
  400 BATCH_HAS_NO_POST_PROCESSING           the batch was launched without a flow

Follow it with 'altscore execution-batches get <batch-id>' (.status, .postProcessing).
The flow's files are the attachments of its run:
'altscore executions get-output-attachments <postProcessing.executionId>'.`,
		Example: `  altscore workflows-v2 batch post-process <batch-id>
  altscore execution-batches get <batch-id> | jq '{status, postProcessing}'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, err := retryBatchPostProcessing(c, args[0])
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

var batchPostProcessHints = map[string]string{
	"BATCH_POST_PROCESSING_NOT_RETRYABLE": "post-processing can be re-run once the batch is complete or post_processing_failed; " +
		"check 'altscore execution-batches get %s'",
	"BATCH_POST_PROCESSING_ALREADY_RUNNING": "nothing to do: wait for it and read .postProcessing from " +
		"'altscore execution-batches get %s'",
	"BATCH_HAS_NO_POST_PROCESSING": "batch %s was launched while its workflow's policy named no flow, and a batch runs the flow named at LAUNCH. " +
		"Set one ('altscore workflows-v2 batch-policy set') for the next batches; this one keeps its default results file ('workflows-v2 download')",
}

func retryBatchPostProcessing(c *client.Client, batchID string) (json.RawMessage, error) {
	path := fmt.Sprintf("/v2/workflows/%s/post-process", batchID)
	data, status, err := c.DoKeepBody("POST", "borrower_central", path, nil)
	if err != nil {
		return nil, err
	}
	if status >= 200 && status < 300 {
		return data, nil
	}
	return nil, describeBatchPostProcessError(status, data, batchID)
}

func describeBatchPostProcessError(status int, data json.RawMessage, batchID string) error {
	env, ok := parseServerErrorEnvelope(data)
	if !ok {
		detail := strings.TrimSpace(string(data))
		if len(detail) > 400 {
			detail = detail[:400] + "..."
		}
		msg := fmt.Sprintf("HTTP %d", status)
		if detail != "" {
			msg += ": " + detail
		}
		return postProcessingRouteError(errors.New(msg), status, "POST /v2/workflows/{batchId}/post-process")
	}
	subCode, _ := env.Details["errorSubCode"].(string)
	// These refusals carry their sentence in details.message; the envelope's own
	// message is the generic one of the error class.
	message, _ := env.Details["message"].(string)
	if message == "" {
		message = env.Message
	}
	msg := fmt.Sprintf("HTTP %d %s", status, env.Code)
	if message != "" {
		msg += ": " + message
	}
	if subCode != "" {
		msg += " [errorSubCode=" + subCode + "]"
	}
	if hint, known := batchPostProcessHints[subCode]; known {
		return fmt.Errorf("%s -- "+hint, msg, batchID)
	}
	return postProcessingRouteError(errors.New(msg), status, "POST /v2/workflows/{batchId}/post-process")
}

type publishWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	UsedBy  []struct {
		Alias   string `json:"alias"`
		Label   string `json:"label"`
		Version any    `json:"version"`
		Status  string `json:"status"`
	} `json:"usedBy"`
}

// The publish already happened: warnings go to stderr and never change the exit code or
// what is printed on stdout. Any shape other than a list of {code, message} is left to
// the JSON on stdout.
func printPublishWarnings(w io.Writer, warningsRaw json.RawMessage) int {
	var warnings []publishWarning
	if len(warningsRaw) == 0 || json.Unmarshal(warningsRaw, &warnings) != nil {
		return 0
	}
	printed := 0
	for _, warning := range warnings {
		if warning.Code == "" && warning.Message == "" {
			continue
		}
		printed++
		fmt.Fprintf(w, "# WARNING [%s]: %s\n", stringOr(warning.Code, "publish"), warning.Message)
		for _, user := range warning.UsedBy {
			fmt.Fprintf(w, "#   used by %s (%s, v%v %s)\n", stringOr(user.Alias, "?"), user.Label, user.Version, user.Status)
		}
		if warning.Code == "POST_PROCESSING_FLOW_INCOMPATIBLE" {
			fmt.Fprintln(w, "#   The workflow IS published. The next batch of each workflow above will end "+
				"post_processing_failed until this flow again requires no input other than 'batch' "+
				"(or their batch policy names another flow: 'altscore workflows-v2 post-processing candidates <workflow>').")
		}
	}
	return printed
}

func printPublishResponseWarnings(w io.Writer, response json.RawMessage) int {
	var envelope struct {
		Warnings json.RawMessage `json:"warnings"`
	}
	if json.Unmarshal(response, &envelope) != nil {
		return 0
	}
	return printPublishWarnings(w, envelope.Warnings)
}
