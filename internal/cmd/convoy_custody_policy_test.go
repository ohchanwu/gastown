package cmd

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestNewListenerPIDsProvesPackageExitBaseline(t *testing.T) {
	baseline := []doltserver.DoltListener{{PID: 10, Port: 3307}, {PID: 11, Port: 4400}}
	after := []doltserver.DoltListener{{PID: 10, Port: 3307}, {PID: 12, Port: 4401}, {PID: 12, Port: 4402}}
	if got, want := newListenerPIDs(baseline, after), []int{12}; !reflect.DeepEqual(got, want) {
		t.Fatalf("newListenerPIDs() = %v, want %v", got, want)
	}
}

func TestCleanupStagedConvoyCustodyPropagatesForcedFailure(t *testing.T) {
	forced := errors.New("forced cleanup failure")
	err := cleanupStagedConvoyDoltCustody("/test/town", nil, func(string, []doltserver.DoltListener) error {
		return forced
	})
	if !errors.Is(err, forced) {
		t.Fatalf("cleanup error = %v, want forced failure", err)
	}
}
