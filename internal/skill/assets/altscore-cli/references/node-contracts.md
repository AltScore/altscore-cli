## Node contracts the schema guide has no notes for

`schema-guide tasks <type>` gives these four only introspected fields. What each takes, what it returns and what passes validation but misbehaves at run time:

### `contact` (and End's `emailReport`)

Sends one email. `contactConfig`: `to`, `cc`, `bcc` (comma-separated), `subject`, `body`, `contentType` (`text`|`html`), `senderMode` (`generic` default, or `connector` + `connectorId`), `fromName`, `attachments: [{token, filename}]` (token = an inputMappings key resolving to a URL). Email only: any other `channel` saves and then fails the node. Nothing looks a recipient up for you: map it (`"cust_email": "entity.borrower.points_of_contact.email"`) and write `to: "{cust_email}"`. `{key}` reads this node's inputMappings, a workflow input or a custom variable; `{{inputs.x}}` paths work too; an unresolved placeholder is sent as written.

Output: `task_outputs.<ref>.{isSuccess, channel, messageId, to}`. A bad `to` fails the node; bad `cc`/`bcc` are dropped silently. A test run mails only the user who ran it, subject `[TEST]`.

To mail the outcome use `endConfig.emailReport` (the same fields plus `enabled`, `reportFilename`), never a node after End. It sends only when End rendered a PDF, never in a batch run, and a failed send is a notice, not a failure. Its `{key}` reads only End's own inputMappings, and an `entity.*` mapping arrives unresolved there, so map the address into a node upstream.

### `notices`

Adds one `{message, severity}` to the execution's notices (Hub panel, live push) to say why a branch ran. `noticesConfig: {message, severity: info|warning|error}`; `{key}` and `{{task_outputs.<ref>.f}}` placeholders as above. It never stops the run and its output (`isSuccess` always true) is nothing to branch on: stop with an `exception` node, put text in the response with End `outputJson`. Severity is how the notice displays; it never stops the run or marks a data source as failed. A notice on a branch that did not run emits nothing; `debug` is v1-only.

### `document-extraction`

Reads one document and returns the fields you declare. `documentExtractionConfig`: exactly one source of `documentUrl`, `documentBase64` or `rawText` (a config value or an inputMappings entry with that key, the mapping wins; a private file needs a signed URL, there is no document-id field), `filename`, and `extractionSchema` as an object schema, `{"type": "object", "properties": {"<name>": {"type": "string", "description": "..."}}}`. A bare map (`{"<name>": "string"}`) passes preflight and fails at run time; a property whose value is not an object is never asked for. Optional: `instructions`, `provider` (`llm`|`ocr-tools`), `includeFullText`, `retryMissingFields`.

Output, wired directly with no compute node: `task_outputs.<ref>.fields.<name>` (every declared name, null when absent), `.missingFields` (the null ones; `required` is ignored, so leave optional fields out), `.isSuccess` (false only when extraction could not run; reason in `.issue`). Gate in a conditional or rule on `isSuccess` `is_true` AND `missingFields` `is_empty`, never `is_empty` alone: it is also true when the node never ran. Per field: `is_not_null`.

### Reading a child workflow's output in the parent

The parent runs the child's ACTIVE version, never your draft, and gets back only the child's End `outputJson` (plus `pdf_url` when the child rendered a PDF). The child's standard output and decision never reach the parent, so emit what the parent needs in the child's `outputJson`.
- Single run (no `inputExpression`, or one resolving to an object): `task_outputs.<childRef>.<outputJsonKey>`. A failed child returns `{}` and the node still succeeds: emit a literal such as `"ok": true` in the child and gate on it.
- Fan-out (`inputExpression` resolving to a list; `runInBatch` is display only): `.items` is `[{index, output} | {index, error}]` in input order, `.summary.{total, success, failed}`; gate on `summary.failed` `eq` 0. Each element is the child's whole input (a scalar arrives as `{item: <v>}`).
- `dispatchMode: "async-batch"` returns a dispatch receipt, no outputs.

Pass `borrower_id` (`"borrower_id": "system.primary_borrower_id"`, or a field of every fan-out element): without it the child's sources skip the cache and save no packages. In a fan-out, a child that ends on an `exception` node, times out or hits a graph error lands in `items` as `{index, error}` and counts in `summary.failed`; a single run returns `{}` for any failed child.
