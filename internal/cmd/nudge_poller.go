package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	nudgePollerIntervalFlag string
	nudgePollerIdleFlag     string
)

const nudgePollerStopInterval = 100 * time.Millisecond

func init() {
	rootCmd.AddCommand(nudgePollerCmd)
	nudgePollerCmd.Flags().StringVar(&nudgePollerIntervalFlag, "interval", nudge.DefaultPollInterval, "Poll interval (e.g., 10s, 30s)")
	nudgePollerCmd.Flags().StringVar(&nudgePollerIdleFlag, "idle-timeout", nudge.DefaultIdleTimeout, "How long to wait for agent idle before skipping")
}

var nudgePollerCmd = &cobra.Command{
	Use:    "nudge-poller <session>",
	Short:  "Background nudge queue poller for non-Claude agents",
	Hidden: true, // Internal command — launched by crew manager, not by users.
	Long: `Polls the nudge queue for a tmux session and drains it when the agent
is idle. This is the background equivalent of Claude's UserPromptSubmit hook
drain — it ensures queued nudges are delivered to agents that lack
turn-boundary hooks (Gemini, Codex, Cursor, etc.).

This command runs as a long-lived background process. It exits when:
  - The target tmux session dies
  - Its generation-bound cooperative stop request is published
  - It receives SIGTERM or SIGINT from external process teardown
  - The poll loop encounters an unrecoverable error

Normally launched automatically by 'gt crew start' for non-Claude agents.
Not intended for direct user invocation.`,
	Args: cobra.ExactArgs(1),
	RunE: runNudgePoller,
}

func runNudgePoller(cmd *cobra.Command, args []string) error {
	sessionName := args[0]

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("cannot find town root: %w", err)
	}

	pollInterval, err := time.ParseDuration(nudgePollerIntervalFlag)
	if err != nil {
		return fmt.Errorf("invalid --interval: %w", err)
	}

	idleTimeout, err := time.ParseDuration(nudgePollerIdleFlag)
	if err != nil {
		return fmt.Errorf("invalid --idle-timeout: %w", err)
	}

	t := tmux.NewTmux()

	// Verify session exists before starting the loop.
	if exists, err := pollerHasSession(t, sessionName); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("session %q not found", sessionName)
	}

	// Resolve nudge options once at startup: if the target agent uses Escape
	// as cancel (e.g., Gemini CLI), skip the Escape keystroke during delivery
	// to avoid canceling in-flight generation. (GH#gt-wasn)
	hasPromptDetection, nudgeOpts := resolvePollerSessionMetadata(t, sessionName)

	// Set up signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	stopContext, cancelStopWatcher := context.WithCancel(cmd.Context())
	defer cancelStopWatcher()
	cooperativeStop := watchCooperativePollerStop(stopContext, nudgePollerStopInterval, func() bool {
		return nudge.StopRequested(townRoot, sessionName)
	})

	for {
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-sigCh:
			return nil // graceful shutdown
		case <-cooperativeStop:
			return nil

		case <-ticker.C:
			// Check if session still exists.
			if exists, err := pollerHasSession(t, sessionName); err != nil {
				fmt.Fprintf(os.Stderr, "nudge-poller: session check error for %s: %v\n", sessionName, err)
				continue
			} else if !exists {
				return nil // session gone, exit
			}

			// Check if there are queued nudges.
			if n, _ := nudge.Pending(townRoot, sessionName); n == 0 {
				continue
			}

			// For runtimes with prompt detection, defer delivery until the session
			// is actually idle. Runtimes without prompt detection preserve the old
			// best-effort behavior and drain on the poll interval.
			claim, err := claimPollerNudgeWhenIdleContext(
				stopContext, cooperativeStop,
				hasPromptDetection,
				func() error { return t.WaitForIdle(sessionName, idleTimeout) },
				func() (*nudge.ClaimedNudge, error) { return nudge.ClaimDue(townRoot, sessionName) },
			)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				fmt.Fprintf(os.Stderr, "nudge-poller: claim error for %s: %v\n", sessionName, err)
				continue
			}
			if claim == nil {
				continue // someone else drained it
			}

			formatted := nudge.FormatForInjection([]nudge.QueuedNudge{claim.Nudge})
			nudgeOpts.TownRoot = townRoot
			nudgeOpts.DeliveryID = claim.Nudge.DeliveryID
			receipt, err := t.NudgeSessionWithReceipt(sessionName, formatted, nudgeOpts)
			if err != nil || !receipt.Submitted {
				fmt.Fprintf(os.Stderr, "nudge-poller: injection error for %s: %v\n", sessionName, err)
				if nackErr := claim.Nack("submit-unverified", nudge.NextRetry(claim.Nudge.Attempts)); nackErr != nil {
					fmt.Fprintf(os.Stderr, "nudge-poller: nack error for %s: %v\n", sessionName, nackErr)
				}
			} else if ackErr := claim.AckSubmitted(receipt); ackErr != nil {
				fmt.Fprintf(os.Stderr, "nudge-poller: ack error for %s: %v\n", sessionName, ackErr)
				if nackErr := claim.Nack("receipt-mismatch", nudge.NextRetry(claim.Nudge.Attempts)); nackErr != nil {
					fmt.Fprintf(os.Stderr, "nudge-poller: nack error for %s: %v\n", sessionName, nackErr)
				}
			}
		}
	}
}

func claimPollerNudgeWhenIdleContext(ctx context.Context, stop <-chan struct{}, hasPromptDetection bool, waitForIdle func() error, claimDue func() (*nudge.ClaimedNudge, error)) (*nudge.ClaimedNudge, error) {
	if !hasPromptDetection {
		return claimDue()
	}
	result := make(chan error, 1)
	go func() { result <- waitForIdle() }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-stop:
		return nil, context.Canceled
	case err := <-result:
		if shouldSkipDrainUntilIdle(true, err) {
			return nil, nil
		}
		return claimDue()
	}
}

func pollerHasSession(t *tmux.Tmux, session string) (bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		exists, err := t.HasSession(session)
		if err == nil || exists {
			return exists, err
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return false, fmt.Errorf("session check failed")
}

func watchCooperativePollerStop(ctx context.Context, interval time.Duration, stopRequested func() bool) <-chan struct{} {
	stopped := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if stopRequested() {
					close(stopped)
					return
				}
			}
		}
	}()
	return stopped
}

func claimPollerNudgeWhenIdle(
	hasPromptDetection bool,
	waitForIdle func() error,
	claimDue func() (*nudge.ClaimedNudge, error),
) (*nudge.ClaimedNudge, error) {
	if shouldSkipDrainUntilIdle(hasPromptDetection, waitForIdle()) {
		return nil, nil
	}
	return claimDue()
}

func resolvePollerSessionMetadata(t *tmux.Tmux, sessionName string) (bool, tmux.NudgeOpts) {
	var opts tmux.NudgeOpts
	agentName, _ := t.GetEnvironment(sessionName, "GT_AGENT")
	preset := config.GetAgentPresetByName(agentName)
	if preset != nil && preset.EscapeCancelsRequest {
		opts.SkipEscape = true
	}
	if prompt, err := t.GetEnvironment(sessionName, "GT_READY_PROMPT_PREFIX"); err == nil {
		return prompt != "", opts
	}
	return preset != nil && preset.ReadyPromptPrefix != "", opts
}

func shouldSkipDrainUntilIdle(hasPromptDetection bool, waitErr error) bool {
	return hasPromptDetection && waitErr != nil
}
