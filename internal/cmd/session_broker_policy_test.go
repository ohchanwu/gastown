package cmd

import (
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

func TestBrokerSafeCommandAllowsReviewedExactLeaves(t *testing.T) {
	tests := [][]string{
		{"prime"},
		{"prime", "--help"},
		{"prime", "--state", "--json"},
		{"hook"},
		{"hook", "status"},
		{"hook", "show", "gastown/witness"},
		{"mail", "inbox", "--unread"},
		{"mail", "read", "hq-example"},
		{"mail", "send", "mayor/", "--subject", "status", "--message", "ready"},
		{"nudge", "mayor", "ready"},
		{"patrol", "scan", "--rig", "gastown", "--json"},
		{"patrol", "report", "--summary", "all clear"},
		{"agents", "list"},
		{"polecat", "list", "gastown"},
		{"polecat", "status", "gastown/example"},
		{"sling", "gt-example", "gastown"},
		{"done"},
		{"health", "--json"},
		{"status", "--fast"},
	}
	for _, args := range tests {
		t.Run(args[0], func(t *testing.T) {
			if err := IsBrokerSafeCommand(rootCmd, args); err != nil {
				t.Fatalf("IsBrokerSafeCommand(%q) error = %v", args, err)
			}
		})
	}
}

func TestBrokerSafeCommandValidationRestoresFlagState(t *testing.T) {
	beforeVars := append([]string(nil), slingVars...)
	beforeState := primeState
	beforeSubject := mailSubject
	beforeVarChanged := slingCmd.Flags().Lookup("var").Changed

	if err := IsBrokerSafeCommand(rootCmd, []string{"sling", "gt-example", "gastown", "--var", "key=value"}); err != nil {
		t.Fatal(err)
	}
	if err := IsBrokerSafeCommand(rootCmd, []string{"prime", "--state"}); err != nil {
		t.Fatal(err)
	}
	if err := IsBrokerSafeCommand(rootCmd, []string{"mail", "send", "mayor/", "--subject", "changed"}); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(slingVars, beforeVars) {
		t.Fatalf("slingVars after validation = %q, want %q", slingVars, beforeVars)
	}
	if primeState != beforeState {
		t.Fatalf("primeState after validation = %v, want %v", primeState, beforeState)
	}
	if mailSubject != beforeSubject {
		t.Fatalf("mailSubject after validation = %q, want %q", mailSubject, beforeSubject)
	}
	if got := slingCmd.Flags().Lookup("var").Changed; got != beforeVarChanged {
		t.Fatalf("sling --var Changed after validation = %v, want %v", got, beforeVarChanged)
	}
}

func TestBrokerSafeCommandDeniesUnsafeOrMalformedPaths(t *testing.T) {
	tests := [][]string{
		nil,
		{"mail"},
		{"mail", "delete", "hq-example"},
		{"mail", "clear"},
		{"hook", "gt-example"},
		{"hook", "attach", "gt-example"},
		{"patrol", "digest", "--yesterday"},
		{"agents", "fix"},
		{"polecat", "nuke", "gastown/example", "--force"},
		{"sling", "respawn-reset", "gt-example"},
		{"session-custody-init"},
		{"session-custody", "--id", "00000000-0000-0000-0000-000000000000", "--", "true"},
		{"shell", "-c", "touch /tmp/escaped"},
		{"doctor", "--fix"},
		{"up", "--restore"},
		{"tmux", "send-keys", "hq-mayor", "payload"},
		{"ma", "in", "--unread"},
		{"prime", "unexpected"},
		{"mail", "inbox", "--definitely-unknown"},
		{"mail", "read", "one", "two"},
		{"patrol", "report"},
		{"polecat", "status"},
	}
	for _, args := range tests {
		t.Run(commandTestName(args), func(t *testing.T) {
			if err := IsBrokerSafeCommand(rootCmd, args); err == nil {
				t.Fatalf("IsBrokerSafeCommand(%q) unexpectedly allowed command", args)
			}
		})
	}
}

func TestBrokerSafeCommandDeniesEveryHiddenCommand(t *testing.T) {
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		for _, child := range command.Commands() {
			if child.Hidden {
				args := brokerCommandPath(child)
				if err := IsBrokerSafeCommand(rootCmd, args); err == nil {
					t.Errorf("hidden command %q is broker-safe", child.CommandPath())
				}
			}
			visit(child)
		}
	}
	visit(rootCmd)
}

func commandTestName(args []string) string {
	if len(args) == 0 {
		return "empty"
	}
	result := args[0]
	for _, arg := range args[1:] {
		result += "_" + arg
	}
	return result
}

func brokerCommandPath(command *cobra.Command) []string {
	var reversed []string
	for current := command; current != nil && current != rootCmd; current = current.Parent() {
		reversed = append(reversed, current.Name())
	}
	result := make([]string, len(reversed))
	for index := range reversed {
		result[len(reversed)-1-index] = reversed[index]
	}
	return result
}
