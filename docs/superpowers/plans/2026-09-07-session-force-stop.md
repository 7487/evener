# Session Force Stop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let Jesse explicitly stop an incompatible or unresponsive local daemon without losing saved session data.

**Architecture:** A hub-owned force-stop operation resolves one local root daemon under the existing session ownership lock. A platform boundary verifies the OS process generation and the daemon's ownership of the session API log, signals that generation, and confirms exit. The frontend asks for explicit confirmation and refreshes only after the operation succeeds.

**Tech Stack:** Go, AppWire, React/TypeScript, Linux pidfds, Darwin proc_info/audit-token signaling. No cgo or new dependencies.

**Spec:** GitHub issue https://github.com/prime-radiant-inc/evener/issues/934 and Jesse's approved design in this task.

## Global Constraints

- Keep this PR separate from #936; initially stack it on codex/running-session-regressions.
- No old-protocol compatibility and no automatic force-stop or restart.
- Refuse stale or ambiguous identity. Never signal by an unverified numeric PID.
- Preserve transcripts and API logs. Do not remove rendezvous or session files as a substitute for process exit.
- Explicitly warn that active turns and jobs may be interrupted.
- Tests use fake OS boundaries or fixture-owned processes and files; never target ambient agent processes.
- Resume and deletion must remain serialized with force-stop.

## Task 1: Verified process termination

**Files:** Create `cmd/evener-hub/internal/daemonprocess/process.go`, `process_darwin.go`, `process_linux.go`, `process_other.go`, and direct behavioral tests.

**Interfaces:** The hub supplies the rendezvous identity and canonical session data location. The package exposes:

```go
type Target struct {
    PID int
    SessionID string
    StateDir string
    StartedAt time.Time
}
type Process interface {
    Kill() error
    Wait(context.Context) error
    Close() error
}
type Controller interface { Open(Target) (Process, error) }
```

- [ ] Write tests for refusal of invalid/self PID, missing session identity, reused process generation, wrong owner, and missing API-log ownership; verify no signal occurs.
- [ ] Verify each test fails before implementation.
- [ ] Implement the platform boundary. Linux opens a pidfd before inspection and signals through that fd. Darwin reads the unique process identifier and pid version, then signals with an audit token carrying that pid version. Compare identity before and after inspecting ownership. Refuse unsupported inspection/signaling APIs.
- [ ] Verify the current user owns a `serve` process and that its writable, locked API-log descriptor identifies `StateDir/sessions/SessionID.api.jsonl`. Compare device/inode, not only a path string. Recheck ownership before signaling. Reject OS process starts after the rendezvous timestamp.
- [ ] Confirm exit using the bound process identity. An already-exited process is idempotent success; a timeout remains an error. A reused PID is never waited on or signaled as the old process.
- [ ] Run direct package tests and cross-compile the supported platform implementations; commit with the identity and failure contracts documented.

## Task 2: Hub force-stop operation

**Files:** Create `cmd/evener-hub/app_force_stop.go` and `app_force_stop_test.go`; modify `appwire/types.go`, the AppWire registration/generation inputs, `cmd/evener-hub/app_rpc.go`, and `cmd/evener-hub/internal/hubcore/config.go`.

**Interfaces:** Add a hub-only `evener/thread/forceStop` request with a local root session ref. Inject Task 1's controller through WebConfig for deterministic OS-boundary tests.

```go
type ThreadForceStopParams struct { Ref string `json:"ref"` }
// The response is EmptyResponse only after confirmed process exit.
```

- [ ] Add real hub RPC tests using real rendezvous files and a fake process controller. Cover incompatible and unresponsive roots, responsive normal shutdown remaining unchanged, already-exited targets, ambiguous/multiple owners, wrong-source refs, stale identity, signal errors, timeout, and saved transcript retention.
- [ ] Add a lock-order test: hold force-stop's exit confirmation and prove resume/deletion cannot replace/remove that session until it completes.
- [ ] Run the new tests and establish failures.
- [ ] Resolve only direct local root claims from strict rendezvous discovery. Do not infer a descendant force-stop as permission to kill its ancestor. Hold the same per-session lock used by resume/deletion across discovery, validation, signaling, and exit confirmation. Call `Open`, `Kill`, then `Wait`; always `Close` the handle. Return errors without changing ownership or saved data.
- [ ] Refresh roster/navigation after confirmed exit. Preserve a successful stop result if ancillary UI refresh fails; retain stale-discovery diagnostics.
- [ ] Register the typed method as hub-only, update required protocol contracts, run `make generate`, and verify focused RPC tests before committing.

## Task 3: Explicit user action and end-to-end verification

**Files:** Modify the session actions UI and its tests, session store method, and generated frontend protocol files.

**Interfaces:** The confirmed action calls `evener/thread/forceStop` and refreshes the session only after success. Ordinary shutdown remains separate.

- [ ] Add frontend behavior tests: no RPC before confirmation, cancel sends nothing, confirm sends one force-stop request, pending confirmation remains disabled, failures retain an actionable error, and successful exit enables explicit resume after refresh.
- [ ] Add a Force stop action for local root sessions, including restart-required sessions whose daemon capabilities are unavailable. Use the existing dialog/button conventions with the warning that active turns and jobs may be interrupted and saved transcripts retained.
- [ ] Run the tests red, implement the action, then run focused tests green.
- [ ] Run Biome on touched src files, `make test-web`, all five browser guards, and `make merge-approval-gate`. Review the whole branch against its stack base and commit verified changes.
- [ ] Create a separate PR closing #934, state the #936 dependency, request RoboRev, and address actionable findings without merging.
