//go:build linux && !integration

package witness

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/delivery"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

const (
	envContainedFlowEnabled  = "GT_TEST_CONTAINED_FLOW"
	envContainedFlowGT       = "GT_TEST_CONTAINED_GT"
	envContainedFlowDoltHost = "GT_TEST_CONTAINED_DOLT_HOST"
	envContainedFlowDoltPort = "GT_TEST_CONTAINED_DOLT_PORT"
)

func TestContainedWitnessFlow(t *testing.T) {
	if os.Getenv(envContainedFlowEnabled) != "1" {
		t.Skip("set GT_TEST_CONTAINED_FLOW=1 to run privileged containment flow")
	}
	gtBinary := requireContainedFlowExecutable(t, envContainedFlowGT)
	t.Setenv("PATH", filepath.Dir(gtBinary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	bdPath := requireContainedFlowExecutable(t, "BD_PATH")
	host := strings.TrimSpace(os.Getenv(envContainedFlowDoltHost))
	if host == "" {
		t.Fatal("GT_TEST_CONTAINED_DOLT_HOST is required")
	}
	port, err := strconv.Atoi(os.Getenv(envContainedFlowDoltPort))
	if err != nil || port <= 0 || port > 65535 {
		t.Fatalf("GT_TEST_CONTAINED_DOLT_PORT is invalid: %q", os.Getenv(envContainedFlowDoltPort))
	}

	t.Setenv("GT_DOLT_HOST", host)
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	t.Setenv("BEADS_DOLT_SERVER_HOST", host)
	t.Setenv("BEADS_DOLT_SERVER_PORT", strconv.Itoa(port))
	t.Setenv("BEADS_DOLT_PORT", strconv.Itoa(port))
	t.Setenv("BEADS_DOLT_AUTO_START", "0")
	townRoot := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	witnessDir := filepath.Join(rigPath, "witness")
	flowDir := filepath.Join(witnessDir, ".contained-flow")
	agentPath := filepath.Join(witnessDir, ".contained-flow-agent")
	containedGT := filepath.Join(witnessDir, ".contained-gt")
	mayorAgentPath := filepath.Join(townRoot, "contained-flow-mayor")
	outerSentinel := filepath.Join(t.TempDir(), "must-stay-outside-contained-root")
	if err := os.WriteFile(outerSentinel, []byte("outer-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	nonce := "contained-flow-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	setupContainedFlowTown(t, townRoot, rigName, rigPath, flowDir, agentPath, mayorAgentPath)

	prefix := "cf" + strings.ReplaceAll(uuid.NewString()[:6], "-", "")
	bd := beads.NewIsolatedWithPort(townRoot, port)
	if err := bd.Init(prefix); err != nil {
		t.Fatalf("initialize disposable town database: %v", err)
	}
	if count, err := formula.ProvisionFormulas(townRoot); err != nil || count == 0 {
		t.Fatalf("provision disposable formula catalog: count=%d err=%v", count, err)
	}
	database := beads.DatabaseNameFromMetadata(filepath.Join(townRoot, ".beads"))
	if database == "" {
		t.Fatal("disposable town database metadata has no database name")
	}
	t.Cleanup(func() { dropContainedFlowDatabase(t, host, port, database) })
	oldPatrolID := createContainedFlowPatrol(t, bd, constants.MolWitnessPatrol, rigName+"/witness")
	requireContainedFlowHTTPSHost(t)

	socket := "gt-contained-flow-" + uuid.NewString()
	t.Setenv("GT_TMUX_SOCKET", socket)
	t.Setenv("GT_TOWN_SOCKET", socket)
	transport := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() {
		if err := transport.KillServer(); err != nil && !errors.Is(err, tmux.ErrNoServer) {
			t.Logf("private tmux cleanup: %v", err)
		}
	})
	mayorSession := "hq-mayor"
	if err := transport.NewSessionWithCommandAndEnv(mayorSession, townRoot, mayorAgentPath, map[string]string{
		"GT_TEST_FLOW_DIR":       flowDir,
		"GT_AGENT":               "codex",
		"GT_READY_PROMPT_PREFIX": "› ",
	}); err != nil {
		t.Fatalf("start private Mayor nudge target: %v", err)
	}
	mayorGeneration, err := transport.CaptureSessionGeneration(mayorSession)
	if err != nil {
		t.Fatalf("capture private Mayor generation: %v", err)
	}
	t.Cleanup(func() { _ = transport.KillSessionGeneration(mayorGeneration) })

	router := mail.NewRouterWithTownRootAndTmux(townRoot, townRoot, transport)
	incoming := mail.NewMessage("mayor/", rigName+"/witness", "contained inbound "+nonce, "contained inbound body "+nonce)
	incoming.Delivery = mail.DeliveryQueue
	incoming.SuppressNotify = true
	if err := router.Send(incoming); err != nil {
		t.Fatalf("seed contained Witness mail: %v", err)
	}
	incomingID := findContainedFlowMail(t, router, rigName+"/witness", incoming.Subject)
	caPath, httpsSeen := startContainedFlowHTTPS(t, flowDir, nonce)
	non443Seen := startContainedFlowHTTP(t, nonce)
	privateDoltIP := requireContainedFlowPrivateIP(t, host)
	copyContainedFlowExecutable(t, gtBinary, containedGT)
	writeContainedFlowAgent(t, agentPath, containedFlowAgentFixture{
		GT: containedGT, BD: bdPath, MailID: incomingID, Nonce: nonce,
		Socket: socket, CA: caPath, PrivateDoltIP: privateDoltIP, PrivateDoltPort: port,
		OuterSentinel: outerSentinel,
	})
	sshListener, err := net.Listen("tcp4", "127.0.0.1:22")
	if err != nil {
		t.Fatalf("reserve outer loopback SSH endpoint: %v", err)
	}
	t.Cleanup(func() { _ = sshListener.Close() })

	mgr := NewManagerWithTmux(&rig.Rig{Name: rigName, Path: rigPath}, transport)
	supervisorStderrPath := filepath.Join(flowDir, "supervisor.stderr")
	mgr.wrapSessionCommand = func(command string) (string, string, error) {
		wrapped, custody, err := tmux.WrapSessionCommandWithCustody(gtBinary, command)
		if err != nil {
			return "", "", err
		}
		return "umask 077; " + wrapped + " 2>" + config.ShellQuote(supervisorStderrPath), custody, nil
	}
	mgr.startPoller = func(townRoot, session string) (int, error) {
		return nudge.StartPollerWithExecutable(townRoot, session, gtBinary, nil)
	}
	receiptRecorded := recordContainedFlowSubmission(townRoot, mayorSession, filepath.Join(flowDir, "mayor.input"), 60*time.Second)
	if err := mgr.Start(false, "contained-flow", []string{
		"PATH=" + filepath.Dir(gtBinary) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GT_TEST_OUTER_SENTINEL=must-not-pass",
	}); err != nil {
		t.Fatalf("start contained Witness: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		if err := mgr.Stop(); err != nil && !errors.Is(err, ErrNotRunning) {
			t.Errorf("contained Witness cleanup: %v", err)
		}
	})

	paneOutput := waitContainedFlowPane(t, transport, mgr.SessionName(), "GT_FLOW_DONE", 90*time.Second)
	results := parseContainedFlowResults(t, paneOutput)
	primeOutput := containedFlowResultNamed(t, results, "prime").Stdout
	if !strings.Contains(primeOutput, "role:"+rigName+"/witness") || !strings.Contains(primeOutput, rigName) {
		prime := containedFlowResultNamed(t, results, "prime")
		t.Fatalf("contained gt prime did not prove Witness identity: stdout=%q stderr=%q code=%d", prime.Stdout, prime.Stderr, prime.Code)
	}
	if !strings.Contains(primeOutput, "gt hook show") {
		t.Fatalf("contained prime omitted broker-scoped live follow-up: %q", primeOutput)
	}
	primeCommandResults := map[string]string{
		"gt hook":                             "hook",
		"gt hook show":                        "prime_hook_show",
		"gt mol current":                      "prime_current",
		"gt mol step close":                   "prime_step_close",
		"gt patrol scan --rig testrig --json": "patrol_scan",
		"gt patrol report --summary '<brief summary of observations>'": "patrol_report",
		"gt mail inbox --unread":      "mail_inbox",
		"gt mail read '<message-id>'": "mail_read",
		"gt mail send mayor/ --subject '<subject>' --message '<body>' --no-notify": "mail_send",
		"gt nudge mayor '<message>' --mode queue":                                  "nudge_queue",
		"gt nudge mayor '<message>' --mode immediate":                              "nudge_immediate",
		"gt agents list":          "agents",
		"gt polecat list testrig": "polecats",
		"gt status --fast":        "status",
	}
	containedContext := primeOutput[strings.Index(primeOutput, "# Contained Witness Context"):]
	segments := strings.Split(containedContext, "`")
	advertisedCommands := make(map[string]bool, len(primeCommandResults))
	for index := 1; index < len(segments); index += 2 {
		command := strings.TrimSpace(strings.ReplaceAll(segments[index], "\n", ""))
		resultName, ok := primeCommandResults[command]
		if !ok {
			t.Fatalf("contained prime emitted unexercised actionable command %q", command)
		}
		assertContainedFlowExit(t, results, resultName, true)
		advertisedCommands[command] = true
	}
	for command := range primeCommandResults {
		if !advertisedCommands[command] {
			t.Fatalf("contained prime omitted reviewed command evidence for %q", command)
		}
	}
	for _, name := range []string{"prime_hook_show", "prime_current", "prime_step_close", "prime_after_close"} {
		assertContainedFlowExit(t, results, name, true)
	}
	if shown := containedFlowResultNamed(t, results, "prime_hook_show").Stdout; !strings.Contains(shown, "[hooked]") {
		t.Fatalf("contained live prime hook-show follow-up was not executed: %q", shown)
	}
	if current := containedFlowResultNamed(t, results, "prime_current").Stdout; !strings.Contains(current, "Current:") {
		t.Fatalf("contained live prime current-step follow-up was not executed: %q", current)
	}
	if closed := containedFlowResultNamed(t, results, "prime_step_close").Stdout; !strings.Contains(closed, "Closed current step") {
		t.Fatalf("contained live prime step-close follow-up was not executed: %q", closed)
	}
	generation, err := transport.CaptureSessionGeneration(mgr.SessionName())
	if err != nil {
		t.Fatalf("capture contained Witness generation after agent flow: %v", err)
	}
	if generation.Custody == "" {
		t.Fatal("contained Witness session has no custody generation")
	}
	initialTrackedPIDs := assertContainedFlowProcessTree(t, transport, generation, supervisorStderrPath)
	info, err := os.Stat(supervisorStderrPath)
	if err != nil {
		t.Fatalf("stat contained supervisor evidence: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("contained supervisor evidence mode = %04o, want 0600", mode)
	}
	if err := <-receiptRecorded; err != nil {
		t.Fatalf("record contained Codex submission receipt: %v", err)
	}

	for _, name := range []string{
		"preset_env", "outer_env", "outer_file", "device_null", "device_full", "private_pts", "forbidden_families",
		"prime", "patrol_scan", "patrol_report", "mail_inbox", "mail_read", "mail_send",
		"nudge_queue", "nudge_immediate", "hook", "agents", "polecats", "status",
		"https",
	} {
		assertContainedFlowExit(t, results, name, true)
	}
	for _, name := range []string{
		"raw_tmux", "host_loopback", "private_dolt", "public_non443", "bd_create", "bd_list", "formula",
		"hidden", "shell", "env_bypass", "descriptor_bypass",
	} {
		assertContainedFlowExit(t, results, name, false)
	}
	for _, name := range []string{"formula", "shell", "env_bypass", "descriptor_bypass"} {
		assertContainedFlowBrokerDenial(t, results, name)
	}
	for _, name := range []string{"bd_create", "bd_list"} {
		stderr := containedFlowResultNamed(t, results, name).Stderr
		if !strings.Contains(stderr, "127.0.0.1:1") && !strings.Contains(stderr, "no beads database found") {
			t.Fatalf("contained direct %s was denied by neither the filesystem boundary nor isolated endpoint: %q", name, stderr)
		}
	}
	assertContainedFlowDenial(t, results, "hidden", 1, "not requested by a trusted launcher")
	for _, name := range []string{"mail_inbox", "mail_read", "https"} {
		if evidence := containedFlowResultNamed(t, results, name).Stdout; !strings.Contains(evidence, nonce) {
			t.Fatalf("contained flow evidence %s lacks nonce %q: %q", name, nonce, evidence)
		}
	}
	if outcomes := strings.ToLower(containedFlowResultNamed(t, results, "descriptor_bypass").Stderr); !strings.Contains(outcomes, "close=operation not permitted") ||
		!strings.Contains(outcomes, "dup2=operation not permitted") ||
		!strings.Contains(outcomes, "fcntl=allowed") {
		t.Fatalf("broker descriptor hardening evidence incomplete: %q", outcomes)
	}
	select {
	case request := <-httpsSeen:
		want := "contained-flow.test|contained-flow.test|/" + nonce
		if request != want {
			t.Fatalf("contained HTTPS request = %q, want %q", request, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("contained HTTPS request did not reach exact outer endpoint")
	}
	select {
	case request := <-non443Seen:
		t.Fatalf("contained non-443 request escaped to outer endpoint: %q", request)
	default:
	}
	waitContainedFlowContains(t, filepath.Join(flowDir, "mayor.input"), nonce+"-immediate", 3*time.Second)
	queued, err := nudge.Drain(townRoot, mayorSession)
	if err != nil {
		t.Fatalf("drain contained Mayor queue: %v", err)
	}
	if len(queued) != 1 || queued[0].Message != nonce+"-queued" {
		t.Fatalf("contained Mayor queue = %#v, want exactly %q", queued, nonce+"-queued")
	}
	if pending, err := nudge.Pending(townRoot, mayorSession); err != nil || pending != 0 {
		t.Fatalf("contained Mayor pending after drain = %d, %v", pending, err)
	}
	findContainedFlowMail(t, router, "mayor/", "contained outbound "+nonce)
	reportedPatrol, err := bd.Show(oldPatrolID)
	if err != nil {
		t.Fatalf("show reported contained patrol %s: %v", oldPatrolID, err)
	}
	if reportedPatrol.Status != "closed" || !strings.Contains(reportedPatrol.Description, nonce) {
		t.Fatalf("reported contained patrol = status %q description %q", reportedPatrol.Status, reportedPatrol.Description)
	}
	openIssues, err := bd.List(beads.ListOptions{Status: string(beads.StatusOpen), Priority: -1})
	if err != nil {
		t.Fatalf("list disposable town after contained direct-bd denial: %v", err)
	}
	for _, issue := range openIssues {
		if strings.Contains(issue.Title, nonce+" bd custody") {
			t.Fatalf("contained direct bd mutation escaped the deny endpoint: %#v", issue)
		}
	}
	newPatrolID := findContainedFlowPatrol(t, bd, constants.MolWitnessPatrol, rigName+"/witness")
	if newPatrolID == oldPatrolID {
		t.Fatalf("patrol report did not rotate root %s", oldPatrolID)
	}

	confirmedGeneration, err := transport.CaptureSessionGeneration(mgr.SessionName())
	if err != nil {
		t.Fatalf("capture contained Witness generation: %v", err)
	}
	if !confirmedGeneration.Equal(generation) {
		t.Fatalf("contained Witness generation changed during flow: got %#v want %#v", confirmedGeneration, generation)
	}
	trackedPIDs := assertContainedFlowProcessTree(t, transport, generation, supervisorStderrPath)
	for index := range trackedPIDs {
		if trackedPIDs[index] != initialTrackedPIDs[index] {
			t.Fatalf("contained process tree changed during flow: got %v want %v", trackedPIDs, initialTrackedPIDs)
		}
	}
	if err := mgr.Stop(); err != nil {
		t.Fatalf("first contained Witness Stop: %v", err)
	}
	stopped = true
	if running, err := transport.HasSession(mgr.SessionName()); err != nil && !errors.Is(err, tmux.ErrNoServer) {
		t.Fatalf("check contained Witness after Stop: %v", err)
	} else if running {
		t.Fatal("contained Witness session survived first Stop")
	}
	pollerPath := filepath.Join(townRoot, ".runtime", "nudge_poller", mgr.SessionName()+".pid")
	if _, err := os.Stat(pollerPath); !os.IsNotExist(err) {
		t.Fatalf("contained Witness poller ownership survived Stop: %v", err)
	}
	waitContainedFlowProcessesGone(t, trackedPIDs, 3*time.Second)
	if running, err := transport.HasSession(mayorSession); err != nil || !running {
		t.Fatalf("contained Witness Stop disturbed private Mayor target: running=%v err=%v", running, err)
	}
}

func readContainedFlowOptional(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "<missing: " + err.Error() + ">"
	}
	return string(data)
}

type containedFlowResult struct {
	Code   int
	Stdout string
	Stderr string
}

func TestParseContainedFlowResults(t *testing.T) {
	output := strings.Join([]string{
		"noise",
		"GT_FLOW_BEGIN:prime:0",
		"stdout line",
		"GT_FLOW_STDERR:prime",
		"stderr line",
		"GT_FLOW_END:prime",
		"GT_FLOW_DONE",
	}, "\n")
	result := containedFlowResultNamed(t, parseContainedFlowResults(t, output), "prime")
	if result.Code != 0 || result.Stdout != "stdout line" || result.Stderr != "stderr line" {
		t.Fatalf("parsed contained result = %#v", result)
	}
}

func waitContainedFlowPane(t *testing.T, transport *tmux.Tmux, session, needle string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var output string
	for time.Now().Before(deadline) {
		captured, err := transport.CapturePaneAll(session)
		if err == nil {
			output = captured
			if strings.Contains(output, needle) {
				return output
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("contained pane did not emit %q: %q", needle, output)
	return ""
}

func parseContainedFlowResults(t *testing.T, output string) map[string]containedFlowResult {
	t.Helper()
	results := make(map[string]containedFlowResult)
	var name string
	var code int
	var stdout, stderr strings.Builder
	inStderr := false
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "GT_FLOW_BEGIN:") {
			parts := strings.Split(line, ":")
			if len(parts) != 3 {
				t.Fatalf("malformed contained flow begin marker %q", line)
			}
			name = parts[1]
			var err error
			code, err = strconv.Atoi(parts[2])
			if err != nil {
				t.Fatalf("malformed contained flow code %q: %v", line, err)
			}
			stdout.Reset()
			stderr.Reset()
			inStderr = false
			continue
		}
		if name == "" {
			continue
		}
		if line == "GT_FLOW_STDERR:"+name {
			inStderr = true
			continue
		}
		if line == "GT_FLOW_END:"+name {
			results[name] = containedFlowResult{Code: code, Stdout: strings.TrimSpace(stdout.String()), Stderr: strings.TrimSpace(stderr.String())}
			name = ""
			continue
		}
		if inStderr {
			stderr.WriteString(raw)
			stderr.WriteByte('\n')
		} else {
			stdout.WriteString(raw)
			stdout.WriteByte('\n')
		}
	}
	return results
}

func containedFlowResultNamed(t *testing.T, results map[string]containedFlowResult, name string) containedFlowResult {
	t.Helper()
	result, ok := results[name]
	if !ok {
		t.Fatalf("contained flow omitted result %q", name)
	}
	return result
}

func assertContainedFlowExit(t *testing.T, results map[string]containedFlowResult, name string, wantSuccess bool) {
	t.Helper()
	result := containedFlowResultNamed(t, results, name)
	if wantSuccess && result.Code != 0 || !wantSuccess && result.Code == 0 {
		t.Fatalf("contained %s exit code = %d, want success=%v; stdout=%q stderr=%q",
			name, result.Code, wantSuccess, result.Stdout, result.Stderr)
	}
}

func assertContainedFlowBrokerDenial(t *testing.T, results map[string]containedFlowResult, name string) {
	t.Helper()
	assertContainedFlowDenial(t, results, name, 126, "not broker-safe")
}

func assertContainedFlowDenial(t *testing.T, results map[string]containedFlowResult, name string, wantCode int, wantStderr string) {
	t.Helper()
	result := containedFlowResultNamed(t, results, name)
	if result.Code != wantCode || !strings.Contains(result.Stderr, wantStderr) {
		t.Fatalf("contained %s did not hit the expected denial: code=%d want=%d stderr=%q want substring=%q", name, result.Code, wantCode, result.Stderr, wantStderr)
	}
}

func waitContainedFlowContains(t *testing.T, path, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), needle) {
			return
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read contained-flow evidence %s: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("contained-flow evidence %s did not contain %q: %q", path, needle, readContainedFlowOptional(path))
}

func recordContainedFlowSubmission(townRoot, session, inputPath string, timeout time.Duration) <-chan error {
	result := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(inputPath)
			if err == nil && strings.Contains(string(data), "[gt-delivery-id:") {
				recorded, recordErr := delivery.RecordPromptSubmitted(townRoot, session, "codex", string(data), time.Now())
				if recordErr != nil {
					result <- recordErr
					return
				}
				if !recorded {
					result <- errors.New("captured target input lacked a valid delivery control message")
					return
				}
				result <- nil
				return
			}
			if err != nil && !os.IsNotExist(err) {
				result <- err
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		result <- errors.New("timed out waiting for contained target submission")
	}()
	return result
}

func findContainedFlowMail(t *testing.T, router *mail.Router, address, subject string) string {
	t.Helper()
	mailbox, err := router.GetMailbox(address)
	if err != nil {
		t.Fatalf("open contained mailbox %s: %v", address, err)
	}
	messages, err := mailbox.List()
	if err != nil {
		t.Fatalf("list contained mailbox %s: %v", address, err)
	}
	var found string
	for _, message := range messages {
		if message.Subject != subject {
			continue
		}
		if found != "" {
			t.Fatalf("contained mailbox %s has duplicate subject %q", address, subject)
		}
		found = message.ID
	}
	if found == "" {
		t.Fatalf("contained mailbox %s lacks subject %q", address, subject)
	}
	return found
}

func findContainedFlowPatrol(t *testing.T, bd *beads.Beads, molName, assignee string) string {
	t.Helper()
	issues, err := bd.List(beads.ListOptions{
		Status: beads.StatusHooked, Assignee: assignee, Ephemeral: true, Priority: -1,
	})
	if err != nil {
		t.Fatalf("list contained patrol roots: %v", err)
	}
	var found string
	for _, issue := range issues {
		if !strings.HasPrefix(issue.Title, molName) {
			continue
		}
		if found != "" {
			t.Fatalf("duplicate contained patrol roots %s and %s", found, issue.ID)
		}
		found = issue.ID
	}
	if found == "" {
		t.Fatalf("no contained %s patrol root for %s", molName, assignee)
	}
	return found
}

func createContainedFlowPatrol(t *testing.T, bd *beads.Beads, molName, assignee string) string {
	t.Helper()
	root, err := bd.Create(beads.CreateOptions{
		Title: molName + " (wisp)", Priority: -1, Ephemeral: true, Actor: "witness",
	})
	if err != nil {
		t.Fatalf("create contained patrol root: %v", err)
	}
	hooked := beads.StatusHooked
	if err := bd.Update(root.ID, beads.UpdateOptions{Status: &hooked, Assignee: &assignee}); err != nil {
		t.Fatalf("hook contained patrol root: %v", err)
	}
	if _, err := bd.Create(beads.CreateOptions{
		Title: "inbox-check", Parent: root.ID, Priority: -1, Ephemeral: true, Actor: "witness",
	}); err != nil {
		t.Fatalf("create contained patrol child: %v", err)
	}
	if _, err := bd.Create(beads.CreateOptions{
		Title: "cleanup-check", Parent: root.ID, Priority: -1, Ephemeral: true, Actor: "witness",
	}); err != nil {
		t.Fatalf("create contained patrol continuation child: %v", err)
	}
	return root.ID
}

func requireContainedFlowPrivateIP(t *testing.T, host string) string {
	t.Helper()
	addresses, err := net.LookupHost(host)
	if err != nil {
		t.Fatalf("resolve configured disposable Dolt host %s: %v", host, err)
	}
	for _, address := range addresses {
		if parsed := net.ParseIP(address); parsed != nil && parsed.To4() != nil && !parsed.IsLoopback() {
			return address
		}
	}
	t.Fatalf("configured disposable Dolt host %s has no private IPv4 address: %v", host, addresses)
	return ""
}

func requireContainedFlowHTTPSHost(t *testing.T) {
	t.Helper()
	addresses, err := net.LookupHost("contained-flow.test")
	if err == nil {
		for _, address := range addresses {
			if address == "1.1.1.2" {
				return
			}
		}
	}
	t.Fatalf("contained-flow.test must resolve to 1.1.1.2 in the disposable runner (Docker: --add-host contained-flow.test:1.1.1.2); got %v (%v)", addresses, err)
}

func startContainedFlowHTTPS(t *testing.T, flowDir, nonce string) (string, <-chan string) {
	t.Helper()
	now := time.Now()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "contained-flow-ca"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "contained-flow.test"},
		DNSNames: []string{"contained-flow.test"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(flowDir, "contained-flow-ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)}),
	)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "1.1.1.2:443")
	if err != nil {
		t.Fatalf("listen on contained public HTTPS fixture: %v", err)
	}
	seen := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		sni := ""
		if request.TLS != nil {
			sni = request.TLS.ServerName
		}
		select {
		case seen <- request.Host + "|" + sni + "|" + request.URL.Path:
		default:
		}
		_, _ = fmt.Fprint(w, nonce)
	})}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	go func() { _ = server.Serve(tlsListener) }()
	t.Cleanup(func() { _ = server.Close() })
	return caPath, seen
}

func startContainedFlowHTTP(t *testing.T, nonce string) <-chan string {
	t.Helper()
	listener, err := net.Listen("tcp4", "1.1.1.2:80")
	if err != nil {
		t.Fatalf("listen on contained public non-443 fixture: %v", err)
	}
	seen := make(chan string, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		select {
		case seen <- request.URL.Path:
		default:
		}
		_, _ = fmt.Fprint(w, nonce)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	response, err := client.Get("http://1.1.1.2/outer-probe")
	if err != nil {
		t.Fatalf("verify contained public non-443 fixture: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || string(body) != nonce {
		t.Fatalf("verify contained public non-443 fixture: status=%d body=%q read=%v close=%v", response.StatusCode, body, readErr, closeErr)
	}
	select {
	case request := <-seen:
		if request != "/outer-probe" {
			t.Fatalf("contained public non-443 outer probe = %q", request)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("contained public non-443 fixture did not observe outer probe")
	}
	return seen
}

func assertContainedFlowProcessTree(t *testing.T, transport *tmux.Tmux, generation tmux.SessionGeneration, supervisorStderrPath string) []int {
	t.Helper()
	pane, err := transport.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatalf("capture contained supervisor process: %v", err)
	}
	var children []int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		children = containedFlowChildren(t, pane.PID)
		if len(children) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(children) != 1 {
		paneOutput, captureErr := transport.CapturePaneAll(generation.Name)
		t.Fatalf("contained supervisor %d children = %v, want only trusted init; pane output=%q capture error=%v supervisor stderr=%q", pane.PID, children, paneOutput, captureErr, readContainedFlowOptional(supervisorStderrPath))
	}
	initPID := children[0]
	if got := containedFlowNamespacePID(t, initPID); got != 1 {
		t.Fatalf("contained trusted init outer PID %d has namespace PID %d, want 1", initPID, got)
	}
	workloads := containedFlowChildren(t, initPID)
	if len(workloads) != 1 {
		t.Fatalf("contained trusted init %d children = %v, want one workload", initPID, workloads)
	}
	// PID allocation is shared with the trusted init's Go runtime threads, so
	// the first child process is not necessarily namespace PID 2. Its direct
	// parent and sole-child relationship prove custody; its namespace PID only
	// needs to be a non-init value.
	if got := containedFlowNamespacePID(t, workloads[0]); got <= 1 {
		t.Fatalf("contained workload outer PID %d has namespace PID %d, want greater than 1", workloads[0], got)
	}
	return []int{pane.PID, initPID, workloads[0]}
}

func containedFlowChildren(t *testing.T, pid int) []int {
	t.Helper()
	taskRoot := fmt.Sprintf("/proc/%d/task", pid)
	tasks, err := os.ReadDir(taskRoot)
	if err != nil {
		t.Fatalf("read tasks for contained PID %d: %v", pid, err)
	}
	childrenByPID := make(map[int]struct{})
	for _, task := range tasks {
		tid, err := strconv.Atoi(task.Name())
		if err != nil || tid <= 0 {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("%s/%d/children", taskRoot, tid))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read children for contained PID %d thread %d: %v", pid, tid, err)
		}
		for _, field := range strings.Fields(string(data)) {
			child, err := strconv.Atoi(field)
			if err != nil || child <= 0 {
				t.Fatalf("parse contained child PID %q for %d: %v", field, pid, err)
			}
			childrenByPID[child] = struct{}{}
		}
	}
	result := make([]int, 0, len(childrenByPID))
	for child := range childrenByPID {
		result = append(result, child)
	}
	sort.Ints(result)
	return result
}

func containedFlowNamespacePID(t *testing.T, pid int) int {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatalf("read status for contained PID %d: %v", pid, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(line)
		value, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			t.Fatalf("parse NSpid for contained PID %d: %v", pid, err)
		}
		return value
	}
	t.Fatalf("contained PID %d status lacks NSpid", pid)
	return 0
}

func waitContainedFlowProcessesGone(t *testing.T, pids []int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remaining := 0
		for _, pid := range pids {
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
				remaining++
			}
		}
		if remaining == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	var survivors []string
	for _, pid := range pids {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			continue
		}
		var fields []string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "State:") || strings.HasPrefix(line, "PPid:") || strings.HasPrefix(line, "NSpid:") {
				fields = append(fields, strings.TrimSpace(line))
			}
		}
		survivors = append(survivors, fmt.Sprintf("%d{%s}", pid, strings.Join(fields, ", ")))
	}
	t.Fatalf("contained process generations survived Stop: %v", survivors)
}

func requireContainedFlowExecutable(t *testing.T, envKey string) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(envKey))
	if path == "" && envKey == "BD_PATH" {
		var err error
		path, err = exec.LookPath("bd")
		if err != nil {
			t.Fatalf("bd is required for contained flow: %v", err)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("%s does not name an executable: %q (%v)", envKey, path, err)
	}
	return path
}

func setupContainedFlowTown(t *testing.T, townRoot, rigName, rigPath, flowDir, agentPath, mayorAgentPath string) {
	t.Helper()
	for _, dir := range []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, "settings"),
		filepath.Join(rigPath, "settings"),
		filepath.Join(rigPath, "witness", ".git"),
		flowDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rigPath, "witness", ".git", "HEAD"), []byte("ref: refs/heads/contained-flow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeContainedFlowJSON(t, filepath.Join(townRoot, "mayor", "town.json"), map[string]any{
		"type": "town", "version": 2, "name": "contained-flow",
	})
	writeContainedFlowJSON(t, filepath.Join(townRoot, "mayor", "rigs.json"), map[string]any{
		"version": 1,
		"rigs": map[string]any{
			rigName: map[string]any{"git_url": "https://example.invalid/contained-flow.git", "added_at": time.Now().UTC()},
		},
	})
	writeContainedFlowJSON(t, filepath.Join(townRoot, "settings", "config.json"), map[string]any{
		"type": "town-settings", "version": 1, "default_agent": "claude",
	})
	writeContainedFlowJSON(t, filepath.Join(rigPath, "settings", "config.json"), map[string]any{
		"type": "rig-settings", "version": 1, "agent": "contained-flow",
		"agents": map[string]any{
			"contained-flow": map[string]any{
				"command": agentPath, "prompt_mode": "none", "process_names": []string{"sh"},
				"env": map[string]string{"GT_TEST_FLOW_PRESET_ENV": "contained-preset-env"},
			},
		},
	})
	mayorAgent := `#!/bin/sh
umask 077
printf '› '
while IFS= read -r line; do
  printf '%s\n' "$line" >>"$GT_TEST_FLOW_DIR/mayor.input"
  printf 'received:%s\n› ' "$line"
done
`
	if err := os.WriteFile(mayorAgentPath, []byte(mayorAgent), 0o700); err != nil {
		t.Fatal(err)
	}
}

type containedFlowAgentFixture struct {
	GT              string
	BD              string
	MailID          string
	Nonce           string
	Socket          string
	CA              string
	PrivateDoltIP   string
	PrivateDoltPort int
	OuterSentinel   string
}

func writeContainedFlowAgent(t *testing.T, path string, fixture containedFlowAgentFixture) {
	t.Helper()
	bindings := strings.Join([]string{
		"flow_gt=" + config.ShellQuote(fixture.GT),
		"flow_bd=" + config.ShellQuote(fixture.BD),
		"flow_mail_id=" + config.ShellQuote(fixture.MailID),
		"flow_nonce=" + config.ShellQuote(fixture.Nonce),
		"flow_socket=" + config.ShellQuote(fixture.Socket),
		"flow_ca=" + config.ShellQuote(fixture.CA),
		"flow_private_dolt_ip=" + config.ShellQuote(fixture.PrivateDoltIP),
		"flow_private_dolt_port=" + config.ShellQuote(strconv.Itoa(fixture.PrivateDoltPort)),
		"flow_outer_sentinel=" + config.ShellQuote(fixture.OuterSentinel),
	}, "\n")
	agent := `#!/bin/sh
set -u
umask 077
` + bindings + `
run_flow() {
  name=$1
  shift
  stdout="$TMPDIR/$name.stdout"
  stderr="$TMPDIR/$name.stderr"
  "$@" >"$stdout" 2>"$stderr"
  code=$?
  printf '\nGT_FLOW_BEGIN:%s:%s\n' "$name" "$code"
  cat "$stdout"
  printf '\nGT_FLOW_STDERR:%s\n' "$name"
  cat "$stderr"
  printf '\nGT_FLOW_END:%s\n' "$name"
  rm -f "$stdout" "$stderr"
}
run_flow preset_env test -z "${GT_TEST_FLOW_PRESET_ENV:-}"
run_flow outer_env test -z "${GT_TEST_OUTER_SENTINEL:-}"
run_flow outer_file test ! -r "$flow_outer_sentinel"
run_flow device_null sh -c 'test -c /dev/null && printf ok >/dev/null'
run_flow device_full test ! -e /dev/full
run_flow private_pts python3 -c 'import os,sys; sys.exit(0 if sorted(os.listdir("/dev/pts")) == ["ptmx"] else 1)'
run_flow forbidden_families python3 -c 'import errno,socket,sys
for family,socktype in ((40,socket.SOCK_STREAM),(16,socket.SOCK_RAW),(17,socket.SOCK_RAW)):
    try:
        value=socket.socket(family,socktype,0)
    except OSError as error:
        if error.errno == errno.EPERM:
            continue
        print("unexpected errno",family,error.errno,file=sys.stderr)
        sys.exit(1)
    value.close()
    print("socket family allowed",family,file=sys.stderr)
    sys.exit(1)'
run_flow prime "$flow_gt" prime --hook
run_flow prime_hook_show "$flow_gt" hook show
run_flow prime_current "$flow_gt" mol current
run_flow prime_step_close "$flow_gt" mol step close
run_flow prime_after_close "$flow_gt" mol current
run_flow patrol_scan "$flow_gt" patrol scan --rig testrig --json
run_flow patrol_report "$flow_gt" patrol report --summary "$flow_nonce" --steps inbox-check:OK
run_flow mail_inbox "$flow_gt" mail inbox --unread
run_flow mail_read "$flow_gt" mail read "$flow_mail_id"
run_flow mail_send "$flow_gt" mail send mayor/ --subject "contained outbound $flow_nonce" --message "contained outbound body $flow_nonce" --no-notify
run_flow nudge_queue "$flow_gt" nudge mayor "$flow_nonce-queued" --mode queue
run_flow nudge_immediate "$flow_gt" nudge mayor "$flow_nonce-immediate" --mode immediate
run_flow hook "$flow_gt" hook
run_flow agents "$flow_gt" agents list
run_flow polecats "$flow_gt" polecat list testrig
run_flow status "$flow_gt" status --fast
run_flow bd_create "$flow_bd" create --title "$flow_nonce bd custody" --type task
run_flow bd_list "$flow_bd" list --status open --json
run_flow https curl --fail --silent --show-error --max-time 5 --cacert "$flow_ca" "https://contained-flow.test/$flow_nonce"
run_flow raw_tmux timeout 5 tmux -L "$flow_socket" list-sessions
run_flow host_loopback nc -z -w 1 127.0.0.1 22
run_flow private_dolt nc -z -w 1 "$flow_private_dolt_ip" "$flow_private_dolt_port"
run_flow public_non443 curl --fail --silent --show-error --max-time 3 "http://contained-flow.test/$flow_nonce"
run_flow formula "$flow_gt" formula list
run_flow hidden "$flow_gt" session-custody-init
run_flow shell "$flow_gt" shell -c true
run_flow env_bypass env -u GT_ROLE -u GT_RIG -u GT_AGENT "$flow_gt" shell -c true
run_flow descriptor_bypass python3 -c 'import fcntl,os,sys
out=[]
for name,operation in (("close",lambda: os.close(6)),("dup2",lambda: os.dup2(0,6)),("fcntl",lambda: fcntl.fcntl(6,fcntl.F_DUPFD,10))):
    try:
        result=operation()
        if name=="fcntl": os.close(result)
        out.append(name+"=allowed")
    except OSError as error:
        out.append(name+"="+error.strerror)
os.write(2,("\n".join(out)+"\n").encode())
os.execve(sys.argv[1],[sys.argv[1],"shell","-c","true"],os.environ)' "$flow_gt"
printf '\nGT_FLOW_DONE\n'
while :; do sleep 60; done
`
	if err := os.WriteFile(path, []byte(agent), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestWriteContainedFlowAgentUsesConfiguredPrivateDoltPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent")
	writeContainedFlowAgent(t, path, containedFlowAgentFixture{
		GT: "/witness/gt", BD: "/usr/bin/bd", MailID: "mail-id", Nonce: "nonce",
		Socket: "socket", CA: "/witness/ca.pem", PrivateDoltIP: "10.20.30.40",
		PrivateDoltPort: 15432, OuterSentinel: "/outer/sentinel",
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{
		"flow_private_dolt_port=15432",
		`run_flow private_dolt nc -z -w 1 "$flow_private_dolt_ip" "$flow_private_dolt_port"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("contained agent omitted configured private Dolt port %q:\n%s", want, script)
		}
	}
}

func copyContainedFlowExecutable(t *testing.T, source, target string) {
	t.Helper()
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeContainedFlowJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitContainedFlowFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read contained-flow evidence %s: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for contained-flow evidence %s", path)
	return ""
}

func dropContainedFlowDatabase(t *testing.T, host string, port int, database string) {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(%s:%d)/", host, port))
	if err != nil {
		t.Errorf("open disposable Dolt cleanup connection: %v", err)
		return
	}
	defer db.Close()
	if _, err := db.Exec("DROP DATABASE IF EXISTS `" + strings.ReplaceAll(database, "`", "``") + "`"); err != nil {
		t.Errorf("drop disposable database %s: %v", database, err)
		return
	}
	if _, err := db.Exec("CALL dolt_purge_dropped_databases()"); err != nil {
		t.Errorf("purge disposable Dolt database %s: %v", database, err)
	}
}
