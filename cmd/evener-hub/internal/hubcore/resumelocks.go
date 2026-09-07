package hubcore

import "sync"

// ResumeLocks hands out one mutex per session id so concurrent resume attempts
// for the same session serialize. Both the REST send path and the RPC
// auto-resume path share a single instance (via WebConfig.ResumeLocks) so a
// resume triggered on one transport blocks a racing resume on the other,
// preventing two daemons from being spawned for one exited session (kata sm1a).
type ResumeLocks struct {
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	recovery map[string]SessionRecoveryState
	sequence uint64
}

// NewResumeLocks returns an empty registry ready for use.
func NewResumeLocks() *ResumeLocks {
	return &ResumeLocks{locks: map[string]*sync.Mutex{}}
}

// For returns the mutex for sessionID, creating it on first use. Repeated calls
// with the same id return the same mutex, so callers serialize against each
// other regardless of which path (REST or RPC) they came in on.
func (r *ResumeLocks) For(sessionID string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.locks[sessionID]
	if !ok {
		m = &sync.Mutex{}
		r.locks[sessionID] = m
	}
	return m
}

// SessionRecoveryState is the action admission state shared by every transport.
// Epoch changes invalidate actions that were waiting for session ownership.
type SessionRecoveryState struct {
	Epoch                uint64
	LastRecoverySequence uint64
	Stopping             int
	ResumeRequired       bool
	group                *sessionRecoveryGroup
}

type sessionRecoveryGroup struct {
	aliases []string
}

func (r *ResumeLocks) RecoveryState(sessionID string) SessionRecoveryState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recovery[sessionID]
}

// RecoverySequence is captured once when a transport is established. A
// request read later cannot turn unread pre-recovery input into fresh intent.
func (r *ResumeLocks) RecoverySequence() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sequence
}

// BeginForceStop blocks new actions and invalidates existing ownership waiters.
// A failed stop releases the active fence without declaring the session stopped.
func (r *ResumeLocks) BeginForceStop(aliases []string) func(bool) {
	r.mu.Lock()
	if r.recovery == nil {
		r.recovery = make(map[string]SessionRecoveryState)
	}
	r.sequence++
	group := &sessionRecoveryGroup{aliases: append([]string(nil), aliases...)}
	for _, id := range aliases {
		state := r.recovery[id]
		state.Epoch++
		state.LastRecoverySequence = r.sequence
		state.Stopping++
		state.group = group
		r.recovery[id] = state
	}
	r.mu.Unlock()
	return func(stopped bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.sequence++
		for _, id := range aliases {
			state := r.recovery[id]
			state.Stopping--
			state.LastRecoverySequence = r.sequence
			state.ResumeRequired = state.ResumeRequired || stopped
			r.recovery[id] = state
		}
	}
}

// ExplicitResumeCompleted clears every alias only if no newer recovery began.
func (r *ResumeLocks) ExplicitResumeCompleted(sessionID string, epoch uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.recovery[sessionID]
	if state.Epoch != epoch || state.Stopping != 0 || state.group == nil {
		return
	}
	for _, id := range state.group.aliases {
		alias := r.recovery[id]
		if alias.group != state.group || alias.Stopping != 0 {
			continue
		}
		alias.ResumeRequired = false
		r.recovery[id] = alias
	}
}
