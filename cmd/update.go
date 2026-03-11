package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/AltScore/altscore-cli/internal/version"
	"github.com/spf13/cobra"
)

const (
	repoOwner = "AltScore"
	repoName  = "altscore-cli"
	releaseURL = "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/releases/latest"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update altscore to the latest version",
	Long: `Check for a newer version on GitHub Releases and update the binary in-place.

Downloads the platform-specific binary, verifies its SHA-256 checksum against
the published checksums.txt, and replaces the current executable.`,
	Example: `  altscore update`,
	RunE:    runUpdate,
}

func init() {
	rootCmd.AddCommand(updateCmd)
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	APIURL             string `json:"url"`
}

func runUpdate(cmd *cobra.Command, args []string) error {
	current := version.Version
	fmt.Fprintf(os.Stderr, "Current version: %s\n", current)

	// Fetch latest release metadata.
	rel, err := fetchLatestRelease()
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Latest version:  %s\n", rel.TagName)

	if current == rel.TagName {
		fmt.Fprintln(os.Stderr, "Already up to date.")
		return nil
	}

	if current == "dev" {
		fmt.Fprintln(os.Stderr, "Warning: running a dev build. Updating to the latest release.")
	}

	// Determine the asset name for this platform.
	assetName := fmt.Sprintf("altscore-%s-%s", runtime.GOOS, runtime.GOARCH)

	// When GITHUB_TOKEN is set (private repo), use the API URL with
	// Accept: application/octet-stream. For public repos, browser_download_url
	// works without auth.
	useAPI := os.Getenv("GITHUB_TOKEN") != ""
	pickURL := func(a ghAsset) string {
		if useAPI {
			return a.APIURL
		}
		return a.BrowserDownloadURL
	}

	var binaryURL, checksumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case assetName:
			binaryURL = pickURL(a)
		case "checksums.txt":
			checksumsURL = pickURL(a)
		}
	}
	if binaryURL == "" {
		return fmt.Errorf("no release asset found for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if checksumsURL == "" {
		return fmt.Errorf("no checksums.txt found in release %s", rel.TagName)
	}

	// Download checksums and find expected hash.
	expectedHash, err := fetchExpectedChecksum(checksumsURL, assetName)
	if err != nil {
		return err
	}

	// Download the binary to a temp file.
	fmt.Fprintf(os.Stderr, "Downloading %s...\n", assetName)
	tmpFile, actualHash, err := downloadToTemp(binaryURL)
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile)

	// Verify checksum.
	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	fmt.Fprintln(os.Stderr, "Checksum verified.")

	// Replace the running binary.
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable path: %w", err)
	}

	if err := replaceBinary(tmpFile, execPath); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Updated to %s\n", rel.TagName)
	return nil
}

// githubGet performs a GET request with the given Accept header,
// attaching a GITHUB_TOKEN if available.
func githubGet(url, accept string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

func fetchLatestRelease() (*ghRelease, error) {
	resp, err := githubGet(releaseURL, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == http.StatusNotFound {
			hint = " (if the repo is private, set GITHUB_TOKEN)"
		}
		return nil, fmt.Errorf("GitHub API returned HTTP %d%s", resp.StatusCode, hint)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parsing release response: %w", err)
	}
	return &rel, nil
}

func fetchExpectedChecksum(url, assetName string) (string, error) {
	resp, err := githubGet(url, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("downloading checksums: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading checksums: %w", err)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		// Format: "<hash>  <filename>" (shasum -a 256 output uses two spaces)
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == assetName {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("no checksum found for %s in checksums.txt", assetName)
}

// downloadToTemp downloads url into a temporary file and returns (path, sha256hex, error).
func downloadToTemp(url string) (string, string, error) {
	resp, err := githubGet(url, "application/octet-stream")
	if err != nil {
		return "", "", fmt.Errorf("downloading binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "altscore-update-*")
	if err != nil {
		return "", "", err
	}

	hasher := sha256.New()
	w := io.MultiWriter(tmp, hasher)
	if _, err := io.Copy(w, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", "", fmt.Errorf("writing binary: %w", err)
	}
	tmp.Close()

	return tmp.Name(), hex.EncodeToString(hasher.Sum(nil)), nil
}

// replaceBinary atomically replaces dst with src, preserving permissions.
func replaceBinary(src, dst string) error {
	dstInfo, err := os.Stat(dst)
	if err != nil {
		return fmt.Errorf("stat current binary: %w", err)
	}

	if err := os.Chmod(src, dstInfo.Mode()); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}

	if err := os.Rename(src, dst); err != nil {
		// Rename fails across filesystems; fall back to copy.
		return copyFile(src, dst, dstInfo.Mode())
	}
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("opening binary for write: %w", err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("writing binary: %w", err)
	}
	return out.Close()
}
