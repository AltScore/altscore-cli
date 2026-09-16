// Package skill embeds the Claude Code skill that teaches agents this CLI and
// installs it into the user's skills directory, so the skill ships with the
// binary and is refreshed whenever the binary is.
package skill

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed all:assets
var assets embed.FS

const (
	// Name is the skill's directory name and its frontmatter `name`.
	Name = "altscore-cli"
	// MarkerFile records that a directory was written by this package; a
	// directory without it is somebody's hand copy and is never overwritten
	// without --force.
	MarkerFile = ".managed-by-altscore.json"

	assetRoot = "assets/" + Name
)

// ErrUnmanaged is returned when the target directory exists but carries no
// marker: an install would clobber files this package did not write.
var ErrUnmanaged = errors.New("skill directory exists but is not managed by altscore")

// Marker is the JSON body of MarkerFile.
type Marker struct {
	Version     string    `json:"version"`
	InstalledAt time.Time `json:"installedAt"`
	Files       []string  `json:"files"`
}

// Status describes an install location.
type Status struct {
	Skill            string     `json:"skill"`
	Dir              string     `json:"dir"`
	Installed        bool       `json:"installed"`
	Managed          bool       `json:"managed"`
	InstalledVersion string     `json:"installedVersion,omitempty"`
	InstalledAt      *time.Time `json:"installedAt,omitempty"`
	EmbeddedVersion  string     `json:"embeddedVersion"`
	UpToDate         bool       `json:"upToDate"`
	FileCount        int        `json:"fileCount"`
}

// InstallResult describes what Install did.
type InstallResult struct {
	Skill     string `json:"skill"`
	Dir       string `json:"dir"`
	Version   string `json:"version"`
	Action    string `json:"action"`
	FileCount int    `json:"fileCount"`
}

// DefaultDir is where Claude Code loads user-level skills from:
// $CLAUDE_CONFIG_DIR/skills/<name>, or ~/.claude/skills/<name>.
func DefaultDir() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "skills", Name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "skills", Name), nil
}

// Files lists the embedded skill files as sorted paths relative to the skill
// root (SKILL.md, references/...).
func Files() ([]string, error) {
	var out []string
	err := fs.WalkDir(assets, assetRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(assetRoot, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ReadFile returns one embedded skill file by its relative path.
func ReadFile(rel string) ([]byte, error) {
	return assets.ReadFile(assetRoot + "/" + filepath.ToSlash(rel))
}

// Inspect reports the state of dir against the embedded skill version.
func Inspect(dir, embeddedVersion string) (Status, error) {
	st := Status{Skill: Name, Dir: dir, EmbeddedVersion: embeddedVersion}
	files, err := Files()
	if err != nil {
		return st, err
	}
	st.FileCount = len(files)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	st.Installed = len(entries) > 0
	if !st.Installed {
		return st, nil
	}

	m, err := readMarker(dir)
	if err != nil {
		return st, nil
	}
	st.Managed = true
	st.InstalledVersion = m.Version
	if !m.InstalledAt.IsZero() {
		t := m.InstalledAt
		st.InstalledAt = &t
	}
	st.UpToDate = m.Version == embeddedVersion
	return st, nil
}

// Install writes the embedded skill into dir and stamps the marker. A fresh
// directory is installed, a managed one is refreshed (its previous files are
// removed first, anything else in the directory is left alone), and an
// unmanaged one is refused unless force, which replaces the whole directory.
func Install(dir, version string, force bool) (InstallResult, error) {
	res := InstallResult{Skill: Name, Dir: dir, Version: version}
	st, err := Inspect(dir, version)
	if err != nil {
		return res, err
	}

	switch {
	case !st.Installed:
		res.Action = "installed"
	case st.Managed && !force:
		res.Action = "refreshed"
		if err := removeManagedFiles(dir); err != nil {
			return res, err
		}
	case force:
		res.Action = "replaced"
		if err := os.RemoveAll(dir); err != nil {
			return res, err
		}
	default:
		return res, fmt.Errorf("%w: %s (run `altscore skill install --force` to replace it)", ErrUnmanaged, dir)
	}

	files, err := Files()
	if err != nil {
		return res, err
	}
	for _, rel := range files {
		data, err := ReadFile(rel)
		if err != nil {
			return res, err
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return res, err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return res, err
		}
	}
	res.FileCount = len(files)

	body, err := marshalMarker(Marker{Version: version, InstalledAt: time.Now().UTC(), Files: files})
	if err != nil {
		return res, err
	}
	if err := os.WriteFile(filepath.Join(dir, MarkerFile), body, 0o644); err != nil {
		return res, err
	}
	return res, nil
}

func marshalMarker(m Marker) ([]byte, error) {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// RefreshIfManaged re-installs when dir is a managed install of a different
// version. It never touches an unmanaged directory, never installs where
// nothing was installed, and never writes a "dev" build over a release.
func RefreshIfManaged(dir, version string) (*InstallResult, error) {
	if version == "dev" {
		return nil, nil
	}
	st, err := Inspect(dir, version)
	if err != nil {
		return nil, err
	}
	if !st.Managed || st.UpToDate {
		return nil, nil
	}
	res, err := Install(dir, version, false)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func readMarker(dir string) (Marker, error) {
	var m Marker
	data, err := os.ReadFile(filepath.Join(dir, MarkerFile))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if m.Version == "" {
		return m, errors.New("marker has no version")
	}
	return m, nil
}

// removeManagedFiles deletes the files a previous Install recorded, plus the
// directories that became empty, without touching anything else in dir.
func removeManagedFiles(dir string) error {
	m, err := readMarker(dir)
	if err != nil {
		return err
	}
	for _, rel := range m.Files {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		p := filepath.Join(dir, clean)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		// Prune now-empty parents up to dir.
		for parent := filepath.Dir(p); parent != dir && strings.HasPrefix(parent, dir); parent = filepath.Dir(parent) {
			if err := os.Remove(parent); err != nil {
				break
			}
		}
	}
	return os.Remove(filepath.Join(dir, MarkerFile))
}
