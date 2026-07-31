package health

import (
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestSummarizeLocalDoltServersSeparatesActionableAndUnknown(t *testing.T) {
	inventory := []doltserver.LocalDoltServer{
		{DoltListener: doltserver.DoltListener{PID: 11}, Class: doltserver.DoltServerCanonical},
		{DoltListener: doltserver.DoltListener{PID: 12}, Class: doltserver.DoltServerConfiguredPortImposter},
		{DoltListener: doltserver.DoltListener{PID: 13}, Class: doltserver.DoltServerOwnedTestLeak},
		{DoltListener: doltserver.DoltListener{PID: 14}, Class: doltserver.DoltServerUnknown},
	}

	got := SummarizeLocalDoltServers(inventory)
	if got.ActionableCount != 2 || len(got.ActionablePIDs) != 2 {
		t.Fatalf("actionable summary = %#v", got)
	}
	if got.UnknownCount != 1 || len(got.UnknownPIDs) != 1 || got.UnknownPIDs[0] != 14 {
		t.Fatalf("unknown summary = %#v", got)
	}
}
