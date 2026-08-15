# Polecat cleanup control-plane defects

Status: open

This note records two control-plane defects observed during narrow polecat
recovery on 2026-08-16. Both affect whether routine cleanup can preserve exact
custody and publication policy. Until they are repaired, treat the workarounds
below as operational requirements.

## 1. Stale identity state can deadlock safe recovery

### Symptom

A stopped polecat can have all live recovery predicates in a safe state while
its durable agent bead still reports stale metadata:

- the session is stopped;
- the hook is empty or its work is terminal;
- the worktree is clean and has no stash;
- the branch has no unpreserved patch or unpushed commit; and
- the branch tip is already contained in the authoritative branch.

Despite those facts, `gt polecat check-recovery --reconcile-cleanup` can leave
the polecat at `NEEDS_RECOVERY` when the agent bead contains both
`agent_state=stuck` and a dirty `cleanup_status`, such as `has_unpushed`.
`gt polecat nuke --dry-run` then refuses the cleanup, as it should when the
precondition has not become `SAFE_TO_NUKE`.

### Cause

The recovery evaluator can ignore a stale dirty `cleanup_status` after it has
independently proved terminal work, a safe hook, no pending merge request, and
safe Git state. The later persistence step is stricter:
`cleanupStatusReconcileCandidate` in `internal/cmd/polecat.go` requires both the
derived polecat state and the stored agent-bead state to be `idle` before it
will rewrite the stale cleanup status.

That makes the repair path depend on the stale field it needs to recover from.
The codebase has an internal `TransitionPolecatToIdle` helper, but the observed
ordinary recovery workflow exposed no generation-safe command that could
correct this durable state without bypassing custody checks.

### Consequence

Safe cleanup becomes a deadlock: force cleanup would discard the safety gate,
while ordinary cleanup cannot make the metadata transition required to pass
the gate. The correct result is leaked but preserved worktrees, branches, and
identity records until a narrow repair exists.

### Required operator behavior

- Preserve the polecat when `check-recovery` remains `NEEDS_RECOVERY`, even if
  independent Git inspection appears clean.
- Do not use `--force`, edit Dolt directly, or restart broad Gas Town services
  to make the record disappear.
- Record exact live custody evidence and leave any unique rejected object under
  an explicit recovery ref.
- Escalate the stale identity record for a generation-bound metadata repair.

### Repair acceptance criteria

- Recovery can atomically reconcile stale `agent_state` and `cleanup_status`
  only after rechecking session generation, hook ownership, work terminality,
  active merge-request state, worktree/stash state, and branch preservation.
- A superseding polecat generation cannot be modified through a stale name.
- Failure between validation and persistence leaves the record conservative
  and retryable.
- Tests reproduce `agent_state=stuck` plus stale `cleanup_status` with safe live
  predicates, as well as generation replacement during reconciliation.

## 2. `gt polecat nuke` can push implicitly

### Symptom

After a polecat passed `SAFE_TO_NUKE`, the dry run previewed local retirement.
The actual `gt polecat nuke` invocation then created or updated the polecat's
remote branch before deleting its local worktree, branch, and identity.

### Cause

`nukePolecatFullWithOptions` in `internal/cmd/polecat.go` contains a
"best-effort push before nuke" step. It pushes the branch to `origin` and
continues even if the push fails. The current dry-run output and cleanup command
reference do not expose that remote mutation as part of the operation.

### Consequence

`nuke` crosses a publication boundary. In the observed case, the pushed object
was already contained in the authoritative branch, so main did not move and no
unique content was published. In another case, the same behavior could publish
local-only or unreviewed commits despite a no-push campaign policy. It can also
leave an unwanted remote branch that requires separate authorization to delete.

### Required operator behavior

- Treat `gt polecat nuke` as remote-mutating until this defect is repaired.
- Do not run it in a local-only or no-push campaign unless an external guard
  provably rejects the push.
- Compare remote refs before and after any authorized cleanup.
- Do not delete an unexpectedly created remote ref without separate authority.

### Repair acceptance criteria

- Routine cleanup never pushes implicitly.
- Any future publish option is explicit, separately authorized, and blocked by
  repository or campaign no-push policy before network activity begins.
- Dry-run output includes every prospective local and remote mutation.
- Tests prove that ordinary `nuke` cannot create or update a remote ref and that
  push rejection does not weaken local custody checks.

## Relationship between the defects

These defects pull in opposite unsafe directions. The stale identity deadlock
prevents a provably clean polecat from retiring, while implicit push makes a
successful retirement broader than the operator authorized. A complete repair
must preserve the conservative recovery gate without coupling local cleanup to
remote publication.
