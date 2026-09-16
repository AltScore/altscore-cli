package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/AltScore/altscore-cli/internal/output"
	"github.com/AltScore/altscore-cli/internal/skill"
	"github.com/AltScore/altscore-cli/internal/version"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// makeSkillCmd groups the commands that manage the Claude Code skill shipped
// inside this binary. The skill is what teaches an agent to drive the CLI; it
// rides with the release so `altscore update` refreshes both together.
func makeSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Install the Claude Code skill that teaches agents this CLI",
		Long: `The altscore binary embeds the Claude Code skill "` + skill.Name + `" (SKILL.md plus
its references). 'skill install' writes it to the user's skills directory so it
loads in every session regardless of the working directory, and stamps a marker
so later updates can refresh it. A directory without the marker is treated as a
hand copy and is never overwritten without --force.

Default location: $CLAUDE_CONFIG_DIR/skills/` + skill.Name + ` or ~/.claude/skills/` + skill.Name + `.
A managed install is refreshed automatically by 'altscore update' and by the
first command run after a new version lands.`,
		Example: `  altscore skill install
  altscore skill install --force            # replace a hand copy
  altscore skill install --dir ./.claude/skills/` + skill.Name + `
  altscore skill status`,
	}
	cmd.AddCommand(makeSkillInstallCmd())
	cmd.AddCommand(makeSkillStatusCmd())
	return cmd
}

func makeSkillInstallCmd() *cobra.Command {
	var dir string
	var force, ifManaged bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write the embedded skill to the Claude Code skills directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveSkillDir(dir)
			if err != nil {
				return err
			}
			if ifManaged {
				res, err := skill.RefreshIfManaged(target, version.Version)
				if err != nil {
					return err
				}
				if res == nil {
					st, err := skill.Inspect(target, version.Version)
					if err != nil {
						return err
					}
					return output.JSON(skillSkipped(st))
				}
				return output.JSON(res)
			}
			res, err := skill.Install(target, version.Version, force)
			if err != nil {
				return err
			}
			if term.IsTerminal(int(os.Stderr.Fd())) {
				fmt.Fprintf(os.Stderr, "Skill %q %s at %s (%d files). Start a new Claude Code session to load it.\n",
					res.Skill, res.Action, res.Dir, res.FileCount)
			}
			return output.JSON(res)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "target directory (default: the user's Claude Code skills directory)")
	cmd.Flags().BoolVar(&force, "force", false, "replace a directory that exists but was not written by altscore")
	cmd.Flags().BoolVar(&ifManaged, "if-managed", false, "refresh only an existing managed install; otherwise do nothing")
	_ = cmd.Flags().MarkHidden("if-managed")
	return cmd
}

func makeSkillStatusCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether the skill is installed, managed and current",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveSkillDir(dir)
			if err != nil {
				return err
			}
			st, err := skill.Inspect(target, version.Version)
			if err != nil {
				return err
			}
			return output.JSON(st)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to inspect (default: the user's Claude Code skills directory)")
	return cmd
}

func resolveSkillDir(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return skill.DefaultDir()
}

// skillSkipped is what `skill install --if-managed` prints when there was
// nothing to refresh, so callers can tell the four states apart.
func skillSkipped(st skill.Status) map[string]any {
	reason := "up to date"
	switch {
	case !st.Installed:
		reason = "not installed"
	case !st.Managed:
		reason = "not managed"
	case version.Version == "dev":
		reason = "dev build"
	}
	return map[string]any{
		"skill":  st.Skill,
		"dir":    st.Dir,
		"action": "skipped",
		"reason": reason,
	}
}

// refreshSkillAfterUpdate asks the binary that was just installed to refresh
// a managed skill. The running process still embeds the OLD skill, so it
// cannot do this itself. Best-effort: every failure is one status line.
func refreshSkillAfterUpdate(status io.Writer) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	c := exec.Command(exe, "skill", "install", "--if-managed")
	c.Env = append(os.Environ(), "ALTSCORE_NO_UPDATE_CHECK=1")
	out, err := c.Output()
	if err != nil {
		fmt.Fprintln(status, "Skill refresh skipped (run `altscore skill status` to inspect).")
		return
	}
	var res struct {
		Dir     string `json:"dir"`
		Action  string `json:"action"`
		Reason  string `json:"reason"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return
	}
	switch {
	case res.Action == "skipped" && res.Reason == "not installed":
		fmt.Fprintf(status, "Tip: `altscore skill install` puts the Claude Code skill in %s.\n", res.Dir)
	case res.Action == "skipped" && res.Reason == "not managed":
		fmt.Fprintf(status, "Skill at %s is a hand copy and was left alone; `altscore skill install --force` replaces it.\n", res.Dir)
	case res.Action == "skipped":
		// Up to date or a dev build: nothing to say.
	default:
		fmt.Fprintf(status, "Skill refreshed to %s in %s.\n", res.Version, res.Dir)
	}
}

// skillRefreshHook keeps a managed install in step with the running binary.
// It runs after every command (root PersistentPostRunE), does nothing for dev
// builds, unmanaged directories or a current install, and never fails the
// command it follows.
func skillRefreshHook(cmd *cobra.Command) {
	if version.Version == "dev" {
		return
	}
	switch topLevelCommandName(cmd) {
	case "skill", "update", "__update-check", "version", "completion", "help", "altscore":
		return
	}
	dir, err := skill.DefaultDir()
	if err != nil {
		return
	}
	res, err := skill.RefreshIfManaged(dir, version.Version)
	if err != nil || res == nil {
		return
	}
	if term.IsTerminal(int(os.Stderr.Fd())) {
		fmt.Fprintf(os.Stderr, "# Claude Code skill refreshed to %s (%s)\n", res.Version, res.Dir)
	}
}
