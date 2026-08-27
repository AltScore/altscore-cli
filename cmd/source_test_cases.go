package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// registerSourceTestCases wires the "source-test-cases" command group: tenant-authored
// canned responses for a data source.
//
// A workflow node that names one gets it back instead of calling the provider, so a
// production source can be queried once and replayed from then on. Two things about them
// shape this command surface:
//
//   - Selection is by NAME, and the name is the ADDRESS. It goes on a node as
//     `testCase: "tenant:<name>"`, which is a bare string inside a saved workflow body,
//     and nothing indexes those back to the case. So the backend refuses to rename one,
//     and `update` here changes the label rather than the address.
//   - An unresolvable reference FAILS the node rather than falling back to a real call.
//     That is the point (a test case is a request NOT to query the provider), but it means
//     `delete` on a case a published workflow names breaks that node, and no query here
//     can tell you which workflows those are.
//
// Standard list/get/create/update/delete come from registerResource. Two custom
// subcommands cover what the generic builder cannot express: `from-package`, which
// records a package a real run already produced, and `set-content`, which replaces a
// stored body without touching identity.
func registerSourceTestCases() {
	group := registerResource(ResourceDef{
		Name:     "source-test-cases",
		Singular: "source-test-case",
		BasePath: "/v1/documentation/source-test-cases",
		Module:   "borrower_central",
		Actions:  []string{"list", "get", "create", "update", "delete"},
		Description: `Manage tenant-authored test cases for data sources.

A test case is a canned response you record for one source. Reference it from an
altdata-enrichment node as:

  "sourcesConfig": [{"sourceId": "ECU-PUB-0063", "version": "v2",
                     "testCase": "tenant:docs-completos"}]

The 'tenant:' prefix is what separates your cases from AltScore's curated ones, so
the two can never be confused. Unlike those, a tenant case also works for sources
your tenant defined itself -- those have no curated examples and cannot be mocked
any other way -- and it works on batch and async runs.

A resolved case never touches the package cache: nothing is read from it, and the
canned response is never written back, so a later run without a testCase can never
be served your mock as if it were real data.`,
		CreateSchema: `  sourceId: string      [required] The source this case answers for
  sourceVersion: string [required] e.g. "v2". A tenant-defined source is always "v1"
  name: string          [required] The address, after the 'tenant:' prefix.
                        Lowercase letters, digits, . _ - ; max 64 chars.
                        'any' is reserved by AltScore. NOT changeable later.
  label: string         What the builder's picker shows. Rename this freely
  description: string   Free text
  content: object       [required] The response body -- see below`,
		UpdateSchema: `  label: string       What the picker shows
  description: string Free text

  'name' is rejected, not ignored: it is the address a saved workflow stores, so
  moving it would break those workflows silently. Create a case under the new name
  instead. Use 'set-content' to replace the body.`,
		ResponseSchema: `  id, sourceId, sourceVersion, name,
  testCaseRef       what you put on a node: "tenant:<name>"
  label, description, packageId,
  cacheRequest      the inputs of the run this was promoted from, when it was
                    promoted. Provenance only -- selection is by name alone
  createdAt, createdBy, updatedAt`,
		FilterHelp: `  sourceId=<id>          only cases for this source
  sourceVersion=<ver>    only cases for this version
  search=<text>          matches name and label`,
	})

	group.AddCommand(makeSourceTestCaseFromPackageCmd())
	group.AddCommand(makeSourceTestCaseSetContentCmd())
	group.AddCommand(makeSourceTestCaseBodyHelpCmd())
}

// makeSourceTestCaseFromPackageCmd hits POST /from-package/{packageId}: record the body
// of a package a real run already produced.
//
// This is the shortest honest path to a mock, and the reason the endpoint exists: run the
// node for real once, promote the package, then every later run replays it for free. The
// source and version are read off the package rather than supplied, so a case can never
// claim to answer for a source whose response it does not actually hold.
func makeSourceTestCaseFromPackageCmd() *cobra.Command {
	var nameFlag, labelFlag, descFlag string

	cmd := &cobra.Command{
		Use:   "from-package <packageId>",
		Short: "Record an existing package's body as a test case",
		Long: `Promote a package a real run produced into a reusable test case.

Find the package id first, e.g.

  altscore api GET '/v1/stores/packages?borrower-id=<id>&source-id=AD_ECU-PUB-0063_v2'

sourceId and sourceVersion come from the package, not from you.

Attachments are NOT copied -- a test case holds the JSON body only. If the package
has any, the response carries a warning saying so, because a document source that
looks fully mocked while its documents are missing is worse than one that admits it.`,
		Example: `  altscore source-test-cases from-package <packageId> --name docs-completos
  altscore source-test-cases from-package <packageId> --name sin-datos --label "No records"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if nameFlag == "" {
				return fmt.Errorf("--name is required: it is the address you reference as tenant:<name>")
			}
			c, err := loadClient()
			if err != nil {
				return err
			}

			payload := map[string]string{"name": nameFlag}
			if labelFlag != "" {
				payload["label"] = labelFlag
			}
			if descFlag != "" {
				payload["description"] = descFlag
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}

			path := "/v1/documentation/source-test-cases/from-package/" + args[0]
			data, _, err := c.Do("POST", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&nameFlag, "name", "", "the case name; referenced as tenant:<name> (required)")
	cmd.Flags().StringVar(&labelFlag, "label", "", "what the builder's picker shows")
	cmd.Flags().StringVar(&descFlag, "description", "", "free-text description")
	return cmd
}

// makeSourceTestCaseSetContentCmd hits PUT /{id}/content.
//
// Separate from `update` on purpose: identity and body are edited independently, so
// rewriting what a case RETURNS never risks moving what it is CALLED. contentType is not
// a parameter -- the backend pins it, which is how this avoids the generic package
// content route's bug of accepting a new type without updating the stored one.
func makeSourceTestCaseSetContentCmd() *cobra.Command {
	var bodyFlag string

	cmd := &cobra.Command{
		Use:   "set-content <id>",
		Short: "Replace a test case's stored response body",
		Long: `Replace the response body a test case returns. Identity (source, version,
name) is untouched, so every workflow already referencing it keeps working and
starts getting the new body.

Pass the body as a JSON object via --body, @file, or stdin. Either the bare body
or a { "content": <body> } wrapper is accepted.

Run 'altscore source-test-cases body-help' for what the body should contain.`,
		Example: `  altscore source-test-cases set-content <id> --body @adjusted.json
  jq '.data.score = 720' original.json | altscore source-test-cases set-content <id>`,
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
			path := "/v1/documentation/source-test-cases/" + args[0] + "/content"
			data, _, err := c.Do("PUT", "borrower_central", path, body)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}

	cmd.Flags().StringVar(&bodyFlag, "body", "", "JSON body (or @file, or pipe via stdin)")
	return cmd
}

// makeSourceTestCaseBodyHelpCmd prints what a test case body has to contain.
//
// It is a command rather than more prose in --help because getting this wrong is the one
// mistake with no loud failure: a body in the wrong shape is served exactly as stored, so
// the node returns something the graph does not expect instead of erroring.
func makeSourceTestCaseBodyHelpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "body-help",
		Short: "What a test case's content must contain",
		Long: `A test case must hold what the source really produces, because the runtime
serves it verbatim rather than coercing it.

MOST SOURCES store the enrichment envelope. This is also exactly what a package
for that source contains, which is why 'from-package' is the reliable way to get
one:

  {
    "sourceId": "ECU-PUB-0063",
    "version": "v2",
    "isSuccess": true,
    "requestId": null,
    "data":       { ... the flattened data the workflow reads ... },
    "sourceData": { ... the raw provider payload ... },
    "inputs": {},
    "requestedAt": null,
    "status": "completed"
  }

'sourceId' must match the case's source, or the create is rejected -- pasting
another source's sample is the common mistake and it would otherwise leave every
downstream reader disagreeing with the node.

TO MOCK A FAILED SOURCE, which is the case AltScore's curated examples express
poorly, set:

  "isSuccess": false, "status": "failed", "errorMessage": "provider timeout"

A SOURCE WITH response_mode "raw" (some tenant-defined sources) stores the
provider body verbatim instead, with no envelope. For those, the content IS that
body, exactly as the provider returns it.

Attachments are not supported: a test case holds JSON only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
}
