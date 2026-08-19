package daemon

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Dog lifecycle defaults — now config-driven via operational.daemon thresholds.
// These vars are still used as fallbacks and for tests; production code
// should prefer d.daemonCfg() accessors loaded from TownSettings.
var (
	// dogIdleSessionTimeout is how long a dog can be idle with a live tmux
	// session before the session is killed (default 1h).
	// Configurable via operational.daemon.dog_idle_session_timeout.
	dogIdleSessionTimeout = config.DefaultDogIdleSessionTimeout

	// dogIdleRemoveTimeout is how long a dog can be idle before it is removed
	// from the kennel entirely (only when pool is oversized, default 4h).
	// Configurable via operational.daemon.dog_idle_remove_timeout.
	dogIdleRemoveTimeout = config.DefaultDogIdleRemoveTimeout

	// staleWorkingTimeout is how long a dog can be in state=working with no
	// activity updates before it is considered stuck (default 2h).
	// Configurable via operational.daemon.stale_working_timeout.
	staleWorkingTimeout = config.DefaultStaleWorkingTimeout

	// maxDogPoolSize is the target pool size (default 4).
	// Configurable via operational.daemon.max_dog_pool_size.
	maxDogPoolSize = config.DefaultMaxDogPoolSize
)

// handleDogs manages Dog lifecycle: cleanup stuck dogs, reap idle dogs, then dispatch plugins.
// This is the main entry point called from heartbeat.
func (d *Daemon) handleDogs() {
	rigsConfig, err := d.loadRigsConfig()
	if err != nil {
		d.logger.Printf("Handler: failed to load rigs config: %v", err)
		return
	}

	opCfg := d.loadOperationalConfig().GetDaemonConfig()

	mgr := dog.NewManager(d.config.TownRoot, rigsConfig)
	t := tmux.NewTmux()
	sm := dog.NewSessionManager(t, d.config.TownRoot, mgr)

	d.cleanupStuckDogs(mgr, sm)
	d.detectStaleWorkingDogs(mgr, sm, opCfg)
	d.reapIdleDogs(mgr, sm, opCfg)
	d.dispatchPlugins(mgr, sm, rigsConfig)
}

// handleDogsCleanupOnly runs dog lifecycle cleanup (stuck, stale, idle) without
// dispatching new work. Used when pressure checks block new spawns.
func (d *Daemon) handleDogsCleanupOnly() {
	rigsConfig, err := d.loadRigsConfig()
	if err != nil {
		d.logger.Printf("Handler: failed to load rigs config: %v", err)
		return
	}

	opCfg := d.loadOperationalConfig().GetDaemonConfig()

	mgr := dog.NewManager(d.config.TownRoot, rigsConfig)
	t := tmux.NewTmux()
	sm := dog.NewSessionManager(t, d.config.TownRoot, mgr)

	d.cleanupStuckDogs(mgr, sm)
	d.detectStaleWorkingDogs(mgr, sm, opCfg)
	d.reapIdleDogs(mgr, sm, opCfg)
	// Skip dispatchPlugins — under pressure
}

// cleanupStuckDogs finds dogs in state=working whose tmux session or agent
// process is dead and clears their work so they return to idle.
func (d *Daemon) cleanupStuckDogs(mgr *dog.Manager, sm *dog.SessionManager) {
	dogs, err := mgr.List()
	if err != nil {
		d.logger.Printf("Handler: failed to list dogs: %v", err)
		return
	}

	hc := dog.NewHealthChecker(mgr, tmux.NewTmux())
	for _, dg := range dogs {
		if dg.State != dog.StateWorking {
			continue
		}

		result := hc.Check(dg, 0, true)
		if result.AutoCleared {
			d.logger.Printf("Handler: dog %s auto-cleared after exact health check (%s)", dg.Name, result.SessionStatus)
		} else if result.NeedsAttention {
			d.logger.Printf("Handler: dog %s exact health requires recovery: %s", dg.Name, result.Recommendation)
		}
	}
}

// detectStaleWorkingDogs finds dogs in state=working whose last_active exceeds
// staleWorkingTimeout. These dogs have live tmux sessions sitting idle at a
// prompt — neither cleanupStuckDogs (needs dead session) nor reapIdleDogs
// (needs state=idle) will catch them.
func (d *Daemon) detectStaleWorkingDogs(mgr *dog.Manager, sm *dog.SessionManager, daemonCfg *config.DaemonThresholds) {
	dogs, err := mgr.List()
	if err != nil {
		d.logger.Printf("Handler: failed to list dogs for stale-working check: %v", err)
		return
	}

	threshold := daemonCfg.StaleWorkingTimeoutD()
	now := time.Now()
	for _, dg := range dogs {
		if dg.State != dog.StateWorking {
			continue
		}

		staleDuration := now.Sub(dg.LastActive)
		if staleDuration < threshold {
			continue
		}

		d.logger.Printf("Handler: dog %s stuck in working state (inactive %v, work: %s), clearing",
			dg.Name, staleDuration.Truncate(time.Minute), dg.Work)

		// StopIfMatches handles both exact live teardown and exact absent-session
		// state reconciliation. It refuses live legacy or same-name replacement
		// sessions instead of mutating them by reusable name.
		if err := sm.StopIfMatches(dg, true); err != nil {
			d.logger.Printf("Handler: failed exact stale-working cleanup for dog %s: %v", dg.Name, err)
		}
	}
}

// reapIdleDogs kills tmux sessions for dogs that have been idle too long, and
// removes long-idle dogs from the kennel when the pool is oversized.
func (d *Daemon) reapIdleDogs(mgr *dog.Manager, sm *dog.SessionManager, daemonCfg *config.DaemonThresholds) {
	dogs, err := mgr.List()
	if err != nil {
		d.logger.Printf("Handler: failed to list dogs for reaping: %v", err)
		return
	}

	idleSessionTimeout := daemonCfg.DogIdleSessionTimeoutD()
	idleRemoveTimeout := daemonCfg.DogIdleRemoveTimeoutD()
	poolMax := daemonCfg.MaxDogPoolSizeV()

	now := time.Now()
	poolSize := len(dogs)

	for _, dg := range dogs {
		if dg.State != dog.StateIdle {
			continue
		}
		if dg.SessionGeneration == nil && !dg.SessionAbsenceProven {
			d.logger.Printf("Handler: preserving idle dog %s: session absence is unproven", dg.Name)
			continue
		}

		idleDuration := now.Sub(dg.LastActive)

		// Phase 1: kill stale tmux sessions for idle dogs.
		if idleDuration >= idleSessionTimeout {
			running, err := sm.IsRunning(dg.Name)
			if err != nil {
				d.logger.Printf("Handler: error checking session for idle dog %s: %v", dg.Name, err)
				continue
			}
			if running || dg.SessionGeneration != nil {
				d.logger.Printf("Handler: reaping idle dog %s session (idle %v)", dg.Name, idleDuration.Truncate(time.Minute))
				if err := sm.StopIfMatches(dg, true); err != nil {
					d.logger.Printf("Handler: failed to stop session for idle dog %s: %v", dg.Name, err)
					continue
				}
				// Exact stop refreshes lifecycle activity. Do not remove the same dog
				// from the kennel using the stale pre-stop List snapshot.
				continue
			}
		}

		// Phase 2: remove long-idle dogs when pool is oversized.
		if poolSize > poolMax && idleDuration >= idleRemoveTimeout {
			d.logger.Printf("Handler: removing long-idle dog %s from kennel (idle %v, pool %d/%d)",
				dg.Name, idleDuration.Truncate(time.Minute), poolSize, poolMax)

			// Ensure session is dead before removing.
			running, runErr := sm.IsRunning(dg.Name)
			if runErr != nil {
				d.logger.Printf("Handler: failed to verify idle dog %s session before removal: %v", dg.Name, runErr)
				continue
			}
			if running || dg.SessionGeneration != nil {
				if err := sm.StopIfMatches(dg, true); err != nil {
					d.logger.Printf("Handler: failed exact stop before removing idle dog %s: %v", dg.Name, err)
				}
				continue
			}

			removed, err := mgr.RemoveIfMatches(dg)
			if err != nil {
				d.logger.Printf("Handler: failed to remove idle dog %s: %v", dg.Name, err)
				continue
			}
			if !removed {
				d.logger.Printf("Handler: skipped removing idle dog %s: lifecycle state changed", dg.Name)
				continue
			}
			poolSize--
		}
	}
}

// dispatchPlugins scans for plugins, evaluates cooldown gates, and dispatches
// eligible plugins to idle dogs.
func (d *Daemon) dispatchPlugins(mgr *dog.Manager, sm *dog.SessionManager, rigsConfig *config.RigsConfig) {
	// Get rig names for scanner
	var rigNames []string
	if rigsConfig != nil {
		for name := range rigsConfig.Rigs {
			rigNames = append(rigNames, name)
		}
	}

	scanner := plugin.NewScanner(d.config.TownRoot, rigNames)
	plugins, err := scanner.DiscoverAll()
	if err != nil {
		d.logger.Printf("Handler: failed to discover plugins: %v", err)
		return
	}

	if len(plugins) == 0 {
		return
	}

	recorder := plugin.NewRecorder(d.config.TownRoot)
	router := mail.NewRouterWithTownRoot(d.config.TownRoot, d.config.TownRoot)

	for _, p := range plugins {
		// Never auto-dispatch manual-gate plugins — they require an explicit trigger.
		if p.Gate != nil && p.Gate.Type == plugin.GateManual {
			d.logger.Printf("Handler: skipping plugin %s (gate=manual, requires explicit trigger)", p.Name)
			continue
		}

		// Only dispatch plugins with cooldown gates.
		if p.Gate == nil || p.Gate.Type != plugin.GateCooldown {
			continue
		}

		// Evaluate cooldown: skip if plugin ran recently.
		if p.Gate.Duration != "" {
			count, err := recorder.CountRunsSince(p.Name, p.Gate.Duration)
			if err != nil {
				d.logger.Printf("Handler: error checking cooldown for plugin %s: %v", p.Name, err)
				continue
			}
			if count > 0 {
				continue // Still in cooldown
			}
		}

		// Find an idle dog that doesn't already have a live tmux session.
		// A leaked session (dog marked idle before its tmux terminated) would
		// cause sm.Start to fail with "session already running", and since
		// mgr.List() returns dogs in directory order, GetIdleDog would always
		// pick the same first idle dog — infinite-looping the same failed
		// dispatch instead of advancing to the next idle dog in the pack.
		// See gt-o24.
		idleDog := findDispatchableDog(mgr, sm, d.logger)
		if idleDog == nil {
			d.logger.Printf("Handler: no dispatchable idle dogs available, deferring remaining plugins")
			return
		}

		// Assign work and start session.
		workDesc := fmt.Sprintf("plugin:%s", p.Name)
		assignedState, err := mgr.AssignWorkIfIdle(idleDog.Name, workDesc)
		if err != nil {
			d.logger.Printf("Handler: failed to assign work to dog %s: %v", idleDog.Name, err)
			continue
		}

		// Send mail with plugin instructions BEFORE starting the session
		// so the dog finds work in its inbox on first check.
		msg := newDogPluginDispatchMessage(idleDog.Name, assignedState, p)
		if err := router.Send(msg); err != nil {
			d.logger.Printf("Handler: failed to send mail to dog %s: %v", idleDog.Name, err)
			// Roll back assignment — no point starting a session without instructions.
			cleared, clearErr := rollbackDogDispatchAssignment(mgr, idleDog.Name, assignedState)
			if clearErr != nil {
				d.logger.Printf("Handler: failed to clear work after mail failure for dog %s: %v", idleDog.Name, clearErr)
			} else if !cleared {
				d.logger.Printf("Handler: preserved changed assignment after mail failure for dog %s", idleDog.Name)
			}
			continue
		}

		if err := sm.Start(idleDog.Name, dog.SessionStartOptions{
			WorkDesc:          workDesc,
			AssignmentReceipt: assignedState.StartReceipt,
		}); err != nil {
			d.logger.Printf("Handler: failed to start session for dog %s: %v", idleDog.Name, err)
			// A cleanup-incomplete start may still own an exact live generation.
			// Preserve the assignment so the dog cannot be reused until recovery.
			if errors.Is(err, dog.ErrSessionStartCleanupIncomplete) {
				d.logger.Printf("Handler: preserving assignment for dog %s because exact session cleanup is incomplete", idleDog.Name)
				continue
			}
			cleared, clearErr := rollbackDogDispatchAssignment(mgr, idleDog.Name, assignedState)
			if clearErr != nil {
				d.logger.Printf("Handler: failed to clear work after start failure for dog %s: %v", idleDog.Name, clearErr)
			} else if !cleared {
				d.logger.Printf("Handler: preserved changed assignment after start failure for dog %s", idleDog.Name)
			}
			continue
		}

		d.logger.Printf("Handler: dispatched plugin %s to dog %s", p.Name, idleDog.Name)

		// Record the dispatch immediately so the cooldown gate is satisfied
		// for the next 1h regardless of what the dog does. Dogs create their
		// own completion beads but don't reliably use the label convention the
		// gate requires, causing infinite re-dispatch loops.
		if _, err := recorder.RecordRun(plugin.PluginRunRecord{
			PluginName: p.Name,
			Result:     plugin.ResultSuccess,
			Body:       fmt.Sprintf("Dispatched to dog %s", idleDog.Name),
		}); err != nil {
			d.logger.Printf("Handler: failed to record dispatch for plugin %s: %v", p.Name, err)
		}
	}
}

func rollbackDogDispatchAssignment(mgr *dog.Manager, dogName string, assigned *dog.DogState) (bool, error) {
	if mgr == nil || assigned == nil {
		return false, errors.New("dog dispatch rollback evidence is unavailable")
	}
	return mgr.ClearWorkIfMatches(dogName, assigned.Work, assigned.WorkStartedAt)
}

func newDogPluginDispatchMessage(dogName string, assigned *dog.DogState, p *plugin.Plugin) *mail.Message {
	msg := mail.NewMessage(
		"daemon",
		fmt.Sprintf("deacon/dogs/%s", dogName),
		fmt.Sprintf("Plugin: %s", p.Name),
		p.FormatMailBody(),
	)
	msg.Type = mail.TypeTask
	msg.Timestamp = time.Now()
	if assigned != nil {
		msg.ThreadID = dog.AssignmentThreadID(dogName, assigned.Work, assigned.WorkStartedAt)
	}
	return msg
}

// findDispatchableDog returns the first dog in the kennel whose registry
// state is idle AND whose tmux session is NOT currently running. Returns nil
// when no dog satisfies both conditions.
//
// This exists because a dog can be marked idle (via gt dog done or the reaper)
// before its tmux session fully terminates, producing a transient window where
// sm.Start would fail with "session already running". Picking that dog every
// dispatch tick infinite-loops the same failed dispatch instead of advancing
// to another genuinely-free dog in the pack. See gt-o24.
//
// IsRunning errors are logged and treated as "not dispatchable" so a flaky
// tmux check can't wedge the whole dispatch cycle.
func findDispatchableDog(mgr *dog.Manager, sm *dog.SessionManager, logger *log.Logger) *dog.Dog {
	dogs, err := mgr.List()
	if err != nil {
		logger.Printf("Handler: failed to list dogs while picking dispatch target: %v", err)
		return nil
	}
	for _, d := range dogs {
		if d.State != dog.StateIdle {
			continue
		}
		running, err := sm.IsRunning(d.Name)
		if err != nil {
			logger.Printf("Handler: IsRunning check failed for dog %s: %v; skipping", d.Name, err)
			continue
		}
		if running {
			continue
		}
		return d
	}
	return nil
}

// loadRigsConfig loads the rigs configuration from mayor/rigs.json.
func (d *Daemon) loadRigsConfig() (*config.RigsConfig, error) {
	rigsPath := filepath.Join(d.config.TownRoot, "mayor", "rigs.json")
	return config.LoadRigsConfig(rigsPath)
}

// loadOperationalConfig loads operational thresholds from town settings.
// Returns a valid (never nil) config — accessors return defaults for nil fields.
func (d *Daemon) loadOperationalConfig() *config.OperationalConfig {
	return config.LoadOperationalConfig(d.config.TownRoot)
}
