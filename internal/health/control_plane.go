package health

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
)

const wakeDeadline = 30 * time.Second

type MailEvidence struct {
	Priority  string
	Type      string
	WrittenAt time.Time
}

type WakeEvidence struct {
	Priority    string
	QueuedAt    time.Time
	FailureCode string
}

type ConvoyEvidence struct {
	TimedOut bool
	Duration time.Duration
}

type CanaryEvidence struct {
	BinaryCommit string
	Result       string
}

type ControlPlaneEvidence struct {
	Now                    time.Time
	MayorMail              []MailEvidence
	WakeDeliveries         []WakeEvidence
	ActionableDoltLeaks    int
	CanonicalDoltReachable bool
	ConvoyCheck            *ConvoyEvidence
	Canary                 *CanaryEvidence
	InstalledBinaryCommit  string
}

type ControlPlaneFailure struct {
	Subsystem  string `json:"subsystem"`
	Diagnostic string `json:"diagnostic"`
}

type ControlPlaneVerdict struct {
	Healthy  bool                  `json:"healthy"`
	Failures []ControlPlaneFailure `json:"failures,omitempty"`
}

type ConvoyCheckState struct {
	SchemaVersion    int       `json:"schema_version"`
	CheckedAt        time.Time `json:"checked_at"`
	DurationMS       int64     `json:"duration_ms"`
	TimedOut         bool      `json:"timed_out"`
	SkippedUncertain int       `json:"skipped_uncertain"`
}

type canaryState struct {
	InstalledBinaryCommit string `json:"installed_binary_commit"`
	Result                string `json:"result"`
}

type controlPlaneSources struct {
	now                    func() time.Time
	unreadMayorMail        func(string) ([]*mail.Message, error)
	queuedMayorWake        func(string) ([]nudge.QueuedNudge, error)
	inventory              func(string) []doltserver.LocalDoltServer
	canonicalDoltReachable func(string) (bool, error)
	readFile               func(string) ([]byte, error)
}

func EvaluateControlPlane(evidence ControlPlaneEvidence) ControlPlaneVerdict {
	failures := make([]ControlPlaneFailure, 0)
	add := func(subsystem, diagnostic string) {
		for _, failure := range failures {
			if failure.Subsystem == subsystem {
				return
			}
		}
		failures = append(failures, ControlPlaneFailure{Subsystem: subsystem, Diagnostic: diagnostic})
	}

	for _, message := range evidence.MayorMail {
		actionable := message.Priority == "urgent" || message.Priority == "high" ||
			message.Type == "task" || message.Type == "escalation"
		if actionable && evidence.Now.Sub(message.WrittenAt) > wakeDeadline {
			add("mayor-mail", "gt mail inbox mayor/")
		}
	}
	for _, delivery := range evidence.WakeDeliveries {
		if delivery.Priority != "urgent" {
			continue
		}
		if delivery.FailureCode != "" || evidence.Now.Sub(delivery.QueuedAt) > wakeDeadline {
			add("wake-delivery", "gt nudge --help")
		}
	}
	if !evidence.CanonicalDoltReachable {
		add("dolt", "gt dolt status")
	} else if evidence.ActionableDoltLeaks > 0 {
		add("dolt", "gt dolt cleanup-test-leaks")
	}
	if evidence.ConvoyCheck != nil && (evidence.ConvoyCheck.TimedOut || evidence.ConvoyCheck.Duration > wakeDeadline) {
		add("convoy", "gt convoy check --dry-run --json")
	}
	if evidence.Canary != nil && evidence.Canary.BinaryCommit == evidence.InstalledBinaryCommit && evidence.Canary.Result == "failed" {
		add("wake-canary", "gt nudge-canary --help")
	}

	return ControlPlaneVerdict{Healthy: len(failures) == 0, Failures: failures}
}

func CollectControlPlane(townRoot, installedBinaryCommit string) (ControlPlaneVerdict, error) {
	return collectControlPlane(townRoot, installedBinaryCommit, defaultControlPlaneSources(townRoot))
}

func CollectControlPlaneNonDolt(townRoot, installedBinaryCommit string) (ControlPlaneVerdict, error) {
	return collectControlPlaneNonDolt(townRoot, installedBinaryCommit, defaultControlPlaneSources(townRoot))
}

func CollectControlPlaneWithDoltEvidence(townRoot, installedBinaryCommit string, actionableDoltLeaks int, canonicalDoltReachable bool) (ControlPlaneVerdict, error) {
	return collectControlPlaneWithDolt(townRoot, installedBinaryCommit, defaultControlPlaneSources(townRoot), &ControlPlaneEvidence{
		ActionableDoltLeaks: actionableDoltLeaks, CanonicalDoltReachable: canonicalDoltReachable,
	})
}

func defaultControlPlaneSources(townRoot string) controlPlaneSources {
	mailbox := mail.NewMailboxFromAddress("mayor/", townRoot)
	return controlPlaneSources{
		now:             time.Now,
		unreadMayorMail: func(string) ([]*mail.Message, error) { return mailbox.ListUnread() },
		queuedMayorWake: func(root string) ([]nudge.QueuedNudge, error) {
			return nudge.ListQueued(root, session.MayorSessionName())
		},
		inventory:              doltserver.InventoryLocalDoltServers,
		canonicalDoltReachable: func(root string) (bool, error) { running, _, err := doltserver.IsRunning(root); return running, err },
		readFile:               os.ReadFile,
	}
}

func collectControlPlane(townRoot, installedBinaryCommit string, sources controlPlaneSources) (ControlPlaneVerdict, error) {
	return collectControlPlaneWithDolt(townRoot, installedBinaryCommit, sources, nil)
}

func collectControlPlaneNonDolt(townRoot, installedBinaryCommit string, sources controlPlaneSources) (ControlPlaneVerdict, error) {
	return collectControlPlaneWithDolt(townRoot, installedBinaryCommit, sources, &ControlPlaneEvidence{CanonicalDoltReachable: true})
}

func collectControlPlaneWithDolt(townRoot, installedBinaryCommit string, sources controlPlaneSources, doltEvidence *ControlPlaneEvidence) (ControlPlaneVerdict, error) {
	messages, err := sources.unreadMayorMail(townRoot)
	if err != nil {
		return ControlPlaneVerdict{}, errors.New("reading Mayor mail evidence failed")
	}
	queued, err := sources.queuedMayorWake(townRoot)
	if err != nil {
		return ControlPlaneVerdict{}, errors.New("reading wake delivery evidence failed")
	}
	evidence := ControlPlaneEvidence{
		Now: sources.now(), InstalledBinaryCommit: installedBinaryCommit,
	}
	if doltEvidence != nil {
		evidence.ActionableDoltLeaks = doltEvidence.ActionableDoltLeaks
		evidence.CanonicalDoltReachable = doltEvidence.CanonicalDoltReachable
	} else {
		reachable, err := sources.canonicalDoltReachable(townRoot)
		if err != nil {
			return ControlPlaneVerdict{}, errors.New("reading canonical Dolt evidence failed")
		}
		evidence.CanonicalDoltReachable = reachable
		evidence.ActionableDoltLeaks = SummarizeLocalDoltServers(sources.inventory(townRoot)).ActionableCount
	}
	for _, message := range messages {
		evidence.MayorMail = append(evidence.MayorMail, MailEvidence{
			Priority: string(message.Priority), Type: string(message.Type), WrittenAt: message.Timestamp,
		})
	}
	for _, delivery := range queued {
		evidence.WakeDeliveries = append(evidence.WakeDeliveries, WakeEvidence{
			Priority: delivery.Priority, QueuedAt: delivery.Timestamp, FailureCode: delivery.LastErrorCode,
		})
	}

	convoyPath := filepath.Join(townRoot, constants.DirRuntime, "health", "convoy-check.json")
	if data, err := sources.readFile(convoyPath); err == nil {
		var state ConvoyCheckState
		if json.Unmarshal(data, &state) != nil {
			return ControlPlaneVerdict{}, errors.New("reading convoy health evidence failed")
		}
		evidence.ConvoyCheck = &ConvoyEvidence{TimedOut: state.TimedOut, Duration: time.Duration(state.DurationMS) * time.Millisecond}
	} else if !os.IsNotExist(err) {
		return ControlPlaneVerdict{}, errors.New("reading convoy health evidence failed")
	}

	canaryPath := filepath.Join(townRoot, constants.DirRuntime, "canary", "control-plane.json")
	if data, err := sources.readFile(canaryPath); err == nil {
		var state canaryState
		if json.Unmarshal(data, &state) != nil {
			return ControlPlaneVerdict{}, errors.New("reading wake canary evidence failed")
		}
		evidence.Canary = &CanaryEvidence{BinaryCommit: state.InstalledBinaryCommit, Result: state.Result}
	} else if !os.IsNotExist(err) {
		return ControlPlaneVerdict{}, errors.New("reading wake canary evidence failed")
	}

	return EvaluateControlPlane(evidence), nil
}

func WriteConvoyCheckEvidence(townRoot string, state ConvoyCheckState) (string, error) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(townRoot, constants.DirRuntime, "health")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "convoy-check.json")
	return path, atomicfile.WriteFile(path, data, 0600)
}
