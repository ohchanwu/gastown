package convoy

import (
	"errors"
	"testing"
)

func TestConvoyTestMainExitCodeFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		testCode   int
		setupErr   error
		cleanupErr error
		want       int
	}{
		{name: "success", want: 0},
		{name: "test failure", testCode: 2, want: 2},
		{name: "setup failure", setupErr: errors.New("setup failed"), want: 1},
		{name: "cleanup failure", cleanupErr: errors.New("cleanup failed"), want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := convoyTestMainExitCode(tt.testCode, tt.setupErr, tt.cleanupErr); got != tt.want {
				t.Fatalf("convoyTestMainExitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}
