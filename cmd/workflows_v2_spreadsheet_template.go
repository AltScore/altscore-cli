package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AltScore/altscore-cli/internal/client"
	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/spf13/cobra"
)

// The spreadsheet-builder's template mode reads an .xlsx the tenant uploaded once. The
// Hub uploads it to the generic attachments route; this is the same call, so a CLI user
// gets a template.objectName without hand-rolling multipart.

const (
	spreadsheetTemplateUploadPath = "/v1/stores/packages/commands/attachments/upload"
	// BC's node refuses a template above this (spreadsheet_template.MAX_TEMPLATE_BYTES);
	// the upload route itself would take up to 32 MB, which would only fail later.
	spreadsheetTemplateMaxBytes = 25 * 1024 * 1024
	// The worker reads a template only under the tenant's attachments prefix.
	spreadsheetTemplateObjectPrefix = "attachments/"
)

type spreadsheetTemplateRef struct {
	ObjectName string `json:"objectName"`
	FileLabel  string `json:"fileLabel"`
}

// checkSpreadsheetTemplateFile refuses, before any request, what the node would refuse.
func checkSpreadsheetTemplateFile(filePath string) error {
	if !strings.EqualFold(filepath.Ext(filePath), ".xlsx") {
		return fmt.Errorf("%s is not an .xlsx file: a spreadsheet-builder template must be an Excel .xlsx "+
			"(re-save .xls / .xlsm / .csv as .xlsx)", filePath)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", filePath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not an .xlsx file", filePath)
	}
	if info.Size() > spreadsheetTemplateMaxBytes {
		return fmt.Errorf("%s is %.1f MB: a spreadsheet-builder template is at most 25 MB",
			filePath, float64(info.Size())/(1024*1024))
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty", filePath)
	}
	return nil
}

func uploadSpreadsheetTemplate(c *client.Client, filePath string) (*spreadsheetTemplateRef, error) {
	if err := checkSpreadsheetTemplateFile(filePath); err != nil {
		return nil, err
	}
	label := filepath.Base(filePath)
	// No attachmentFileName: BC then names the object under the tenant's prefix, the
	// only place the worker reads a template from.
	newBody, contentType := multipartFileBody(filePath, "file", label)
	data, _, err := c.DoRaw("POST", "borrower_central", spreadsheetTemplateUploadPath, newBody, contentType)
	if err != nil {
		return nil, err
	}
	var res struct {
		FileName string `json:"fileName"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("could not parse the upload response: %w", err)
	}
	if !strings.HasPrefix(res.FileName, spreadsheetTemplateObjectPrefix) {
		return nil, fmt.Errorf("the upload answered fileName %q, not an %s<tenant>/... object: the "+
			"spreadsheet-builder worker would refuse it as template.objectName", res.FileName, spreadsheetTemplateObjectPrefix)
	}
	return &spreadsheetTemplateRef{ObjectName: res.FileName, FileLabel: label}, nil
}

func makeWfv2SpreadsheetGroupCmd() *cobra.Command {
	group := &cobra.Command{
		Use:   "spreadsheet",
		Short: "Helpers for the spreadsheet-builder node (Excel files)",
		Long: `Helpers for the spreadsheet-builder node. The node itself is authored with
'workflows-v2 apply'; its config reference is
'altscore workflows-v2 schema-guide tasks spreadsheet-builder'.

  upload-template   upload an .xlsx for source "template"; prints template.objectName / fileLabel`,
	}
	group.AddCommand(makeWfv2SpreadsheetUploadTemplateCmd())
	return group
}

func makeWfv2SpreadsheetUploadTemplateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload-template <path.xlsx>",
		Short: "Upload an .xlsx template for a spreadsheet-builder node",
		Long: `Uploads an Excel file to the tenant's attachments
(POST /v1/stores/packages/commands/attachments/upload, multipart field 'file'), the
same call the Hub makes, and prints what goes into the node's config:

  {"objectName": "attachments/<tenant>/<uuid>.xlsx", "fileLabel": "<file name>"}

Copy both into spreadsheetBuilderConfig.template (source "template"). Refused before
uploading: anything that is not .xlsx, and files over 25 MB (the node's limit).
Needs the bc.attachments.upload permission. Each run uploads a new object; re-run it
only when the template changes.`,
		Example: `  altscore workflows-v2 spreadsheet upload-template ./Plantilla.xlsx
  altscore workflows-v2 spreadsheet upload-template ./Plantilla.xlsx > template.json
  jq --slurpfile t template.json '(.nodes[] | select(.ref=="planilla") | .spreadsheetBuilderConfig.template) += $t[0]' spec.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Local checks first: a wrong file needs no login.
			if err := checkSpreadsheetTemplateFile(args[0]); err != nil {
				return err
			}
			c, err := loadClient()
			if err != nil {
				return err
			}
			ref, err := uploadSpreadsheetTemplate(c, args[0])
			if err != nil {
				return err
			}
			return output.JSON(ref)
		},
	}
}
