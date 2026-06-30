package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/AltScore/altscore-cli/internal/config"
)

func TestPrintUpdateNotice(t *testing.T) {
	var buf bytes.Buffer
	printUpdateNotice(&buf, "v0.11.0", "v0.12.0")
	out := buf.String()

	if !strings.Contains(out, "v0.11.0") || !strings.Contains(out, "v0.12.0") {
		t.Errorf("notice missing version strings:\n%s", out)
	}
	if !strings.Contains(out, "altscore update") {
		t.Errorf("notice missing upgrade hint:\n%s", out)
	}
	if !strings.Contains(out, "\x1b[1;91m") {
		t.Errorf("notice missing bold bright-red ANSI code:\n%q", out)
	}
	if !strings.Contains(out, "╭") || !strings.Contains(out, "╰") {
		t.Errorf("notice missing box borders:\n%s", out)
	}

	// All box rows must be the same visual width (rune count, ignoring ANSI).
	var widths []int
	for _, line := range strings.Split(out, "\n") {
		line = stripANSI(line)
		if strings.HasPrefix(line, "╭") || strings.HasPrefix(line, "│") || strings.HasPrefix(line, "╰") {
			widths = append(widths, len([]rune(line)))
		}
	}
	if len(widths) != 4 {
		t.Fatalf("expected 4 box rows, got %d", len(widths))
	}
	for _, w := range widths {
		if w != widths[0] {
			t.Errorf("box rows misaligned: %v", widths)
			break
		}
	}
}

func stripANSI(s string) string {
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			return s
		}
		j := strings.IndexByte(s[i:], 'm')
		if j < 0 {
			return s
		}
		s = s[:i] + s[i+j+1:]
	}
}

func TestUpdateStateRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // config dir derives from $HOME

	if _, err := loadUpdateState(); err == nil {
		t.Error("expected error loading a non-existent state file")
	}

	now := time.Now().Truncate(time.Second)
	want := updateState{LastChecked: now, LatestVersion: "v0.12.0"}
	if err := saveUpdateState(want); err != nil {
		t.Fatalf("saveUpdateState: %v", err)
	}

	got, err := loadUpdateState()
	if err != nil {
		t.Fatalf("loadUpdateState: %v", err)
	}
	if got.LatestVersion != want.LatestVersion || !got.LastChecked.Equal(want.LastChecked) {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}

	// State must live alongside config, not in the credentials file.
	if p, _ := config.StateFilePath(updateStateFile); !strings.HasSuffix(p, "update_check.json") {
		t.Errorf("unexpected state path %q", p)
	}
}

func TestTopLevelCommandName(t *testing.T) {
	// borrowers list -> "borrowers"; update -> "update".
	if got := topLevelCommandName(updateCmd); got != "update" {
		t.Errorf("topLevelCommandName(update) = %q, want \"update\"", got)
	}
	// Find the borrowers group and a leaf under it.
	for _, c := range rootCmd.Commands() {
		if c.Name() == "borrowers" {
			for _, sub := range c.Commands() {
				if got := topLevelCommandName(sub); got != "borrowers" {
					t.Errorf("topLevelCommandName(borrowers %s) = %q, want \"borrowers\"", sub.Name(), got)
				}
			}
		}
	}
}

func TestAcquireCheckLockSingleFlight(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	release, ok := acquireCheckLock()
	if !ok {
		t.Fatal("first lock acquisition should succeed")
	}
	if _, ok2 := acquireCheckLock(); ok2 {
		t.Error("second concurrent lock acquisition should fail")
	}
	release()
	if _, ok3 := acquireCheckLock(); !ok3 {
		t.Error("lock should be re-acquirable after release")
	}
}
