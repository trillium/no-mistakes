package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

var (
	daemonRun         = daemon.Run
	daemonStartFn     = daemon.Start
	daemonStopFn      = daemon.Stop
	daemonIsRunningFn = daemon.IsRunning
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the no-mistakes daemon",
	}

	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonRestartCmd())
	cmd.AddCommand(newDaemonStatusCmd())
	cmd.AddCommand(newDaemonRunCmd())
	cmd.AddCommand(newDaemonAdmitPushCmd())
	cmd.AddCommand(newDaemonNotifyPushCmd())

	return cmd
}

func newDaemonAdmitPushCmd() *cobra.Command {
	var gate string
	cmd := &cobra.Command{
		Use:    "admit-push",
		Short:  "Authorize a managed gate ref update",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}
			p, err := paths.New()
			if err != nil {
				return err
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()
			var result ipc.AdmitPushResult
			if err := client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: gatePath}, &result); err != nil {
				return err
			}
			if !result.Context.Nested {
				return nil
			}
			return emitGateContextRefusal(cmd, gatecontext.Result{
				Nested:           result.Context.Nested,
				ManagedGit:       result.Context.ManagedGit,
				AgentDescendant:  result.Context.AgentDescendant,
				DaemonDescendant: result.Context.DaemonDescendant,
				MarkerPresent:    result.Context.MarkerPresent,
				RunID:            result.Context.RunID,
				Phase:            result.Context.Phase,
			})
		},
	}
	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that is about to receive a push")
	_ = cmd.MarkFlagRequired("gate")
	return cmd
}

// notifyPushCallTimeout bounds how long the post-receive hook blocks the
// pushing client while the daemon creates the run. The daemon answers only
// after real work - a fetch of the trusted default branch, worktree creation,
// and agent resolution - which was measured at ~40s on a cold repo, so the 30s
// default call timeout expired mid-flight on healthy pushes (robots-8bao). It
// is deliberately not unbounded: past this point the advisory below is a better
// answer than a longer silent stall. Overridable for tests.
var notifyPushCallTimeout = 60 * time.Second

// unconfirmedNotifyPushAdvisory is the entire output of a successful-but-
// unconfirmed notification. The hook treats any output as the unconfirmed
// signal, so it must stand alone in place of the "Pipeline started" banner and
// name both the check and the recovery command - neither of which appeared
// anywhere in the old failure text.
//
// It claims exactly one thing: the push landed. That much is certain, because
// post-receive runs after the ref is already stored. Whether the daemon got far
// enough to create the run is genuinely unknown here - the deadline can expire
// at any point in its handler - so the advisory says so and makes `axi status`
// the deciding check rather than asserting an outcome it cannot see.
func unconfirmedNotifyPushAdvisory(ref string, waited time.Duration) string {
	return fmt.Sprintf(`no-mistakes: %s was pushed and the daemon accepted the notification,
but it did not confirm the run within %s. The push itself succeeded - the ref is
stored on the gate - so it does not need to be repeated. Whether the run was
created is unconfirmed; check before assuming either way.

  check:   no-mistakes axi status
  recover: no-mistakes axi run   (only if no run appears)

`, ref, waited)
}

func newDaemonNotifyPushCmd() *cobra.Command {
	var gate string
	var ref string
	var oldSHA string
	var newSHA string
	var pushOptions []string

	cmd := &cobra.Command{
		Use:    "notify-push",
		Short:  "Notify daemon about a git push",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			skipSteps, err := parseSkipPushOptions(pushOptions)
			if err != nil {
				return err
			}
			intent, err := parseIntentPushOptions(pushOptions)
			if err != nil {
				return err
			}
			agentOverride := parseAgentPushOptions(pushOptions)
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}

			p, err := paths.New()
			if err != nil {
				return err
			}

			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			var result ipc.PushReceivedResult
			// The context is threaded through rather than using
			// CallWithTimeout, which supplies context.Background(): this is a
			// 60-second block on the pushing client, and an interrupt during it
			// must stay a failure rather than become a successful unconfirmed
			// notification.
			err = client.CallWithContext(cmd.Context(), ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate:      gatePath,
				Ref:       ref,
				Old:       oldSHA,
				New:       newSHA,
				SkipSteps: skipSteps,
				Intent:    intent,
				Agent:     agentOverride,
			}, &result, notifyPushCallTimeout)
			if err != nil {
				// The request was written and the daemon simply outran its
				// reply: the ref is stored on the gate and run creation is the
				// daemon's, not ours. Reporting exit 1 here made a healthy push
				// read as a failed one, so an agent retried the push or
				// abandoned the PR while the run was already queued
				// (robots-8bao). Every other failure - no daemon, a closed
				// connection, an error the daemon itself reported - is evidence
				// the run does not exist, and still fails.
				if ipc.IsResponseTimeout(err) {
					fmt.Fprint(cmd.OutOrStdout(), unconfirmedNotifyPushAdvisory(ref, notifyPushCallTimeout))
					return nil
				}
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that received the push")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "previous commit SHA")
	cmd.Flags().StringVar(&newSHA, "new", "", "new commit SHA")
	cmd.Flags().StringArrayVar(&pushOptions, "push-option", nil, "git push option")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("old")
	_ = cmd.MarkFlagRequired("new")

	return cmd
}

func normalizeNotifyGatePath(gate string) (string, error) {
	if strings.TrimSpace(gate) == "" {
		return "", fmt.Errorf("gate path is required")
	}
	abs, err := filepath.Abs(gate)
	if err != nil {
		return "", fmt.Errorf("resolve gate path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func parseSkipPushOptions(options []string) ([]types.StepName, error) {
	var steps []types.StepName
	for _, option := range options {
		value, ok := strings.CutPrefix(option, "no-mistakes.skip=")
		if !ok {
			continue
		}
		parsed, err := parseSkipSteps(value)
		if err != nil {
			return nil, err
		}
		steps = append(steps, parsed...)
	}
	return dedupeSteps(steps), nil
}

func parseSkipSteps(value string) ([]types.StepName, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var steps []types.StepName
	for _, part := range strings.Split(value, ",") {
		step := types.StepName(strings.TrimSpace(part))
		if !validStep(step) {
			return nil, fmt.Errorf("unknown step %q", step)
		}
		steps = append(steps, step)
	}
	return dedupeSteps(steps), nil
}

// intentPushOptionPrefix carries an agent-supplied intent through a git push.
// The value is base64-encoded so multi-line or special-character intents
// survive the push-option transport (which is line-oriented).
const intentPushOptionPrefix = "no-mistakes.intent="

// formatIntentPushOption encodes intent as a single push option, or returns ""
// when there is no intent to carry.
func formatIntentPushOption(intent string) string {
	if strings.TrimSpace(intent) == "" {
		return ""
	}
	return intentPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(intent))
}

// parseIntentPushOptions extracts and decodes the intent push option, if any.
// The last occurrence wins.
func parseIntentPushOptions(options []string) (string, error) {
	intent := ""
	for _, option := range options {
		encoded, ok := strings.CutPrefix(option, intentPushOptionPrefix)
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("decode intent push option: %w", err)
		}
		intent = string(decoded)
	}
	return intent, nil
}

// agentPushOptionPrefix carries a per-run agent-selector override through a git
// push, so `axi run --agent <name>` reaches the daemon on the push-trigger path
// exactly like the intent option does. Agent selectors are constrained tokens
// (auto, native names, acp:<target>), so no encoding is needed.
const agentPushOptionPrefix = "no-mistakes.agent="

// formatAgentPushOption encodes an agent-selector override as a single push
// option, or returns "" when there is no override to carry.
func formatAgentPushOption(agent string) string {
	trimmed := strings.TrimSpace(agent)
	if trimmed == "" {
		return ""
	}
	return agentPushOptionPrefix + trimmed
}

// parseAgentPushOptions extracts the agent-selector override, if any. The last
// occurrence wins, mirroring parseIntentPushOptions.
func parseAgentPushOptions(options []string) string {
	agent := ""
	for _, option := range options {
		if value, ok := strings.CutPrefix(option, agentPushOptionPrefix); ok {
			agent = strings.TrimSpace(value)
		}
	}
	return agent
}

func formatSkipPushOptions(steps []types.StepName) []string {
	if len(steps) == 0 {
		return nil
	}
	parts := make([]string, 0, len(steps))
	for _, step := range dedupeSteps(steps) {
		parts = append(parts, string(step))
	}
	return []string{"no-mistakes.skip=" + strings.Join(parts, ",")}
}

func validStep(step types.StepName) bool {
	for _, known := range types.AllSteps() {
		if step == known {
			return true
		}
	}
	return false
}

func dedupeSteps(steps []types.StepName) []types.StepName {
	seen := make(map[types.StepName]bool, len(steps))
	out := make([]types.StepName, 0, len(steps))
	for _, step := range steps {
		if seen[step] {
			continue
		}
		seen[step] = true
		out = append(out, step)
	}
	return out
}

func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Install or refresh the managed daemon service and start it",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.start", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := daemonStartFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon started\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.stop", force)
			return trackCommand("daemon.stop", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon stop", force); err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon stopped\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop the daemon even when pipeline runs are active")
	return cmd
}

func newDaemonRestartCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon (stop if running, then start)",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.restart", force)
			return trackCommand("daemon.restart", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon restart", force); err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return fmt.Errorf("stop daemon: %w", err)
				}
				if err := daemonStartFn(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon restarted\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restart the daemon even when pipeline runs are active")
	return cmd
}

func guardDestructiveDaemonLifecycle(p *paths.Paths, stderr io.Writer, action string, force bool) error {
	runs, err := lifecycle.ActiveRuns(p)
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}
	if len(runs) == 0 {
		return nil
	}
	if force {
		fmt.Fprintf(stderr, "FORCE: %s will stop/restart the daemon while %d active pipeline runs are in progress\n", action, len(runs))
		fmt.Fprint(stderr, lifecycle.RunList(runs))
		return nil
	}
	return fmt.Errorf("refusing %s because %d active pipeline runs are in progress; pass --force to stop/restart the daemon anyway\n%s", action, len(runs), lifecycle.RunList(runs))
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.status", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				alive, err := daemonIsRunningFn(p)
				if err != nil {
					return err
				}
				if alive {
					pid, _ := daemon.ReadPID(p)
					if pid > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running %s\n", sGreen.Render("●"), sDim.Render(fmt.Sprintf("(pid %d)", pid)))
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running\n", sGreen.Render("●"))
					}
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon not running\n", sDim.Render("○"))
				}
				return nil
			})
		},
	}
}

func newDaemonRunCmd() *cobra.Command {
	var root string

	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Run the daemon in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if root != "" {
				if err := os.Setenv("NM_HOME", root); err != nil {
					return fmt.Errorf("set NM_HOME: %w", err)
				}
			}
			return daemonRun()
		},
	}

	cmd.Flags().StringVar(&root, "root", "", "override no-mistakes data directory")
	return cmd
}
