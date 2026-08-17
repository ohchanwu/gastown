// Package dog manages Dogs - Deacon's helper workers for infrastructure tasks.
// Dogs are reusable workers with multi-rig worktrees, managed by the Deacon.
// Unlike polecats (single-rig, ephemeral sessions), dogs handle cross-rig infrastructure work.
package dog

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// AssignmentThreadID returns the durable mail-ownership token for one exact
// dog assignment. Every dispatcher and closeout path must derive the token
// here so same-name replacement assignments cannot inherit old instructions.
func AssignmentThreadID(dogName, work string, startedAt time.Time) string {
	if dogName == "" || work == "" || startedAt.IsZero() {
		return ""
	}
	sum := sha256.Sum256([]byte(dogName + "\x00" + work + "\x00" + startedAt.UTC().Format(time.RFC3339Nano)))
	return fmt.Sprintf("dog-dispatch-%x", sum[:16])
}

// State represents a dog's operational state.
type State string

const (
	// StateIdle means the dog is available for work.
	StateIdle State = "idle"
	// StateWorking means the dog is executing a task.
	StateWorking State = "working"
)

// Dog represents a Deacon helper worker.
type Dog struct {
	Name              string             // Dog name (e.g., "alpha")
	State             State              // Current state
	Path              string             // Path to kennel dir (~/gt/deacon/dogs/<name>)
	Worktrees         map[string]string  // Rig name -> worktree path
	LastActive        time.Time          // Last activity timestamp
	Work              string             // Current work assignment (bead ID or molecule)
	WorkStartedAt     time.Time          // When current work was assigned
	CreatedAt         time.Time          // When dog was added to kennel
	SessionGeneration *SessionGeneration `json:"-"` // Exact tmux generation owned by this dog
}

// SessionGeneration is the JSON-compatible form of tmux.SessionGeneration.
// Custody may be empty on platforms without an OS containment marker.
type SessionGeneration struct {
	Name           string `json:"name"`
	SessionID      string `json:"session_id"`
	Nonce          string `json:"nonce"`
	Custody        string `json:"custody"`
	ServerPID      int    `json:"server_pid"`
	ServerIdentity string `json:"server_identity"`
}

// SessionGenerationFromTmux converts exact tmux custody into its durable form.
func SessionGenerationFromTmux(generation tmux.SessionGeneration) *SessionGeneration {
	return &SessionGeneration{
		Name:           generation.Name,
		SessionID:      generation.SessionID,
		Nonce:          generation.Nonce,
		Custody:        generation.Custody,
		ServerPID:      generation.ServerPID,
		ServerIdentity: generation.ServerIdentity,
	}
}

// Tmux converts the durable record back to exact tmux custody.
func (g SessionGeneration) Tmux() tmux.SessionGeneration {
	return tmux.SessionGeneration{
		Name:           g.Name,
		SessionID:      g.SessionID,
		Nonce:          g.Nonce,
		Custody:        g.Custody,
		ServerPID:      g.ServerPID,
		ServerIdentity: g.ServerIdentity,
	}
}

// EqualTmux reports whether the durable record identifies the same generation.
func (g SessionGeneration) EqualTmux(other tmux.SessionGeneration) bool {
	return g.Tmux().Equal(other)
}

// DogState is the persistent state stored in .dog.json.
type DogState struct {
	Name              string             `json:"name"`
	State             State              `json:"state"`
	LastActive        time.Time          `json:"last_active"`
	Work              string             `json:"work,omitempty"`            // Current work assignment
	WorkStartedAt     time.Time          `json:"work_started_at,omitempty"` // When work was assigned
	Worktrees         map[string]string  `json:"worktrees,omitempty"`       // Rig -> path (for verification)
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
	SessionGeneration *SessionGeneration `json:"session_generation,omitempty"`
}
