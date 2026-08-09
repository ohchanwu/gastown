package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	BrokerSafeAnnotation      = "gastown.io/session-broker-safe"
	brokerSafeArgsAnnotation  = "gastown.io/session-broker-args"
	brokerSafeFlagsAnnotation = "gastown.io/session-broker-flags"
	brokerSafeArgsCobra       = "cobra"
	brokerSafeArgsNone        = "none"
	brokerSafeArgsExactOne    = "exact:1"
	brokerSafeArgsMaximumOne  = "maximum:1"
)

var brokerPolicyMu sync.Mutex

// IsBrokerSafeCommand resolves and validates one exact reviewed Cobra leaf.
// Prefixes, aliases, hidden commands, unannotated commands, malformed flags,
// and invalid positional arguments are denied before the broker starts a
// process outside the containment namespace.
func IsBrokerSafeCommand(root *cobra.Command, args []string) error {
	brokerPolicyMu.Lock()
	defer brokerPolicyMu.Unlock()

	if root == nil || len(args) == 0 {
		return errors.New("broker command is empty")
	}
	command, _, err := root.Find(args)
	if err != nil {
		return fmt.Errorf("resolving broker command: %w", err)
	}
	if command == root || command.Hidden || command.Annotations[BrokerSafeAnnotation] != "true" {
		return fmt.Errorf("command %q is not broker-safe", command.CommandPath())
	}

	path, err := exactBrokerCommandPath(root, command)
	if err != nil {
		return err
	}
	if len(args) < len(path) {
		return errors.New("broker command path is truncated")
	}
	for index, segment := range path {
		if args[index] != segment {
			return fmt.Errorf("broker command path must use exact name %q", strings.Join(path, " "))
		}
	}
	return validateBrokerCommandArguments(command, args[len(path):])
}

func exactBrokerCommandPath(root, command *cobra.Command) ([]string, error) {
	var reversed []string
	current := command
	for current != nil && current != root {
		reversed = append(reversed, current.Name())
		current = current.Parent()
	}
	if current != root {
		return nil, errors.New("broker command is not attached to the supplied root")
	}
	path := make([]string, len(reversed))
	for index := range reversed {
		path[len(reversed)-1-index] = reversed[index]
	}
	return path, nil
}

type brokerFlagState struct {
	flag    *pflag.Flag
	value   string
	slice   []string
	isSlice bool
	changed bool
}

func validateBrokerCommandArguments(command *cobra.Command, args []string) (retErr error) {
	flags := command.Flags()
	states := make([]brokerFlagState, 0, flags.NFlag())
	flags.VisitAll(func(flag *pflag.Flag) {
		state := brokerFlagState{flag: flag, value: flag.Value.String(), changed: flag.Changed}
		if sliceValue, ok := flag.Value.(pflag.SliceValue); ok {
			state.slice = append([]string(nil), sliceValue.GetSlice()...)
			state.isSlice = true
		}
		states = append(states, state)
		// Cobra commands are process-global and other in-process callers may
		// leave Changed set. Broker validation must authorize only the flags in
		// this request, then restore the caller's exact prior state below.
		flag.Changed = false
	})
	defer func() {
		var restoreErrors []error
		for _, state := range states {
			var err error
			if state.isSlice {
				err = state.flag.Value.(pflag.SliceValue).Replace(state.slice)
			} else {
				err = state.flag.Value.Set(state.value)
			}
			if err != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restoring --%s: %w", state.flag.Name, err))
			}
			state.flag.Changed = state.changed
		}
		retErr = errors.Join(retErr, errors.Join(restoreErrors...))
	}()

	if err := command.ParseFlags(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parsing broker command flags: %w", err)
	}
	allowedFlags := make(map[string]struct{})
	for _, name := range strings.Split(command.Annotations[brokerSafeFlagsAnnotation], ",") {
		if name = strings.TrimSpace(name); name != "" {
			allowedFlags[name] = struct{}{}
		}
	}
	var forbiddenFlag string
	// FlagSet.Visit walks pflag's process-global "actual" cache, which retains
	// flags parsed by earlier in-process Cobra executions even after Changed is
	// restored. Inspect the freshly reset Changed bits directly so authorization
	// covers only this broker request.
	flags.VisitAll(func(flag *pflag.Flag) {
		if forbiddenFlag != "" {
			return
		}
		if !flag.Changed {
			return
		}
		if _, ok := allowedFlags[flag.Name]; !ok {
			forbiddenFlag = flag.Name
		}
	})
	if forbiddenFlag != "" {
		return fmt.Errorf("broker command flag --%s is not allowed", forbiddenFlag)
	}
	positionals := command.Flags().Args()
	switch policy := command.Annotations[brokerSafeArgsAnnotation]; policy {
	case "", brokerSafeArgsCobra:
		if err := command.ValidateArgs(positionals); err != nil {
			return fmt.Errorf("validating broker command arguments: %w", err)
		}
	case brokerSafeArgsNone:
		if len(positionals) != 0 {
			return fmt.Errorf("broker command accepts no positional arguments; got %d", len(positionals))
		}
	case brokerSafeArgsExactOne:
		if len(positionals) != 1 {
			return fmt.Errorf("broker command requires exactly one positional argument; got %d", len(positionals))
		}
	case brokerSafeArgsMaximumOne:
		if len(positionals) > 1 {
			return fmt.Errorf("broker command accepts at most one positional argument; got %d", len(positionals))
		}
	default:
		if strings.HasPrefix(policy, "exact:") {
			count, err := strconv.Atoi(strings.TrimPrefix(policy, "exact:"))
			if err == nil && len(positionals) == count {
				break
			}
		}
		return fmt.Errorf("invalid broker argument policy %q", policy)
	}
	if err := command.ValidateRequiredFlags(); err != nil {
		return fmt.Errorf("validating broker required flags: %w", err)
	}
	if err := command.ValidateFlagGroups(); err != nil {
		return fmt.Errorf("validating broker flag groups: %w", err)
	}
	return nil
}
