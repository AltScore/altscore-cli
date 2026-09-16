package skill

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesEmbedTheSkillEntryPointAndReferences(t *testing.T) {
	files, err := Files()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Fatalf("expected SKILL.md plus references, got %v", files)
	}
	if files[0] != "SKILL.md" {
		t.Errorf("SKILL.md must sort first, got %v", files)
	}
	refs := 0
	for _, f := range files {
		if strings.HasPrefix(f, "references/") {
			refs++
		}
	}
	if refs == 0 {
		t.Errorf("no references/ files embedded: %v", files)
	}
	body, err := ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "name: "+Name) {
		t.Errorf("SKILL.md frontmatter must declare name: %s", Name)
	}
}

func TestInstallIntoFreshDirWritesFilesAndMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), Name)
	res, err := Install(dir, "v1.2.3", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "installed" {
		t.Errorf("action = %q, want installed", res.Action)
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md not written: %v", err)
	}
	st, err := Inspect(dir, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || !st.Managed || !st.UpToDate || st.InstalledVersion != "v1.2.3" {
		t.Errorf("status after install = %+v", st)
	}
	if st.FileCount != res.FileCount {
		t.Errorf("fileCount %d != %d", st.FileCount, res.FileCount)
	}
}

func TestInstallRefusesAnUnmanagedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("hand copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Install(dir, "v1.2.3", false)
	if !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("expected ErrUnmanaged, got %v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error must point at --force: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if string(got) != "hand copy" {
		t.Errorf("hand copy was overwritten")
	}
	st, _ := Inspect(dir, "v1.2.3")
	if !st.Installed || st.Managed {
		t.Errorf("status = %+v, want installed and unmanaged", st)
	}
}

func TestInstallForceReplacesAnUnmanagedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Install(dir, "v1.2.3", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "replaced" {
		t.Errorf("action = %q, want replaced", res.Action)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("force must clear the old directory, stale.md survived")
	}
}

func TestRefreshOfAManagedInstallRemovesItsOldFilesAndKeepsForeignOnes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), Name)
	if _, err := Install(dir, "v1.0.0", false); err != nil {
		t.Fatal(err)
	}
	// Simulate a file the previous release shipped and this one does not.
	old := filepath.Join(dir, "references", "retired.md")
	if err := os.WriteFile(old, []byte("gone next release"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := readMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Files = append(m.Files, "references/retired.md")
	if err := writeMarkerForTest(dir, m); err != nil {
		t.Fatal(err)
	}
	// And a file the user added themselves, which the marker does not list.
	foreign := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(foreign, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := RefreshIfManaged(dir, "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.Action != "refreshed" {
		t.Fatalf("expected a refresh, got %+v", res)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("retired managed file survived the refresh")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign file was removed: %v", err)
	}
	st, _ := Inspect(dir, "v1.1.0")
	if !st.UpToDate || st.InstalledVersion != "v1.1.0" {
		t.Errorf("status after refresh = %+v", st)
	}
}

func TestRefreshIfManagedIsANoopWhenCurrentUnmanagedAbsentOrDev(t *testing.T) {
	base := t.TempDir()

	current := filepath.Join(base, "current")
	if _, err := Install(current, "v2.0.0", false); err != nil {
		t.Fatal(err)
	}
	if res, err := RefreshIfManaged(current, "v2.0.0"); err != nil || res != nil {
		t.Errorf("up-to-date install must not refresh: res=%+v err=%v", res, err)
	}
	if res, err := RefreshIfManaged(current, "dev"); err != nil || res != nil {
		t.Errorf("a dev build must never overwrite a release install: res=%+v err=%v", res, err)
	}

	unmanaged := filepath.Join(base, "unmanaged")
	if err := os.MkdirAll(unmanaged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unmanaged, "SKILL.md"), []byte("hand copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := RefreshIfManaged(unmanaged, "v2.0.0"); err != nil || res != nil {
		t.Errorf("unmanaged dir must be left alone: res=%+v err=%v", res, err)
	}

	absent := filepath.Join(base, "absent")
	if res, err := RefreshIfManaged(absent, "v2.0.0"); err != nil || res != nil {
		t.Errorf("nothing installed means nothing to refresh: res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("refresh must not create a directory")
	}
}

func TestDefaultDirHonoursClaudeConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/claude-cfg")
	dir, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/tmp/claude-cfg", "skills", Name)
	if dir != want {
		t.Errorf("DefaultDir = %q, want %q", dir, want)
	}
}

func writeMarkerForTest(dir string, m Marker) error {
	body, err := marshalMarker(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MarkerFile), body, 0o644)
}
