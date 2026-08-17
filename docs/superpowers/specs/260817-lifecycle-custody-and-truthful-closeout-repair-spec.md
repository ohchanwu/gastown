# Lifecycle Custody and Truthful Closeout Repair Specification

Status: Approved design; awaiting implementation plan

Created: 2026-08-17

Tracker: `gastown-2oo`

Implementation branch: `repair-260801-integrated-control-plane`

## Summary

Repair three reachable Gas Town lifecycle defects without broadening the prior
control-plane campaign:

1. Safe polecat recovery can remain blocked by stale persisted lifecycle fields
   after current evidence proves that the work is terminal and preserved.
2. Ordinary polecat retirement can perform an implicit network push even though
   publication was not requested.
3. Dog completion and status can disagree with the real runtime session, making
   safe closeout unreliable.

The repairs must be generation-bound, fail closed on identity drift, and report
the distinction between durable work custody and runtime session custody.

## Why This Matters

The defects affect operators, Mayor/Witness recovery workflows, the polecat
allocator, and dog-based automation. They are reachable in the current trusted
single-user deployment and can cause one of three concrete failures:

- preserved work remains permanently allocated and blocks later dispatch;
- a local cleanup command unexpectedly publishes a Git ref; or
- a dog is reported as closed or running when the exact runtime generation says
  otherwise.

Repair is timely because the Gas Town implementation campaign has resumed and a
fresh dispatch already encountered an unreclaimable stale polecat identity.

## Verified Current State

### Stale polecat recovery state

The recovery evaluator can prove terminal work, safe hook state, no pending merge
request, safe Git state, and preserved branch contents while the agent bead still
contains `agent_state=stuck` and a dirty `cleanup_status`.

`cleanupStatusReconcileCandidate` in `internal/cmd/polecat.go` currently refuses
to repair the cleanup field unless both the derived polecat state and persisted
agent state are already `idle`. This creates a circular dependency: the stale
persisted state prevents the reconciliation that would make the record truthful.
The operational evidence and current safe handling are documented in
`docs/operations/agents/260816-polecat-cleanup-control-plane-defects.md`.

### Implicit publication during polecat retirement

`nukePolecatFullWithOptions` in `internal/cmd/polecat.go` constructs a branch
refspec and calls `Push("origin", refspec, false)` before local deletion. The push
is best-effort, but it is still an external mutation that ordinary cleanup did
not request. A configured no-push remote happened to reject this behavior during
the observed recovery; that policy guard is not a substitute for correct command
semantics.

### Dog completion and session truth

The installed CLI has reproduced `gt dog done` and `gt dog done <name>` reaching
the generic `gt done` polecat-only rejection for a dog actor. The current source
also contains a dog-specific `runDogDone`, so the regression must be tested at the
real Cobra command-entry boundary rather than assumed from a direct unit call.

Dog state is persisted separately from tmux state. `runDogDone` clears work before
asynchronously killing the reusable `hq-dog-<name>` session name, and
`showDogStatus` independently probes that name. A name alone does not prove that
the session being stopped or reported is the generation that completed the work.
Observed recovery state has therefore disagreed with the real session lifecycle.

## Decision Principles

A Witness finding belongs in this campaign when it is reachable under the actual
deployment assumptions and can affect custody, integrity, availability, bounded
cleanup, or truthful lifecycle reporting.

A finding may remain deferred when it depends on a deliberately excluded naming
or trust configuration, has no demonstrated path in this deployment, and can be
isolated without weakening the reachable safety properties above.

These principles keep the campaign focused on operationally meaningful defects
without treating every theoretical capability boundary as an immediate blocker.

## Goals

1. Allow stale polecat lifecycle fields to be repaired only when fresh evidence
   proves that the exact generation is safe to retire.
2. Make ordinary polecat retirement local-only and free of implicit publication.
3. Make dog completion use its dog-specific path and stop only the generation
   that completed the assignment.
4. Make human and machine-readable status distinguish work state from session
   state and avoid unsupported claims.
5. Cover normal, failure, and identity-substitution paths with regression tests.

## Non-Goals and Deferred Work

The following are explicitly outside this campaign:

- mailbox identity aliases created by naming crew agents `mayor` or `deacon`;
- authorization hardening for direct mutation of foreign known mail IDs in the
  trusted local store;
- redesign of the previously approved cgroup, PID namespace, retained-cleanup,
  notification, or convoy-completion fixes;
- a new general-purpose branch publication feature for `polecat nuke`;
- broad session restart, restore, doctor-fix, Dolt mutation, or Jobcron resume;
- installation, deployment, remote push, or production cleanup during
  implementation and review.

## Proposed Design

### 1. Generation-bound polecat reconciliation

Recovery evaluation must produce a reconciliation proof tied to the exact
polecat generation, not only the reusable polecat name. The proof must include
the stable identifiers needed to detect replacement between evaluation and
persistence, including the agent bead and the runtime/session generation identity
available to the current control plane.

Immediately before persistence, reconciliation must recheck:

- generation identity and current session truth;
- hook ownership and terminal work state;
- active or pending merge-request state;
- worktree cleanliness and untracked files;
- relevant stash ownership;
- branch ancestry, patch preservation, and authoritative containment; and
- the expected previous persisted lifecycle values.

If every predicate still matches, one narrow store transition must update the
stale `agent_state` and `cleanup_status` together to the truthful safe state. The
transition must use transactional or compare-and-set semantics so that a
superseding generation cannot be modified through a stale name. If the store
cannot prove the preconditions at commit time, no lifecycle field is changed and
the result remains `NEEDS_RECOVERY` with a useful diagnostic.

A partial update is a failure. The command must not expose `SAFE_TO_NUKE` unless
the persisted record and freshly rechecked evidence agree.

### 2. Local-only polecat retirement

Remove branch publication from the ordinary nuke path. `gt polecat nuke` must not
contact a remote to create or update refs, including when cleanup succeeds.

Before deleting a local branch, the command must continue to prove that unique
work is preserved by the already-established recovery policy. If preservation
requires publication, nuke fails closed and tells the operator to use the
separate reconciliation or publication workflow. This campaign does not add a
new nuke flag that mixes publication back into retirement.

Dry-run must model all proposed local effects while producing no local mutation,
network mutation, mail, or session input. Real-run output must not say that a
remote branch is preserved unless the precondition was actually verified.

### 3. Dog-specific closeout with exact session custody

The public commands `gt dog done` and `gt dog done <name>` must always dispatch to
the dog-specific completion implementation for a valid dog actor. Tests must
exercise the compiled command tree with realistic `GT_ROLE`, `BD_ACTOR`, current
directory, and explicit-name variants so command-routing regressions are caught.

Closeout must capture the dog session's stable generation identity before it
changes durable work state. It may stop a session only after verifying that the
current runtime session still has that identity. Reuse of `hq-dog-<name>` by a
replacement generation must cause the stop to fail closed without killing the
replacement.

Durable assignment completion and runtime teardown are related but distinct
facts. If work is cleared but teardown fails or identity changes, the command
must report incomplete teardown rather than claim complete closeout. Retrying
must be idempotent: an already-idle dog with no matching live generation succeeds
without mutating an unrelated session.

### 4. Truthful dog status

Status output must report at least these independent facts:

- durable work state: `idle` or `working`;
- work assignment, when present; and
- runtime session state: `running`, `absent`, `stale`, or `unknown`.

`running` requires positive evidence for the expected session generation.
`absent` requires an authoritative no-session result. A tmux error is `unknown`,
not `absent` or `running`. A reusable name occupied by another generation is
`stale` relative to the dog record and must never be treated as the closeout
target.

Existing JSON fields should remain compatible. New status detail may be additive;
silent reinterpretation of an existing field is not acceptable.

## Failure Semantics

All lifecycle mutations fail closed:

- identity drift: preserve both generations and report the mismatched evidence;
- store precondition failure: leave persisted fields unchanged;
- Git ambiguity: preserve worktree, stash, branch, and recovery refs;
- remote access: ordinary nuke never attempts it;
- tmux lookup error: report session state as unknown and do not kill by name;
- teardown failure after durable completion: report incomplete teardown and keep
  enough identity evidence for a safe retry; and
- duplicate retry: produce the same safe state without duplicate delivery,
  mutation, or termination.

No error path authorizes `--force`, direct Dolt edits, a broad restart, or
destruction of unresolved recovery evidence.

## Required Tests

### Unit tests

- stale `agent_state` plus dirty `cleanup_status` reconciles when all proof
  predicates match;
- every missing or changed predicate prevents both lifecycle-field updates;
- a store failure cannot leave only one field updated;
- nuke contains no push call and reports preservation truthfully;
- dog command routing accepts dog actor identities and rejects invalid actors;
- dog status maps positive, absent, stale, and error results correctly; and
- completion retry is idempotent.

### Integration tests

Use isolated repositories, fake remotes, disposable beads databases, and private
tmux sockets to prove:

- a fully safe stale polecat becomes reclaimable;
- replacing the generation between evaluation and commit causes no mutation;
- replacing a dog session between lookup and stop does not kill the replacement;
- both dog command forms exit successfully through the real CLI entry point;
- a remote ref and remote reflog remain byte-for-byte unchanged after nuke;
- dry-run causes zero local and network mutations; and
- output and JSON match the actual runtime state after normal and failed
  teardown.

### Regression and race gates

Run focused normal and race tests for the affected command, polecat, dog, tmux,
Git, and beads-store packages. Then run the repository's documented full test,
formatting, vet, and lint gates. Test harnesses must use unique temporary roots
and must prove that they leave no Dolt databases, worktrees, branches, stashes,
sessions, processes, or remote refs behind.

## Acceptance Criteria

1. A proven-safe stale polecat record transitions atomically to truthful idle and
   clean state and becomes reclaimable through the ordinary allocator path.
2. A superseding polecat generation cannot be changed or retired through a stale
   name, timestamp, PID, or session handle.
3. `gt polecat nuke` and its dry-run perform zero remote writes under success,
   refusal, retry, and failure conditions.
4. Nuke refuses local branch deletion whenever preservation is not already
   proven.
5. `gt dog done` and `gt dog done <name>` succeed for a dog actor through the
   compiled CLI command tree.
6. Dog closeout stops only the captured session generation and never a
   replacement with the same name.
7. Human and JSON status agree with durable work state and authoritative runtime
   session evidence, including lookup errors.
8. All focused normal/race gates and the full repository suite pass from a clean
   checkout with no new test residue.
9. Witness reviews the exact clean candidate SHA and approves the cumulative
   repair, including all earlier unresolved findings carried forward.
10. Implementation produces local commits only. No binary installation, remote
    push, live cleanup, broad restart, or Jobcron resume occurs.

## Implementation Sequence

1. Add failing regression tests at the real recovery, nuke, dog CLI, and session
   boundaries.
2. Introduce the generation-bound reconciliation proof and atomic persistence
   primitive.
3. Remove implicit publication from nuke and tighten preservation messages.
4. Make dog closeout generation-safe and make status evidence-based.
5. Run focused, race, full-suite, residue, and clean-tree gates.
6. Commit each meaningful checkpoint locally and request exact-SHA Witness review.
7. Resolve critical reachable custody defects under the documented decision
   principles; record defensible deferrals without expanding scope silently.

The work is sequential because reconciliation and nuke overlap in
`internal/cmd/polecat.go`, and the shared lifecycle semantics should settle before
dog closeout adopts them.

## Do Not Touch

Preserve these already-approved behaviors unless a new regression test proves
that a narrow compatibility edit is required:

- bounded retained-cleanup retries and caller deadlines;
- cgroup and PID-namespace custody protections;
- generation-aware poller and session ownership fixes;
- Mayor wake-on-convoy-completion behavior;
- existing foreign worktrees, stashes, recovery refs, active agents, and protected
  Dolt databases; and
- the current paused state of Jobcron.

## Rollback Strategy

Keep each repair as a local, reviewable commit. If a candidate regresses lifecycle
behavior, revert only the responsible commit in a clean worktree and rerun the
same focused and full gates. Do not use a destructive reset, remove recovery
evidence, or reinstall a binary merely to make tests pass.

The pre-campaign installed source commit remains `7fc97535`; the current branch
adds only the existing documentation commit before this specification. Any later
installation requires a separately reviewed exact SHA and an explicit rollout
decision.

## Completion Evidence

The implementation handoff must record:

- exact base and candidate SHAs;
- focused, race, full-suite, and residue-check commands and results;
- proof that test remotes received no unexpected refs;
- proof that replacement-generation fixtures survived attempted stale cleanup;
- clean worktree and stash status;
- Witness's durable exact-SHA verdict; and
- any unexpected error, the chosen recovery, and its remaining risk.
