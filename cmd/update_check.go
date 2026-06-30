package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/AltScore/altscore-cli/internal/config"
	"github.com/AltScore/altscore-cli/internal/version"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const (
	// updateCheckInterval throttles how often we hit GitHub. The foreground
	// command never blocks on the check; this only bounds the background work.
	updateCheckInterval = 24 * time.Hour
	updateStateFile     = "update_check.json"
	updateLockFile      = "update_check.lock"
)

// updateState is the cached result of the last background release check.
type updateState struct {
	LastChecked   time.Time `json:"last_checked"`
	LatestVersion string    `json:"latest_version"`
}

func init() {
	rootCmd.AddCommand(updateCheckCmd)

	// Fire after every successful command. No subcommand defines its own
	// PersistentPostRun, so this root hook runs for the whole CLI. It is
	// strictly best-effort: it never delays the command (the GitHub call
	// happens in a detached child) and never returns an error.
	rootCmd.PersistentPostRunE = func(cmd *cobra.Command, args []string) error {
		updateCheckHook(cmd)
		return nil
	}
}

// updateCheckCmd is the hidden entry point the background process runs. It
// refreshes the cached latest-release info and, when the foreground asked for
// it, self-updates the binary.
var updateCheckCmd = &cobra.Command{
	Use:    "__update-check",
	Hidden: true,
	Short:  "Internal: refresh cached latest-release info in the background",
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		runBackgroundCheck()
		return nil
	},
}

// updateCheckHook notifies based on the previous check and, if stale, kicks off
// a new background check. Called from the root PersistentPostRunE.
func updateCheckHook(cmd *cobra.Command) {
	if shouldSkipUpdateCheck(cmd) {
		return
	}
	// Auto-update users opted into a silent experience: no notice, the
	// background process applies the update for the next invocation.
	if !autoUpdateEnabled() {
		maybeNotify()
	}
	scheduleBackgroundCheck()
}

// shouldSkipUpdateCheck disables the feature for dev builds, an explicit
// opt-out, and the commands where a notice would be noise or recursive.
func shouldSkipUpdateCheck(cmd *cobra.Command) bool {
	if version.Version == "dev" {
		return true
	}
	if os.Getenv("ALTSCORE_NO_UPDATE_CHECK") != "" {
		return true
	}
	switch topLevelCommandName(cmd) {
	case "update", "version", "completion", "help", "__update-check", "altscore":
		return true
	}
	return false
}

// topLevelCommandName returns the name of the command directly under root
// (e.g. "borrowers" for `altscore borrowers list`), or the root's own name
// when invoked bare.
func topLevelCommandName(cmd *cobra.Command) string {
	c := cmd
	for c.HasParent() && c.Parent().HasParent() {
		c = c.Parent()
	}
	return c.Name()
}

// maybeNotify prints a bright-red "update available" notice when the cached
// latest release is newer than the running build. Shown only on an interactive
// terminal so piped output and CI logs stay clean.
func maybeNotify() {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	st, err := loadUpdateState()
	if err != nil || st.LatestVersion == "" {
		return
	}
	if version.Compare(st.LatestVersion, version.Version) > 0 {
		printUpdateNotice(os.Stderr, version.Version, st.LatestVersion)
	}
}

// printUpdateNotice draws a bold bright-red box. The caller has already
// confirmed the writer is a TTY, so ANSI color is always safe here.
func printUpdateNotice(w io.Writer, current, latest string) {
	const (
		red   = "\x1b[1;91m"
		reset = "\x1b[0m"
	)
	msgs := []string{
		fmt.Sprintf("  Update available: %s → %s  ", current, latest),
		"  Run  altscore update  to upgrade.  ",
	}
	width := 0
	for _, m := range msgs {
		if l := len([]rune(m)); l > width {
			width = l
		}
	}
	bar := strings.Repeat("─", width)
	fmt.Fprintf(w, "\n%s╭%s╮%s\n", red, bar, reset)
	for _, m := range msgs {
		pad := strings.Repeat(" ", width-len([]rune(m)))
		fmt.Fprintf(w, "%s│%s%s│%s\n", red, m, pad, reset)
	}
	fmt.Fprintf(w, "%s╰%s╯%s\n\n", red, bar, reset)
}

// scheduleBackgroundCheck spawns a detached child to refresh the cache when the
// last check is older than the throttle interval. It returns immediately; the
// child outlives this process.
func scheduleBackgroundCheck() {
	st, _ := loadUpdateState()
	if !st.LastChecked.IsZero() && time.Since(st.LastChecked) < updateCheckInterval {
		return
	}

	// Optimistically stamp now (preserving the known latest version) so a burst
	// of commands doesn't spawn a burst of children before the first finishes.
	st.LastChecked = time.Now()
	_ = saveUpdateState(st)

	exe, err := os.Executable()
	if err != nil {
		return
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer devnull.Close()

	c := exec.Command(exe, "__update-check")
	c.Stdin, c.Stdout, c.Stderr = devnull, devnull, devnull
	env := append(os.Environ(), "ALTSCORE_NO_UPDATE_CHECK=1")
	// Decide auto-update here, in the foreground, where TTY/CI reflect the real
	// session; the detached child's stdio is /dev/null and can't tell.
	if autoUpdateEnabled() && autoUpdateAllowed() {
		env = append(env, "ALTSCORE_DO_AUTO_UPDATE=1")
	}
	c.Env = env
	_ = c.Start() // not Wait()ed: the child continues after we exit.
}

// runBackgroundCheck refreshes the cache and self-updates if requested. Runs in
// the detached child; all failures are swallowed.
func runBackgroundCheck() {
	release, ok := acquireCheckLock()
	if !ok {
		return
	}
	defer release()

	rel, err := fetchLatestRelease()
	if err != nil {
		return
	}
	_ = saveUpdateState(updateState{LastChecked: time.Now(), LatestVersion: rel.TagName})

	if os.Getenv("ALTSCORE_DO_AUTO_UPDATE") == "1" && version.Compare(rel.TagName, version.Version) > 0 {
		_ = performUpdate(rel, nil)
	}
}

// autoUpdateEnabled reports whether the user opted into silent self-update,
// via ALTSCORE_AUTO_UPDATE or auto_update in [defaults].
func autoUpdateEnabled() bool {
	if v := os.Getenv("ALTSCORE_AUTO_UPDATE"); v != "" {
		return v == "1" || strings.EqualFold(v, "true")
	}
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	return cfg.Defaults.AutoUpdate
}

// autoUpdateAllowed gates the actual binary replacement off for CI and
// non-interactive shells, where a surprise binary swap is unwelcome.
func autoUpdateAllowed() bool {
	if os.Getenv("CI") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// acquireCheckLock provides single-flight protection so concurrent invocations
// don't all hit GitHub at once. A lock older than an hour is treated as stale.
func acquireCheckLock() (release func(), ok bool) {
	p, err := config.StateFilePath(updateLockFile)
	if err != nil {
		return nil, false
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if info, statErr := os.Stat(p); statErr == nil && time.Since(info.ModTime()) > time.Hour {
			_ = os.Remove(p)
			f, err = os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		}
		if err != nil {
			return nil, false
		}
	}
	_ = f.Close()
	return func() { _ = os.Remove(p) }, true
}

func loadUpdateState() (updateState, error) {
	var st updateState
	p, err := config.StateFilePath(updateStateFile)
	if err != nil {
		return st, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}

func saveUpdateState(st updateState) error {
	p, err := config.StateFilePath(updateStateFile)
	if err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
