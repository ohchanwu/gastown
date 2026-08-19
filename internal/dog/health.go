package dog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// sessionChecker abstracts the tmux health-check methods needed by the
// health checker.  Satisfied by *tmux.Tmux; mockable in tests.
type sessionChecker interface {
	CheckSessionHealth(session string, maxInactivity time.Duration) tmux.ZombieStatus
	HasSession(name string) (bool, error)
	CaptureSessionGeneration(name string) (tmux.SessionGeneration, error)
	KillSessionGenerationWithProcessesPortableContext(context.Context, tmux.SessionGeneration) error
}

// DogHealthResult describes the health of a single dog.
type DogHealthResult struct {
	Name           string        `json:"name"`
	State          State         `json:"state"`
	SessionStatus  string        `json:"session_status"`          // from ZombieStatus.String()
	WorkDuration   time.Duration `json:"work_duration,omitempty"` // how long current work has been running
	NeedsAttention bool          `json:"needs_attention"`
	AutoCleared    bool          `json:"auto_cleared,omitempty"`
	Recommendation string        `json:"recommendation,omitempty"`
}

// HealthChecker performs health checks on dogs in the kennel.
type HealthChecker struct {
	mgr                  *Manager
	checker              sessionChecker
	checkerForGeneration func(tmux.SessionGeneration) (sessionChecker, error)
}

// NewHealthChecker creates a HealthChecker.
func NewHealthChecker(mgr *Manager, checker sessionChecker) *HealthChecker {
	return &HealthChecker{
		mgr:     mgr,
		checker: checker,
		checkerForGeneration: func(generation tmux.SessionGeneration) (sessionChecker, error) {
			return tmux.NewTmuxForSessionGeneration(generation)
		},
	}
}

// dogSessionName returns the tmux session name for a dog.
func dogSessionName(name string) string {
	return fmt.Sprintf("hq-dog-%s", name)
}

// Check performs a health check on a single dog.
func (hc *HealthChecker) Check(d *Dog, maxInactivity time.Duration, autoClear bool) DogHealthResult {
	result := DogHealthResult{
		Name:  d.Name,
		State: d.State,
	}

	// Compute work duration if working and WorkStartedAt is set.
	if d.State == StateWorking && !d.WorkStartedAt.IsZero() {
		result.WorkDuration = time.Since(d.WorkStartedAt)
	}

	session := dogSessionName(d.Name)
	checker, checkerErr := hc.checkerForDog(d)
	if checkerErr != nil {
		result.SessionStatus = "unknown"
		result.NeedsAttention = true
		result.Recommendation = "health check failed: " + checkerErr.Error()
		return result
	}

	switch d.State {
	case StateWorking:
		status := checker.CheckSessionHealth(session, maxInactivity)
		result.SessionStatus = status.String()

		switch status {
		case tmux.SessionDead:
			// Zombie: state says working but session is gone.
			result.NeedsAttention = true
			result.Recommendation = "zombie: session dead but state=working"
			if autoClear {
				if err := hc.clearExactDogRuntimeWithChecker(d, false, checker); err == nil {
					result.AutoCleared = true
					result.Recommendation = "zombie auto-cleared (session dead)"
				} else {
					result.Recommendation = "zombie: auto-clear failed: " + err.Error()
				}
			}

		case tmux.AgentDead:
			// Zombie: session exists but agent process died.
			result.NeedsAttention = true
			result.Recommendation = "zombie: agent dead in session"
			if autoClear {
				if err := hc.clearExactDogRuntimeWithChecker(d, true, checker); err == nil {
					result.AutoCleared = true
					result.Recommendation = "zombie auto-cleared (agent dead, session killed)"
				} else {
					result.Recommendation = "zombie: auto-clear failed: " + err.Error()
				}
			}

		case tmux.AgentHung:
			// Hung: process alive but no tmux activity for maxInactivity.
			// If autoClear is on, kill and reclaim — the dog almost certainly
			// finished its work but failed to call `gt dog done`.
			result.NeedsAttention = true
			if autoClear {
				if err := hc.clearExactDogRuntimeWithChecker(d, true, checker); err == nil {
					result.AutoCleared = true
					result.Recommendation = "hung dog auto-cleared (idle prompt, session killed)"
				} else {
					result.Recommendation = "hung: auto-clear failed: " + err.Error()
				}
			} else {
				result.Recommendation = "hung: agent alive but no tmux activity"
			}

		default: // SessionHealthy — status.String() already set above
		}

	case StateIdle:
		// Check for orphan session.
		has, _ := checker.HasSession(session)
		if has {
			result.SessionStatus = "orphan"
			result.NeedsAttention = true
			if autoClear {
				if err := hc.clearExactDogRuntimeWithChecker(d, true, checker); err == nil {
					result.AutoCleared = true
					result.Recommendation = "orphan auto-cleared (session killed)"
				} else {
					result.Recommendation = "orphan: auto-clear failed: " + err.Error()
				}
			} else {
				result.Recommendation = "orphan: dog idle but tmux session exists"
			}
		} else {
			result.SessionStatus = "none"
		}
	}

	return result
}

func (hc *HealthChecker) checkerForDog(d *Dog) (sessionChecker, error) {
	if d == nil || d.SessionGeneration == nil {
		return nil, ErrSessionGenerationUnavailable
	}
	return hc.checkerForGeneration(d.SessionGeneration.Tmux())
}

func (hc *HealthChecker) clearExactDogRuntime(d *Dog, sessionLive bool) error {
	checker, err := hc.checkerForDog(d)
	if err != nil {
		return fmt.Errorf("checking exact dog session transport: %w", err)
	}
	return hc.clearExactDogRuntimeWithChecker(d, sessionLive, checker)
}

func (hc *HealthChecker) clearExactDogRuntimeWithChecker(d *Dog, sessionLive bool, checker sessionChecker) error {
	if d == nil {
		return errors.New("dog lifecycle evidence unavailable")
	}
	if d.SessionGeneration == nil {
		return ErrSessionGenerationUnavailable
	}

	expected := d.SessionGeneration.Tmux()
	teardown := func(generation tmux.SessionGeneration) error {
		ctx, cancel := context.WithTimeout(context.Background(), dogSessionTeardownTimeout)
		defer cancel()
		return checker.KillSessionGenerationWithProcessesPortableContext(ctx, generation)
	}
	if sessionLive {
		current, err := checker.CaptureSessionGeneration(dogSessionName(d.Name))
		if err != nil {
			return fmt.Errorf("capturing exact dog session: %w", err)
		}
		if !current.Equal(expected) {
			return tmux.ErrSessionGenerationChanged
		}
	}

	var (
		cleared bool
		err     error
	)
	if d.State == StateIdle && d.Work == "" {
		cleared, err = hc.mgr.RetireSessionWithTeardownIfMatches(d.Name, expected, teardown)
	} else {
		cleared, err = hc.mgr.CompleteWorkWithTeardownIfMatches(
			d.Name,
			d.Work,
			d.WorkStartedAt,
			expected,
			teardown,
		)
	}
	if err != nil {
		return err
	}
	if !cleared {
		return errors.New("dog assignment or session generation changed during cleanup")
	}
	return nil
}

// CheckAll performs health checks on all dogs.
func (hc *HealthChecker) CheckAll(maxInactivity time.Duration, autoClear bool) ([]DogHealthResult, error) {
	dogs, err := hc.mgr.List()
	if err != nil {
		return nil, fmt.Errorf("listing dogs: %w", err)
	}

	results := make([]DogHealthResult, 0, len(dogs))
	for _, d := range dogs {
		results = append(results, hc.Check(d, maxInactivity, autoClear))
	}
	return results, nil
}

// NeedsAttentionCount returns how many results need attention.
func NeedsAttentionCount(results []DogHealthResult) int {
	n := 0
	for _, r := range results {
		if r.NeedsAttention {
			n++
		}
	}
	return n
}
