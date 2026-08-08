package cmd

import (
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/tmux"
)

var sessionCustodyID string

func init() {
	rootCmd.AddCommand(sessionCustodyCmd)
	sessionCustodyCmd.Flags().StringVar(&sessionCustodyID, "id", "", "generation-bound custody token")
}

var sessionCustodyCmd = &cobra.Command{
	Use:    "session-custody --id <token> -- <command>",
	Short:  "Launch an internal agent command in an OS-owned process container",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return tmux.RunSessionCustodyCommand(sessionCustodyID, args[0])
	},
}
