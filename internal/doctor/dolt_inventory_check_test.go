package doctor

import (
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestDoltInventoryCheckClassifiesConsumerVerdict(t *testing.T) {
	tests := []struct {
		name    string
		servers []doltserver.LocalDoltServer
		want    CheckStatus
	}{
		{name: "clean", want: StatusOK},
		{name: "unknown is report only", servers: []doltserver.LocalDoltServer{{Class: doltserver.DoltServerUnknown}}, want: StatusWarning},
		{name: "configured imposter is actionable", servers: []doltserver.LocalDoltServer{{Class: doltserver.DoltServerConfiguredPortImposter}}, want: StatusError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := newDoltInventoryCheck(func(string) []doltserver.LocalDoltServer { return tt.servers })
			if got := check.Run(&CheckContext{TownRoot: t.TempDir()}); got.Status != tt.want {
				t.Fatalf("status = %s, want %s", got.Status, tt.want)
			}
		})
	}
}
