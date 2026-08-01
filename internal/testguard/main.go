package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: testguard snapshot|cleanup RECEIPT LAUNCHER_PID OWNER_ROOT")
		os.Exit(2)
	}
	launcherPID, err := strconv.Atoi(os.Args[3])
	if err != nil || launcherPID <= 0 {
		fmt.Fprintln(os.Stderr, "testguard: launcher PID must be positive")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "snapshot":
		var baseline []doltserver.DoltListener
		baseline, err = doltserver.FindAllDoltListenersWithError()
		if err == nil {
			_, err = requiredBaselineListener(baseline, launcherPID)
		}
		if err == nil {
			err = writeBaseline(os.Args[2], baseline)
		}
	case "cleanup":
		err = cleanupSinceBaseline(os.Args[2], launcherPID, os.Args[4])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "testguard: %v\n", err)
		os.Exit(1)
	}
}

func writeBaseline(path string, baseline []doltserver.DoltListener) error {
	data, err := json.Marshal(baseline)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create private baseline receipt: %w", err)
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write baseline receipt: %w", err)
	}
	return file.Close()
}

func requiredBaselineListener(baseline []doltserver.DoltListener, launcherPID int) (doltserver.DoltListener, error) {
	for _, listener := range baseline {
		if listener.PID == launcherPID {
			return listener, nil
		}
	}
	return doltserver.DoltListener{}, fmt.Errorf("launcher PID %d missing from listener inventory", launcherPID)
}

func cleanupSinceBaseline(path string, launcherPID int, ownerRoot string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat baseline receipt: %w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("baseline receipt must be private")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read baseline receipt: %w", err)
	}
	var baseline []doltserver.DoltListener
	if err := json.Unmarshal(data, &baseline); err != nil {
		return fmt.Errorf("decode baseline receipt: %w", err)
	}
	required, err := requiredBaselineListener(baseline, launcherPID)
	if err != nil {
		return err
	}
	current, err := doltserver.FindAllDoltListenersWithError()
	if err != nil {
		return err
	}
	found := false
	for _, listener := range current {
		if listener == required {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("launcher listener PID %d port %d missing during cleanup", required.PID, required.Port)
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("find Gas Town workspace: %w", err)
	}
	return doltserver.CleanupOwnedLocalTestLeaks(townRoot, baseline, ownerRoot)
}
