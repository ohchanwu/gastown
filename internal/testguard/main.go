package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/workspace"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: testguard snapshot|cleanup RECEIPT")
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "snapshot":
		err = writeBaseline(os.Args[2], doltserver.FindAllDoltListeners())
	case "cleanup":
		err = cleanupSinceBaseline(os.Args[2])
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

func cleanupSinceBaseline(path string) error {
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
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("find Gas Town workspace: %w", err)
	}
	return doltserver.CleanupOwnedLocalTestLeaks(townRoot, baseline)
}
