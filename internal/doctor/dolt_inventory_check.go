package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/doltserver"
)

type DoltInventoryCheck struct {
	BaseCheck
	inventory func(string) []doltserver.LocalDoltServer
}

func NewDoltInventoryCheck() *DoltInventoryCheck {
	return newDoltInventoryCheck(doltserver.InventoryLocalDoltServers)
}

func newDoltInventoryCheck(inventory func(string) []doltserver.LocalDoltServer) *DoltInventoryCheck {
	return &DoltInventoryCheck{
		BaseCheck: BaseCheck{
			CheckName:        "dolt-listener-inventory",
			CheckDescription: "Classify local Dolt listeners by ownership evidence",
			CheckCategory:    CategoryInfrastructure,
		},
		inventory: inventory,
	}
}

func (c *DoltInventoryCheck) Run(ctx *CheckContext) *CheckResult {
	result := &CheckResult{Name: c.Name(), Status: StatusOK, Message: "local Dolt listener inventory is clean"}
	actionable, unknown := 0, 0
	for _, server := range c.inventory(ctx.TownRoot) {
		if server.Actionable() {
			actionable++
			result.Details = append(result.Details, fmt.Sprintf("actionable: PID %d port %d (%s)", server.PID, server.Port, server.Class))
		} else if server.Class == doltserver.DoltServerUnknown {
			unknown++
			result.Details = append(result.Details, fmt.Sprintf("report only: PID %d port %d (unknown ownership)", server.PID, server.Port))
		}
	}
	if actionable > 0 {
		result.Status = StatusError
		result.Message = fmt.Sprintf("%d actionable Dolt listener(s) found", actionable)
		result.FixHint = "preview with gt dolt kill-imposters; apply only after reviewing ownership"
	} else if unknown > 0 {
		result.Status = StatusWarning
		result.Message = fmt.Sprintf("%d unknown Dolt listener(s) reported without action", unknown)
	}
	return result
}
