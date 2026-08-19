package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMailThreadBeadsCompatibility(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed")
	}

	port := testutil.StartIsolatedDoltContainer(t)
	homeDir := t.TempDir()
	townRoot := filepath.Join(t.TempDir(), "town")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"mail-thread-test"}`), 0644); err != nil {
		t.Fatal(err)
	}
	env := mailThreadCompatibilityEnv(homeDir, townRoot, port)

	runMailThreadFixtureCommand(t, townRoot, env, "bd",
		"init", "--prefix", "mt", "--database", "mail_thread_test",
		"--server", "--external", "--server-host", "127.0.0.1", "--server-port", port,
		"--skip-hooks", "--skip-agents", "--non-interactive")

	threadID := "thread-fixture"
	firstID := createMailThreadFixture(t, townRoot, env, "first", false, "gt:message", "thread:"+threadID, "from:deacon/")
	secondID := createMailThreadFixture(t, townRoot, env, "second", false, "gt:message", "thread:"+threadID, "from:mayor/")
	wispID := createMailThreadFixture(t, townRoot, env, "ephemeral", true, "gt:message", "thread:"+threadID, "from:deacon/")
	createMailThreadFixture(t, townRoot, env, "other thread", false, "gt:message", "thread:other", "from:deacon/")
	createMailThreadFixture(t, townRoot, env, "missing message label", false, "thread:"+threadID, "from:deacon/")
	createMailThreadFixture(t, townRoot, env, "missing thread label", false, "gt:message", "from:deacon/")
	runMailThreadFixtureCommand(t, townRoot, env, "bd", "close", firstID, "--reason", "read")

	gtBinary := buildGT(t)
	output := runMailThreadFixtureCommand(t, townRoot, env, gtBinary, "mail", "thread", threadID, "--json")
	var messages []*mail.Message
	if err := json.Unmarshal([]byte(output), &messages); err != nil {
		t.Fatalf("decode gt mail thread output: %v\nstdout: %s", err, output)
	}
	wantIDs := map[string]bool{firstID: true, secondID: true, wispID: true}
	if len(messages) != len(wantIDs) {
		t.Fatalf("gt mail thread returned %d messages, want %d: %s", len(messages), len(wantIDs), output)
	}
	byID := make(map[string]*mail.Message, len(messages))
	for i, message := range messages {
		if !wantIDs[message.ID] {
			t.Fatalf("unexpected message ID %q: %s", message.ID, output)
		}
		byID[message.ID] = message
		if i > 0 {
			previous := messages[i-1]
			if message.Timestamp.Before(previous.Timestamp) ||
				(message.Timestamp.Equal(previous.Timestamp) && message.ID < previous.ID) {
				t.Fatalf("messages are not ordered by timestamp then ID: %s", output)
			}
		}
	}
	if !byID[firstID].Read {
		t.Fatalf("closed message %q was not preserved as read", firstID)
	}
	if !byID[wispID].Wisp {
		t.Fatalf("ephemeral message %q was not preserved as a wisp", wispID)
	}

	missing := runMailThreadFixtureCommand(t, townRoot, env, gtBinary, "mail", "thread", "thread-missing", "--json")
	if strings.TrimSpace(missing) != "[]" {
		t.Fatalf("missing thread output = %q, want []", missing)
	}

	envelopeEnv := append(append([]string(nil), env...), "BD_JSON_ENVELOPE=1")
	enveloped := runMailThreadFixtureCommand(t, townRoot, envelopeEnv, gtBinary, "mail", "thread", threadID, "--json")
	var envelopedMessages []*mail.Message
	if err := json.Unmarshal([]byte(enveloped), &envelopedMessages); err != nil {
		t.Fatalf("decode enveloped gt mail thread output: %v\nstdout: %s", err, enveloped)
	}
	if len(envelopedMessages) != len(wantIDs) {
		t.Fatalf("enveloped gt mail thread returned %d messages, want %d: %s", len(envelopedMessages), len(wantIDs), enveloped)
	}
	envelopedMissing := runMailThreadFixtureCommand(t, townRoot, envelopeEnv, gtBinary, "mail", "thread", "thread-missing", "--json")
	if strings.TrimSpace(envelopedMissing) != "[]" {
		t.Fatalf("enveloped missing thread output = %q, want []", envelopedMissing)
	}
}

func createMailThreadFixture(t *testing.T, townRoot string, env []string, title string, ephemeral bool, labels ...string) string {
	t.Helper()
	args := []string{"create", title, "--assignee", "mayor", "--labels", strings.Join(labels, ","), "--silent"}
	if ephemeral {
		args = append(args, "--ephemeral")
	}
	id := strings.TrimSpace(runMailThreadFixtureCommand(t, townRoot, env, "bd", args...))
	if id == "" || strings.ContainsAny(id, " \t\n") {
		t.Fatalf("bd create returned invalid ID %q", id)
	}
	return id
}

func runMailThreadFixtureCommand(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...) //nolint:gosec // fixed test binaries and arguments
	command.Dir = dir
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("%s %s: %v\nstdout: %s\nstderr: %s", name, strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func mailThreadCompatibilityEnv(homeDir, townRoot, port string) []string {
	env := make([]string, 0, len(os.Environ())+10)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "HOME=") ||
			strings.HasPrefix(entry, "GT_") ||
			strings.HasPrefix(entry, "BD_") ||
			strings.HasPrefix(entry, "BEADS_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HOME="+homeDir,
		"GT_TOWN_ROOT="+townRoot,
		"GT_ROOT="+townRoot,
		"GT_ROLE=mayor",
		"GT_DOLT_HOST=127.0.0.1",
		"GT_DOLT_PORT="+port,
		"BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"BEADS_DOLT_SERVER_PORT="+port,
		"BEADS_DOLT_PORT="+port,
		"BEADS_DOLT_AUTO_START=0",
	)
}
