//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/steveyegge/gastown/internal/tmux"
)

func scheduleDogCloseoutHostFallback(
	_ *tmux.Tmux,
	_ string,
	executable string,
	townRoot string,
	args []string,
	environment map[string]string,
) (tmux.SessionGeneration, bool, error) {
	environment[dogCloseoutDetachedHostEnv] = environment[dogCloseoutFinalizerEnv]
	command := exec.Command(executable, args...)
	command.Dir = townRoot
	command.Env = mergeDogCloseoutEnvironment(os.Environ(), environment)
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	const createNoWindow = 0x08000000
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow,
	}
	if err := command.Start(); err != nil {
		return tmux.SessionGeneration{}, false, err
	}
	return tmux.SessionGeneration{}, false, command.Process.Release()
}

func mergeDogCloseoutEnvironment(source []string, overrides map[string]string) []string {
	result := make([]string, 0, len(source)+len(overrides))
	for _, entry := range source {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			result = append(result, entry)
		}
	}
	for key, value := range overrides {
		if value != "" {
			result = append(result, key+"="+value)
		}
	}
	return result
}
