package doctor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/health"
)

type ControlPlaneHealthCheck struct {
	BaseCheck
	installedBinaryCommit string
	collect               func(string, string) (health.ControlPlaneVerdict, error)
}

func NewControlPlaneHealthCheck(installedBinaryCommit string) *ControlPlaneHealthCheck {
	return newControlPlaneHealthCheck(installedBinaryCommit, health.CollectControlPlaneNonDolt)
}

func newControlPlaneHealthCheck(installedBinaryCommit string, collect func(string, string) (health.ControlPlaneVerdict, error)) *ControlPlaneHealthCheck {
	return &ControlPlaneHealthCheck{
		BaseCheck: BaseCheck{
			CheckName: "control-plane-health", CheckDescription: "Aggregate evidence-backed control-plane health",
			CheckCategory: CategoryInfrastructure,
		},
		installedBinaryCommit: installedBinaryCommit,
		collect:               collect,
	}
}

func (c *ControlPlaneHealthCheck) Run(ctx *CheckContext) *CheckResult {
	result := &CheckResult{
		Name: c.Name(), Status: StatusOK,
		Message: "non-Dolt delivery/convoy/canary evidence is healthy; Dolt covered by dedicated checks",
	}
	verdict, err := c.collect(ctx.TownRoot, c.installedBinaryCommit)
	if err != nil {
		result.Status = StatusError
		result.Message = "control-plane evidence unavailable"
		result.FixHint = "run gt health --json for a sanitized diagnostic"
		return result
	}
	if len(verdict.Failures) == 0 {
		return result
	}
	result.Status = StatusError
	subsystems := make([]string, 0, len(verdict.Failures))
	for _, failure := range verdict.Failures {
		subsystems = append(subsystems, failure.Subsystem)
		result.Details = append(result.Details, failure.Subsystem+": "+failure.Diagnostic)
	}
	sort.Strings(subsystems)
	result.Message = fmt.Sprintf("%d control-plane subsystem(s) unhealthy: %s", len(verdict.Failures), strings.Join(subsystems, ", "))
	result.FixHint = "run gt health --json for per-subsystem diagnostics"
	return result
}
