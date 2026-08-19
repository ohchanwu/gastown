# Lifecycle Custody and Truthful Closeout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> `superpowers:subagent-driven-development` (recommended) or
> `superpowers:executing-plans` to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make dog lifecycle decisions generation-bound and make Reaper report
auto-close success only after SQL and Dolt commit proof.

**Architecture:** Existing dog sessions use their persisted tmux generation and
transport as authority. Generation-free startup requires durable absence proof
plus the non-serialized capability returned by the atomic assignment; legacy
state without that proof remains recovery-blocked. Auto-close changes remain
provisional until both commit layers succeed, and a connection whose transaction
cleanup cannot be proven is discarded before it can return to the pool. Daemon
and CLI callers turn commit uncertainty into a failed lifecycle step and zero
success count.

**Tech Stack:** Go, `database/sql`, Dolt SQL, tmux generation receipts, Go tests.

**Spec:** `docs/operations/agents/260816-polecat-cleanup-control-plane-defects.md`

## Global Constraints

- Continue from exact candidate `bf0de1f0acf8b58ea2755932ac689cda04c8c8e9`
  on `repair-260801-integrated-control-plane`.
- Preserve foreign stash `4eb5f11328a4ba75ddb7340f49905cd1479b7bab`.
- Use disposable fixtures only; do not mutate production Dolt, live identities,
  Jobcron, unrelated worktrees, or live agent sessions.
- Do not push, install, deploy, run `gt done`, or run broad restart/cleanup.
- Keep ordinary success counts at zero until commit proof is complete.

---

### Task 1: Finish generation-bound dog lifecycle authority

**Files:**

- Modify: `internal/dog/session_manager.go`
- Modify: `internal/dog/manager.go`
- Modify: `internal/dog/types.go`
- Modify: `internal/dog/health.go`
- Modify: `internal/cmd/dog.go`
- Modify: `internal/cmd/sling_dog.go`
- Modify: `internal/daemon/handler.go`
- Test: `internal/dog/session_manager_test.go`
- Test: `internal/dog/manager_test.go`
- Test: `internal/dog/manager_lifecycle_test.go`
- Test: `internal/dog/types_test.go`
- Test: `internal/dog/health_test.go`
- Test: `internal/cmd/dog_test.go`
- Test: `internal/cmd/sling_dog_cleanup_test.go`
- Test: `internal/daemon/handler_test.go`

**Interfaces:**

- Produces: `dog.ErrSessionGenerationUnavailable`, returned whenever a
  destructive or absence-based decision has no persisted generation.
- Produces: persisted-generation controller selection for `Start`,
  `EnsureRunning`, stop, health, removal, and daemon cleanup.
- Produces: durable `SessionAbsenceProven` state and a non-serialized
  `AssignmentStartReceipt` issued only by `AssignWorkIfIdle`.

- [x] **Step 1: Preserve and review the inherited patch**

Confirm the patch changes only the eight files above and leaves the exact HEAD
and foreign stash unchanged:

```bash
git status --short --branch
git diff --check
git stash list --format='%gd %H %s'
```

- [x] **Step 2: Run the behavior-specific regressions**

```bash
CGO_ENABLED=0 go test ./internal/dog \
  -run 'Test(Health_IdleLegacyDogRequiresRecovery|HealthPreservesLegacyAssignmentWhenAmbientEndpointIsAbsent|DogStopIfMatchesPreservesLegacyAssignmentWhenAmbientEndpointIsAbsent)$' \
  -count=1
CGO_ENABLED=0 go test ./internal/cmd \
  -run '^TestRemoveDogExactRejectsAmbientAbsenceForLegacySession$' -count=1
CGO_ENABLED=0 go test ./internal/daemon \
  -run '^TestCleanupStuckDogsUsesPersistedEndpointBeforeExactTeardown$' -count=1
```

Expected: all pass; each test catches release of legacy custody or use of the
ambient tmux root in place of the persisted endpoint.

- [x] **Step 3: Keep the minimal production behavior**

The implementation must return `ErrSessionGenerationUnavailable` when neither
exact generation custody nor durable absence proof exists, reconstruct
`SessionGeneration.Tmux()` before liveness checks and replacement, and route
daemon cleanup through exact lifecycle state. Fresh startup consumes the
absence proof and requires the in-memory assignment capability. Do not add a
legacy name-based recovery path.

### Task 2: Reject unproven Reaper commits

**Files:**

- Modify: `internal/reaper/reaper.go`
- Test: `internal/reaper/reaper_test.go`

**Interfaces:**

- Produces: a non-nil outcome-unknown error for failed SQL `COMMIT` or
  non-benign `DOLT_COMMIT`.
- Produces: `AutoCloseResult.Closed == 0` and no `ClosedEntries` on every commit
  error.

- [x] **Step 1: Add fake-driver failure controls and failing tests**

Add one-shot fake-driver errors for SQL commit, Dolt commit, rollback, and
autocommit reset. Add these tests:

```go
func TestAutoCloseSQLCommitFailureReturnsNoSuccess(t *testing.T)
func TestAutoCloseDoltCommitFailureReturnsNoSuccess(t *testing.T)
func TestAutoCloseDiscardsConnectionWhenTransactionResetFails(t *testing.T)
```

The first two must assert a non-nil outcome-unknown error,
`result.Closed == 0`, and empty `result.ClosedEntries`. The third must force
commit plus rollback/reset failure, then prove the next database operation opens
a new fake connection.

- [x] **Step 2: Run the tests and observe RED**

```bash
CGO_ENABLED=0 go test ./internal/reaper \
  -run '^TestAutoClose(SQLCommitFailureReturnsNoSuccess|DoltCommitFailureReturnsNoSuccess|DiscardsConnectionWhenTransactionResetFails)$' \
  -count=1
```

Expected: fail because current `AutoClose` returns `result, nil` with provisional
success and can return a dirty connection to the pool.

- [x] **Step 3: Publish results only after commit proof**

Keep updated IDs and entries in local slices. Assign them to `AutoCloseResult`
only after SQL `COMMIT` and successful or benign-empty `DOLT_COMMIT`. Return a
wrapped `ErrAutoCloseCommitOutcomeUnknown` for either non-benign commit error.

- [x] **Step 4: Discard connections after failed rollback/reset**

Use `sql.Conn.Raw` with `driver.ErrBadConn` when deferred rollback or
`SET @@autocommit = 1` fails, then close the connection. Join cleanup failure
with the primary error and clear any success fields before returning.

- [x] **Step 5: Run the focused tests and observe GREEN**

Run the Step 2 command again. Expected: pass.

### Task 3: Make callers fail closed

**Files:**

- Modify: `internal/daemon/wisp_reaper.go`
- Test: `internal/daemon/wisp_reaper_test.go`
- Modify: `internal/cmd/reaper.go`
- Test: `internal/cmd/reaper_test.go`

**Interfaces:**

- Consumes: non-nil `AutoClose` commit error with zero ordinary success.
- Produces: daemon `auto-close` step failure and non-nil CLI command error.

- [x] **Step 1: Add caller regressions**

Add a daemon outcome test proving an `AutoCloseResult{Closed: 1}` accompanied by
an error contributes zero to `totalAutoClosed` and increments the failure count.
Add a CLI command test with an injected single-database auto-close function that
  returns an outcome-unknown error; assert `RunE` returns that error and
does not append or print an ordinary success result.

- [x] **Step 2: Run the caller tests and observe RED**

```bash
CGO_ENABLED=0 go test ./internal/daemon -run '^TestAutoCloseCommitErrorIsNotCounted$' -count=1
CGO_ENABLED=0 go test ./internal/cmd -run '^TestReaperAutoCloseCommandReturnsCommitError$' -count=1
```

- [x] **Step 3: Implement the smallest caller changes**

Keep the daemon's existing `mol.failStep("auto-close", ...)` path, but route
counting through a tested outcome classifier. In the CLI, accumulate per-database
errors and return `errors.Join(...)` after rendering only committed results.

- [x] **Step 4: Run the caller tests and observe GREEN**

Run the Step 2 commands again. Expected: pass.

### Task 4: Verify, document, and hand off exact custody

**Files:**

- Modify: `docs/design/architecture.md`
- Move on completion:
  `docs/superpowers/plans/260818-lifecycle-custody-truthful-closeout.md`
  to `docs/superpowers/archive/260818-lifecycle-custody-truthful-closeout.md`
- Modify: `docs/superpowers/README.md`
- Create: `docs/superpowers/archive/AGENTS.md`

- [x] **Step 1: Run formatting and focused gates**

```bash
gofmt -w internal/reaper/reaper.go internal/reaper/reaper_test.go \
  internal/daemon/wisp_reaper.go internal/daemon/wisp_reaper_test.go \
  internal/cmd/reaper.go internal/cmd/reaper_test.go
git diff --check
CGO_ENABLED=0 go test ./internal/cmd ./internal/daemon ./internal/dog \
  ./internal/polecat ./internal/reaper ./internal/tmux ./internal/witness
```

- [x] **Step 2: Run race and repository gates**

```bash
CGO_CFLAGS='-I/opt/homebrew/opt/icu4c@78/include' \
CGO_LDFLAGS='-L/opt/homebrew/opt/icu4c@78/lib' \
go test -race ./internal/cmd ./internal/daemon ./internal/dog \
  ./internal/polecat ./internal/reaper ./internal/tmux ./internal/witness
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c ./internal/cmd \
  -o /tmp/gastown-cmd-windows.test.exe
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c ./internal/tmux \
  -o /tmp/gastown-tmux-linux.test
```

Run `TestAutoCloseConditionalUpdateRunsOnIsolatedDolt` only against a disposable
localhost Dolt server on a non-production port, with `GT_TEST_EXTERNAL_DOLT=1`
and `GT_TEST_ISOLATED=1`. Preserve any timeout as `UNVERIFIED`; do not substitute
partial output for success.

Verification boundary: Reaper and tmux passed under `-race`. The other five
packages could not link the CGO-only ICU wrapper because this arm64 host has no
arm64 ICU headers or libraries; the installed ICU 78 artifacts are x86_64.
Their race status is `UNVERIFIED`, not failed. All seven packages passed without
the race detector, and the clean serialized repository suite passed in full.
The external conditional-update test passed against a disposable Dolt listener
on port 44009, which the harness removed afterward.

Final rereview also closed three successor gaps: persisted-absent replacement
starts on the persisted transport, fresh generation-free startup requires an
in-memory assignment capability and consumes durable absence proof, and legacy
idle dogs cannot be dispatched, cleared, removed, or daemon-reaped from ambient
absence. The post-fix clean serialized repository suite, vet, build, and both
cross-compilation gates passed.

The exact-SHA rereview then found two closeout-reporting gaps. Full `reaper run`
now joins auto-close commit errors and reports an incomplete cycle instead of
returning success. Commit-outcome errors also carry their structured anomaly and
affected IDs across every caller boundary. SQL commit uncertainty records the
inspect-then-retry-or-commit decision, while a post-SQL `DOLT_COMMIT` failure
records a direct pending-working-set commit action and forbids replaying
auto-close.

A successor rereview found that the complete recovery record still crossed the
daemon-to-`bd` boundary as one unbounded `--reason` argument. Molecule failure
closeout now sends that record through `bd close --reason-file -` on stdin. A
multi-database regression transports more than 1 MiB of affected IDs while argv
stays constant-sized. If the durable close fails, the cleanup backstop preserves
the failed child and leaves the root molecule open instead of erasing the
missing-reason evidence.

- [x] **Step 3: Update maintained architecture and archive the plan**

Record generation-bound legacy behavior, commit-proof result publication, and
dirty-connection discard in `docs/design/architecture.md`. Move this completed
plan to the tracked archive, add archive reading rules, and update the index.

- [x] **Step 4: Run the documentation publication gate**

Inspect the complete staged diff, run the configured secret scanner (Gitleaks
when available), and manually check for credentials, personal data, and
unnecessary production-specific details.

- [x] **Step 5: Commit locally and request exact-SHA rereview**

Commit on `repair-260801-integrated-control-plane`, verify exact HEAD/parent,
clean status, ancestry, and unchanged stash, then send the exact SHA to
`gastown/witness`. Do not push or run `gt done`.
