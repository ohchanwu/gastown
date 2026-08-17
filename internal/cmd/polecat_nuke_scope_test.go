package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	polecatpkg "github.com/steveyegge/gastown/internal/polecat"
	rigpkg "github.com/steveyegge/gastown/internal/rig"
	tmuxpkg "github.com/steveyegge/gastown/internal/tmux"
)

func TestRunPolecatNukeDoesNotSweepTownWideOrphans(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "polecat.go", nil, 0)
	if err != nil {
		t.Fatalf("parse polecat.go: %v", err)
	}

	if calls := callsTo(t, file, "runPolecatNuke", "cleanupOrphanedProcesses"); calls != 0 {
		t.Fatalf("runPolecatNuke called town-wide cleanup %d time(s); want target-only cleanup", calls)
	}
	if calls := callsTo(t, file, "runPolecatStale", "cleanupOrphanedProcesses"); calls == 0 {
		t.Fatal("runPolecatStale no longer exposes the independently gated orphan cleanup path")
	}
}

func TestPolecatNukeUsesOnlyLocalRemovalPrimitives(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "polecat.go", nil, 0)
	if err != nil {
		t.Fatalf("parse polecat.go: %v", err)
	}

	if calls := callsTo(t, file, "nukePolecatFullWithOptions", "Push"); calls != 0 {
		t.Fatalf("nukePolecatFullWithOptions called Push %d time(s), want 0", calls)
	}
	if calls := callsTo(t, file, "nukePolecatFullWithOptions", "RemoveWithOptions"); calls != 0 {
		t.Fatalf("nukePolecatFullWithOptions called publishing RemoveWithOptions %d time(s), want 0", calls)
	}
	if calls := callsTo(t, file, "nukePolecatFullWithOptions", "RemoveWithOptionsLocalOnly"); calls != 1 {
		t.Fatalf("nukePolecatFullWithOptions local-only removal calls = %d, want 1", calls)
	}
}

func TestTargetSessionNukePreservesUnrelatedSessionsOnDedicatedSocket(t *testing.T) {
	socket := fmt.Sprintf("gt-nuke-scope-%d", os.Getpid())
	tm := tmuxpkg.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = tm.KillServer() })

	for _, name := range []string{"gt-target", "gt-unrelated-a", "gt-unrelated-b"} {
		if err := tm.NewSessionWithCommand(name, t.TempDir(), "sleep 300"); err != nil {
			t.Fatalf("create session %s: %v", name, err)
		}
	}

	sessions := polecatpkg.NewSessionManager(tm, &rigpkg.Rig{Name: "scope"})
	if err := sessions.Stop("target", true); err != nil {
		t.Fatalf("nuke target session: %v", err)
	}

	for _, tc := range []struct {
		name string
		want bool
	}{
		{name: "gt-target", want: false},
		{name: "gt-unrelated-a", want: true},
		{name: "gt-unrelated-b", want: true},
	} {
		got, err := tm.HasSession(tc.name)
		if err != nil {
			t.Fatalf("check session %s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("session %s alive = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func callsTo(t *testing.T, file *ast.File, function, callee string) int {
	t.Helper()

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == function {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("function %s not found", function)
	}

	calls := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if name == callee {
			calls++
		}
		return true
	})
	return calls
}
