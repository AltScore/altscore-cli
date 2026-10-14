package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// The four credit-decisioning entities (mapping tables, rule trees, scorecards, evaluation
// rules) mint an immutable version on every save, and publishing a v2 workflow pins the
// exact version each of its references reads (config.entityPins). borrower-central #2402,
// live 2026-10-14. ResourceDef.Versioned wires what follows into a group.

// Appended to the group's --help: the one place that says what an edit does now.
const versionedEntityNote = `Versions: every save (create, update, import, copy, set-test, the re-scope
'workflows-v2 apply' makes) mints an immutable version, and the entity carries
'version', its latest number. A save that changes nothing mints none.
'versions <id>' lists them; 'get-version <id> <n>' prints one.

An edit does NOT reach production on its own. A workflow version published
since 2026-10-14 runs on the entity versions pinned when it was published
('workflows-v2 pins <alias>'), so the edit goes live when each workflow that
references the entity is published again. Drafts and test runs (--test) read
the latest version, so test the edit there first. A version published before
2026-10-14 carries no pins and keeps reading the latest entity until it is
republished.`

func versionedUpdateNote(singular string) string {
	return fmt.Sprintf(`Saving mints a new version of this %s. Published workflow versions keep
running the version they pinned: the change reaches production when each
workflow that references it is published again ('workflows-v2 publish', or an
'apply' over the ACTIVE version). Drafts and test runs read it at once.
A "version" key in the body is dropped before the request: the server
assigns it on every save.`, singular)
}

func versionedDeleteNote(singular string) string {
	return fmt.Sprintf(`The versions of the %s survive the delete: a published workflow version that
pinned it keeps running on its pinned body, and 'versions <id>' still lists
them. Drafts, test runs and versions published without pins read the live %s
and lose it at once.`, singular, singular)
}

func entityVersionsPath(basePath, id string) string {
	return basePath + "/" + url.PathEscape(id) + "/versions"
}

func entityVersionPath(basePath, id string, version int) string {
	return entityVersionsPath(basePath, id) + "/" + strconv.Itoa(version)
}

// "3" and "v3" both name version 3: workflow versions are often written with the v.
func parseEntityVersion(raw string) (int, error) {
	s := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("version must be a positive integer (e.g. 3), got %q; 'versions <id>' lists them", raw)
	}
	return n, nil
}

// The server assigns "version" on every save and ignores one in a write body, but a
// get | jq | update round trip carries it, so it is dropped here instead of relying on that.
func dropServerVersion(body json.RawMessage, stderr io.Writer) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["version"]; !ok {
		return body
	}
	delete(obj, "version")
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	if stderr != nil {
		fmt.Fprintln(stderr, `# note: dropped "version" from the body: the server assigns it on every save`)
	}
	return out
}

func makeEntityVersionsCmd(def ResourceDef) *cobra.Command {
	return &cobra.Command{
		Use:   "versions <id> [version]",
		Short: fmt.Sprintf("List the stored versions of %s %s, or print one", article(def.Singular), def.Singular),
		Long: fmt.Sprintf(`GET %[1]s/{id}/versions: every stored version of the %[2]s, newest first,
as {version, createdAt, createdBy, source}. source is one of api, import,
decision_flow, revert, pin, migration or other.

With a version number it prints the %[2]s's body at that version, the 'get'
shape plus "version" (GET %[1]s/{id}/versions/{version}), exactly like
'get-version'. A published workflow version runs on the body its pin names:
'workflows-v2 pins <alias>' lists the pins with each entity id and version.

Versions outlive the %[2]s: after a delete the listing still answers.`, def.BasePath, def.Singular),
		Example: fmt.Sprintf(`  altscore %[1]s versions <id>
  altscore %[1]s versions <id> 3
  altscore %[1]s versions <id> | jq '.[0].version'`, def.Name),
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := entityVersionsPath(def.BasePath, args[0])
			if len(args) == 2 {
				n, err := parseEntityVersion(args[1])
				if err != nil {
					return err
				}
				path = entityVersionPath(def.BasePath, args[0], n)
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", def.Module, path, nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}

func makeEntityGetVersionCmd(def ResourceDef) *cobra.Command {
	return &cobra.Command{
		Use:   "get-version <id> <version>",
		Short: fmt.Sprintf("Print %s %s as it was at one version", article(def.Singular), def.Singular),
		Long: fmt.Sprintf(`GET %[1]s/{id}/versions/{version}: the %[2]s's body at that version, the 'get'
shape plus "version". 404 when the %[2]s has no such version; 'versions <id>'
lists them. Still answers after the %[2]s is deleted.

Use it to read what a published workflow actually runs: 'workflows-v2 pins
<alias>' names each pinned entity id and version.`, def.BasePath, def.Singular),
		Example: fmt.Sprintf(`  altscore %[1]s get-version <id> 2
  diff <(altscore %[1]s get-version <id> 2) <(altscore %[1]s get <id>)`, def.Name),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := parseEntityVersion(args[1])
			if err != nil {
				return err
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			data, _, err := c.Do("GET", def.Module, entityVersionPath(def.BasePath, args[0], n), nil)
			if err != nil {
				return err
			}
			return output.RawJSON(data)
		},
	}
}
