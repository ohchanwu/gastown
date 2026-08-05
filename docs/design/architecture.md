# Gas Town Architecture

Technical architecture for Gas Town multi-agent workspace management.

## Two-Level Beads Architecture

Gas Town uses a two-level beads architecture to separate organizational coordination
from project implementation work.

| Level | Location | Prefix | Purpose |
|-------|----------|--------|---------|
| **Town** | `~/gt/.beads/` | `hq-*` | Cross-rig coordination, Mayor mail, agent identity |
| **Rig** | `<rig>/mayor/rig/.beads/` | project prefix | Implementation work, MRs, project issues |

### Town-Level Beads (`~/gt/.beads/`)

Organizational chain for cross-rig coordination:
- Mayor mail and messages
- Convoy coordination (batch work across rigs)
- Strategic issues and decisions
- **Town-level agent beads** (Mayor, Deacon)
- **Role definition beads** (global templates)

### Rig-Level Beads (`<rig>/mayor/rig/.beads/`)

Project chain for implementation work:
- Bugs, features, tasks for the project
- Merge requests and code reviews
- Project-specific molecules
- **Rig-level agent beads** (Witness, Refinery, Polecats)

## Agent Bead Storage

Agent beads track lifecycle state for each agent. Storage location depends on
the agent's scope.

| Agent Type | Scope | Bead Location | Bead ID Format |
|------------|-------|---------------|----------------|
| Mayor | Town | `~/gt/.beads/` | `hq-mayor` |
| Deacon | Town | `~/gt/.beads/` | `hq-deacon` |
| Boot | Town | `~/gt/.beads/` | `hq-boot` |
| Dogs | Town | `~/gt/.beads/` | `hq-dog-<name>` |
| Witness | Rig | `<rig>/.beads/` | `<prefix>-<rig>-witness` |
| Refinery | Rig | `<rig>/.beads/` | `<prefix>-<rig>-refinery` |
| Polecats | Rig | `<rig>/.beads/` | `<prefix>-<rig>-polecat-<name>` |
| Crew | Rig | `<rig>/.beads/` | `<prefix>-<rig>-crew-<name>` |

### Role Beads

Role beads are global templates stored in town beads with `hq-` prefix:
- `hq-mayor-role` - Mayor role definition
- `hq-deacon-role` - Deacon role definition
- `hq-boot-role` - Boot role definition
- `hq-witness-role` - Witness role definition
- `hq-refinery-role` - Refinery role definition
- `hq-polecat-role` - Polecat role definition
- `hq-crew-role` - Crew role definition
- `hq-dog-role` - Dog role definition

Each agent bead references its role bead via the `role_bead` field.

## Agent Taxonomy

### Town-Level Agents (Cross-Rig)

| Agent | Role | Persistence |
|-------|------|-------------|
| **Mayor** | Global coordinator, handles cross-rig communication and escalations | Persistent |
| **Deacon** | Daemon beacon — receives heartbeats, runs plugins and monitoring | Persistent |
| **Boot** | Deacon watchdog — spawned by daemon for triage decisions when Deacon is down | Ephemeral |
| **Dogs** | Long-running workers for cross-rig batch work | Variable |

### Rig-Level Agents (Per-Project)

| Agent | Role | Persistence |
|-------|------|-------------|
| **Witness** | Monitors polecat health, handles nudging and cleanup | Persistent |
| **Refinery** | Processes merge queue, runs verification | Persistent |
| **Polecats** | Workers with persistent identity, assigned to specific issues | Persistent identity, ephemeral sessions |
| **Crew** | Human workspaces — full git clones, user-managed lifecycle | Persistent |

## Directory Structure

```
~/gt/                           Town root
├── .beads/                     Town-level beads (hq-* prefix)
│   ├── metadata.json           Beads config (dolt_mode, dolt_database)
│   └── routes.jsonl            Prefix → rig routing table
├── .dolt-data/                 Centralized Dolt data directory
│   ├── hq/                     Town beads database (hq-* prefix)
│   ├── gastown/                Gastown rig database (gt-* prefix)
│   ├── beads/                  Beads rig database (bd-* prefix)
│   └── <other rigs>/           Per-rig databases
├── daemon/                     Daemon runtime state
│   ├── dolt-state.json         Dolt server state (pid, port, databases)
│   ├── dolt-server.log         Server log
│   └── dolt.pid                Server PID file
├── deacon/                     Deacon workspace
│   └── dogs/<name>/            Dog worker directories
├── mayor/                      Mayor agent home
│   ├── town.json               Town configuration
│   ├── rigs.json               Rig registry
│   ├── daemon.json             Daemon patrol config
│   └── accounts.json           Claude Code account management
├── settings/                   Town-level settings
│   ├── config.json             Town settings (agents, themes)
│   └── escalation.json         Escalation routes and contacts
├── directives/                 Town-level role directives (operator policy)
│   └── <role>.md               Markdown injected at prime time
├── formula-overlays/           Town-level formula overlays
│   └── <formula>.toml          TOML step overrides (replace/append/skip)
├── config/
│   └── messaging.json          Mail lists, queues, channels
└── <rig>/                      Project container (NOT a git clone)
    ├── config.json             Rig identity and beads prefix
    ├── directives/             Rig-level role directives (overrides town)
    │   └── <role>.md
    ├── formula-overlays/       Rig-level formula overlays (full precedence)
    │   └── <formula>.toml
    ├── mayor/rig/              Canonical clone (beads live here, NOT an agent)
    │   └── .beads/             Rig-level beads (redirected to Dolt)
    ├── refinery/               Refinery agent home
    │   └── rig/                Worktree from mayor/rig
    ├── witness/                Witness agent home (no clone)
    ├── crew/                   Crew parent
    │   └── <name>/             Human workspaces (full clones)
    └── polecats/               Polecats parent
        └── <name>/<rigname>/   Worker worktrees from mayor/rig
```

**Note**: No per-directory CLAUDE.md or AGENTS.md is created. Only `~/gt/CLAUDE.md`
(town-root identity anchor) exists on disk. Full context is injected by `gt prime`
via SessionStart hook.

### Worktree Architecture

Polecats and refinery are git worktrees, not full clones. This enables fast spawning
and shared object storage. The worktree base is `mayor/rig`:

```go
// From polecat/manager.go - worktrees are based on mayor/rig
git worktree add -b polecat/<name>-<timestamp> polecats/<name>
```

Crew workspaces (`crew/<name>/`) are full git clones for human developers who need
independent repos. Polecat sessions are ephemeral and benefit from worktree efficiency.

## Storage Layer: Dolt SQL Server

All beads data is stored in a single Dolt SQL Server process per town. There is
no embedded Dolt fallback — if the server is down, `bd` fails fast with a clear
error pointing to `gt dolt start`.

```
┌─────────────────────────────────┐
│  Dolt SQL Server (per town)     │
│  Port 3307, managed by daemon   │
│  Data: ~/gt/.dolt-data/         │
└──────────┬──────────────────────┘
           │ MySQL protocol
    ┌──────┼──────┬──────────┐
    │      │      │          │
  USE hq  USE gastown  USE beads  ...
```

Each rig database is a subdirectory under `.dolt-data/`. The daemon monitors
the server on every heartbeat and auto-restarts on crash.

Local Dolt listeners are discovered once and classified by port plus process
ownership evidence. Health, Doctor, `gt dolt kill-imposters`, and `gt down`
share that inventory: configured-port imposters and positively owned town/test
leaks are actionable, while unknown listeners are reported without signals.
`gt dolt kill-imposters` applies only to configured-port imposters and reports
other classes without action. Broader leak remediation previews first and only
signals new listeners with positive ownership evidence. An extant CWD is positive
test ownership only when its resolved path remains under the OS temp root and has
the exact `.ctx-mode-*` / Go `Test...<digits>` / numeric-temp / terminal
`.beads/dolt` shape. On macOS, `lsof` reports the live process's kernel-resolved
CWD; if that exact reported path has since been deleted, ENOENT plus the same
strict shape is accepted as positive evidence. This exception is limited to
provenance-marked `lsof` CWD evidence, not arbitrary path strings. Traversal,
extant symlink escapes, town paths, arbitrary temp paths, and lookalike names
remain unknown. Staged-convoy tests register per-test cleanup before
subprocesses and enforce a package-exit listener baseline.

The repository-wide isolated test launcher also records a mode-0600 listener
baseline before packages start. Its outer EXIT trap covers package failures and
timeouts, selecting only new listeners owned within that launcher's private
temporary root and revalidating PID, port, class, process-start identity, and
opaque ownership token before signaling. Concurrent runs, baseline,
canonical, unknown, non-test-owned, and changed-owner listeners remain
untouched, and cleanup failure overrides the suite status so retries never
silently accept an orphan from an earlier attempt.

`gt dolt cleanup-test-leaks` is the user-facing test-leak path. Preview is the
default and atomically writes a mode-0600 receipt under a mode-0700 `.runtime`
directory. Each private selector binds PID, port, class, and an opaque ownership
token derived from that positive owner evidence; `--apply`
revalidates all four before signaling. Paths and tokens are never rendered.
Unpreviewed and non-test listeners remain report-only.

## Control-Plane Delivery and Recovery

Mayor wake delivery is receipt-based, not pane-delta-based. Each submission has
an opaque delivery ID, and success requires a runtime receipt for the same
session and delivery ID that is strictly newer than the attempt baseline.
Same-session submission locks prevent overlapping writers. A failed or
unverified submission remains in the durable queue under a claim/ack contract;
only a matching submitted receipt permits acknowledgement and deletion.
Notification results preserve queued and failed identities so callers cannot
mistake partial delivery for success.

If pane or composer verification becomes ambiguous after text was typed, the
receipt verifier still performs its bounded lookup. A matching post-baseline
runtime receipt proves acceptance; without one, the original delivery error is
preserved. This prevents retrying a turn that the runtime already accepted.

Codex startup nudges use the same receipt proof before the legacy idle-prompt
check. A matching submitted receipt ends startup verification immediately, so
a fast turn that has already completed and returned to the prompt is not
mistaken for a lost nudge and sent again. Providers without receipt support
retain the bounded idle-prompt fallback.

The effective preset name is session identity, not provider capability. Gas
Town resolves the preset once, derives receipt and prompt behavior from its
provider, and stores the resolved ready prompt with the tmux session. A custom
alias backed by Codex therefore retains Codex delivery semantics; an unknown or
unsupported provider fails closed instead of being guessed from the alias.

Nudge leases canonicalize the town root before constructing their lock path or
comparing ownership. A caller that acquires a lease through a symlinked path
therefore reuses that lease when receipt verification resolves the same town to
its physical path; it cannot wait on its own cross-process lock. Missing roots,
failed canonicalization, and a symlink retargeted while a lease is active fail
closed instead of falling back to a path identity that can change later.

Durable mail storage and runtime wake delivery are separate guarantees. A mail
write can succeed while its notification remains queued. The router therefore
keeps unverified notifications retryable and uses the configured agent provider
for submission instead of inferring behavior from a session name. In
particular, text left in an interactive prompt composer is not a successful
Codex submission without the matching runtime receipt.

`gt nudge-canary --confirm-live` verifies this path with 20 receipt-confirmed
turns in a temporary town, isolated Mayor identity, and dedicated tmux socket.
The canary requires zero attached clients and sole lock ownership, waits for a
steady idle state between turns, and removes only artifacts it owns. A matching
runtime receipt proves prompt wake within the delivery bound; the exact nonce
response uses the existing full-turn startup bound so provider inference time
is not misclassified as a missed wake. Its hook
trust bypass is scoped to the isolated canary launch. The latest sanitized
result is written atomically to `.runtime/canary/control-plane.json` with mode
0600 and contributes to `gt health` and Doctor's control-plane verdict.

Changes to provider, session, or wake delivery behavior are accepted only after
the isolated normal/race gates, a clean 20-turn source canary, exact-candidate
installation, and three consecutive fresh end-to-end mail round trips. Each
round trip requires one original mail, one acknowledgment, matching submitted
receipts in both directions, and no duplicate accepted turn. A causal repair
restarts the consecutive count from run one.

The background nudge poller publishes a structured ownership record that binds
the PID, process-start identity, command, session, and a random per-launch
generation. Start and stop transitions share one lifecycle lock. Shutdown is a
generation-bound cooperative request: the poller observes a byte-exact request,
exits, and its owner removes custody only after revalidating the unchanged
record. `StopPoller` never turns a previously validated PID into a later numeric
signal, so process exit and PID reuse cannot redirect termination to an
unrelated process. Live legacy PID-only records fail closed and require an
explicit, identity-checked migration.

An external poller inherits the target transport under both
`GT_TOWN_SOCKET` and `GT_TMUX_SOCKET`. The first supports direct tmux-client
fallbacks, while the second survives the standard CLI registry initialization
that runs before the poller command. Keeping them identical prevents an
isolated poller from silently deriving a different town socket and exiting
without draining its queue.

Refinery stop and restart actions use the same manager as ordinary lifecycle
commands. The manager first proves whether the session exists, then retires the
poller before killing or replacing a present session. An unknown tmux state
fails before custody changes; a proven-absent session still retires its poller
before returning not-running.

Global convoy checking and stranded detection use bounded worker pools, a
per-run tracked-issue cache, deterministic output, and a 30-second command
deadline. Lookup uncertainty is isolated per convoy and fails closed: a convoy
can close only after every tracked issue was checked and found complete.
Reconciliation remains dry-run-first and never treats age alone as proof of
completion. Reaper anomaly occurrences are durably linked and deduplicated,
while convoy issue types and `gt:convoy` tracking records remain protected from
age-based closure.

Doctor treats an exact persisted polecat cleanup status of `preserved` as
intentional recovery evidence: it neither reports that work as stalled nor
offers to push it through `--fix`. Missing, malformed, and unknown statuses stay
fail-closed and continue to warn.

For write concurrency, all agents write directly to `main` using transaction
discipline (`BEGIN` / `DOLT_COMMIT` / `COMMIT` atomically). This eliminates
branch proliferation and ensures immediate cross-agent visibility.

See [dolt-storage.md](dolt-storage.md) for full details.

## Beads Routing

The `routes.jsonl` file maps issue ID prefixes to rig locations (relative to town root):

```jsonl
{"prefix":"hq-","path":"."}
{"prefix":"gt-","path":"gastown/mayor/rig"}
{"prefix":"bd-","path":"beads/mayor/rig"}
```

Routes point to `mayor/rig` because that's where the canonical `.beads/` lives.
This enables transparent cross-rig beads operations:

```bash
bd show hq-mayor    # Routes to town beads (~/.gt/.beads)
bd show gt-xyz      # Routes to gastown/mayor/rig/.beads
```

## Beads Redirects

Worktrees (polecats, refinery, crew) don't have their own beads databases. Instead,
they use a `.beads/redirect` file that points to the canonical beads location:

```
polecats/alpha/.beads/redirect → ../../mayor/rig/.beads
refinery/rig/.beads/redirect   → ../../mayor/rig/.beads
```

`ResolveBeadsDir()` follows redirect chains (max depth 3) with circular detection.
This ensures all agents in a rig share a single beads database via the Dolt server.

## Merge Queue: Batch-then-Bisect

The refinery processes MRs through a batch-then-bisect merge queue (Bors-style).
This is a core capability, not a pluggable strategy.

### How It Works

```
MRs waiting:  [A, B, C, D]
                    ↓
Batch:        Rebase A..D as a stack on main
                    ↓
Test tip:     Run tests on D (tip of stack)
                    ↓
If PASS:      Fast-forward merge all 4 → done
If FAIL:      Binary bisect → test B (midpoint)
                    ↓
              If B passes: C or D broke it → bisect [C,D]
              If B fails:  A or B broke it → bisect [A,B]
```

### Implementation Phases

| Phase | Bead | What | Status |
|-------|------|------|--------|
| 1: GatesParallel | gt-8b2i | Run test + lint concurrently per MR | In progress |
| 2: Batch-then-bisect | gt-i2vm | Bors-style batching with binary bisect | Blocked by Phase 1 |
| 3: Pre-verification | gt-lu84 | Polecats run tests before MR submission | Blocked by Phase 2 |

Gates (test command, lint, etc.) are pluggable. The batching strategy is core.

Design doc: produced by gt-yxx0 review.

## Polecat Lifecycle: Self-Managed Completion

Polecats manage their own lifecycle end-to-end. The Witness observes but does NOT
gate completion. This prevents the Witness from becoming a bottleneck.

### Polecat Completion Flow

```
Polecat finishes work
  → Push branch to remote
  → Submit MR (bd update --mr-ready)
  → Update bead status
  → Tear down worktree
  → Go idle (available for next assignment)
```

The Witness monitors for stuck/zombie polecats (no activity for extended period)
and nudges or escalates. It does NOT process completion — that's the polecat's job.

Design bead: gt-0wkk.

## Data Plane Lifecycle

All beads data flows through a six-stage lifecycle managed by Dogs:

```
CREATE → LIVE → CLOSE → DECAY → COMPACT → FLATTEN
  │        │       │        │        │          │
  Dolt   active   done   DELETE   REBASE     SQUASH
  commit  work    bead    rows    commits    all history
                         >7-30d  together   to 1 commit
```

Stages 1-3 are automated today. Stages 4-6 are being shipped via Dog automation
(gt-at0i Reaper DELETE, gt-l8dc Compactor REBASE, gt-emm4 Doctor gc).

See [dolt-storage.md](dolt-storage.md) for full details.

## Deployment Artifacts

Gas Town and Beads are distributed through multiple channels. Tag pushes (`v*`)
trigger GitHub Actions release workflows that build and publish everything.

### Gas Town (`gt`)

| Channel | Artifact | Trigger |
|---------|----------|---------|
| **GitHub Releases** | Platform binaries (darwin/linux/windows, amd64/arm64) + checksums | GoReleaser on tag push |
| **Homebrew** | `brew install steveyegge/gastown/gt` — formula auto-updated on release | `update-homebrew` job pushes to `steveyegge/homebrew-gastown` |
| **npm** | `npx @gastown/gt` — wrapper that downloads the correct binary | OIDC trusted publishing (no token) |
| **Local build** | `go build -o $(go env GOPATH)/bin/gt ./cmd/gt` | Manual |

### Beads (`bd`)

| Channel | Artifact | Trigger |
|---------|----------|---------|
| **GitHub Releases** | Platform binaries + checksums | GoReleaser on tag push |
| **Homebrew** | `brew install steveyegge/beads/bd` | `update-homebrew` job |
| **npm** | `npx @beads/bd` — wrapper that downloads the correct binary | OIDC trusted publishing (no token) |
| **PyPI** | `beads-mcp` — MCP server integration | `publish-pypi` job with `PYPI_API_TOKEN` secret |
| **Local build** | `go build -o $(go env GOPATH)/bin/bd ./cmd/bd` | Manual |

### npm Authentication

Both repos use **OIDC trusted publishing** — no `NPM_TOKEN` secret needed.
Authentication is handled by GitHub's OIDC provider. The workflow needs:

```yaml
permissions:
  id-token: write  # Required for npm trusted publishing
```

Configure on npmjs.com: Package Settings → Trusted Publishers → link to the
GitHub repo and `release.yml` workflow file.

### What the binary embeds

The Go binary is the primary distribution vehicle. It embeds:
- **Role templates** — Agent priming context, served by `gt prime`
- **Formula definitions** — Workflow molecules, served by `bd mol`
- **Doctor checks** — Health diagnostics, including migration checks
- **Default configs** — `daemon.json` lifecycle defaults, operational thresholds

This means upgrading the binary automatically propagates most fixes. Files that
are NOT embedded (and require `gt doctor` or `gt upgrade` to update):
- Town-root `CLAUDE.md` (created at `gt install` time)
- `daemon.json` patrol entries (created at install, extended by `EnsureLifecycleDefaults`)
- Claude Code hooks (`.claude/settings.json` managed sections)
- Dolt schema (migrations run on first `bd` command after upgrade)

## Role Directives and Formula Overlays

Operators can customize agent behavior at the town or rig level without
modifying the Go binary or embedded templates. This follows the property layer
model (rig > town > system) and the hooks override precedent.

### Role Directives

Per-role Markdown files injected during `gt prime`, after the role template but
before context files and handoff content. Operator policy that overrides formula
instructions where they conflict.

```
~/gt/directives/<role>.md              # Town-level (all rigs)
~/gt/<rig>/directives/<role>.md        # Rig-level
```

Both levels concatenate (rig content appears last and wins conflicts).
Implemented in `internal/config/directives.go` (`LoadRoleDirective`),
integrated via `outputRoleDirectives()` in `internal/cmd/prime_output.go`.

### Formula Overlays

Per-formula TOML files that modify individual steps. Applied post-parse before
rendering in `showFormulaStepsFull()`.

```
~/gt/formula-overlays/<formula>.toml   # Town-level
~/gt/<rig>/formula-overlays/<formula>.toml  # Rig-level (full precedence)
```

Rig-level overlays fully replace town-level (not merged). Three override modes:

| Mode | Effect |
|------|--------|
| `replace` | Swap the step description entirely |
| `append` | Add text after the existing step description |
| `skip` | Remove the step (dependents inherit its needs) |

Implemented in `internal/formula/overlay.go` (`LoadFormulaOverlay`,
`ApplyOverlays`). `gt doctor` validates overlay step IDs against current
formula definitions and can auto-fix stale references.

See [directives-and-overlays.md](directives-and-overlays.md) for the full
reference with examples and design rationale.

## See Also

- [dolt-storage.md](dolt-storage.md) - Dolt storage architecture
- [reference.md](../reference.md) - Command reference
- [directives-and-overlays.md](directives-and-overlays.md) - Directives and overlays reference
- [molecules.md](../concepts/molecules.md) - Workflow molecules
- [identity.md](../concepts/identity.md) - Agent identity and BD_ACTOR
