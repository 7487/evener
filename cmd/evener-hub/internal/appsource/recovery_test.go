package appsource

import (
	"context"
	"errors"
	"testing"
	"time"

	"primeradiant.com/evener/rendezvous"
)

func TestRecoveryCancelsDaemonCallsAcrossClearAliases(t *testing.T) {
	source := NewLocalDaemonSource("local", nil, nil)
	entry := rendezvous.Entry{PID: 4242, StartedAt: time.Now(), StateDir: t.TempDir(), SessionID: "before", ThreadID: "stable"}
	pending, finish := source.beginDaemonCall(t.Context(), entry)
	defer finish()
	other := entry
	other.PID++
	unaffected, finishOther := source.beginDaemonCall(t.Context(), other)
	defer finishOther()
	cleared := entry
	// Rendezvous discovery has no monotonic clock component.
	cleared.StartedAt = entry.StartedAt.UTC()
	cleared.SessionID = "after"
	cleared.WorkspaceRef = "local:after"
	release := source.BeginRecovery(cleared)
	if !errors.Is(pending.Err(), context.Canceled) {
		t.Fatalf("in-flight old alias: %v", pending.Err())
	}
	if unaffected.Err() != nil {
		t.Fatalf("another daemon canceled: %v", unaffected.Err())
	}
	blocked, finishBlocked := source.beginDaemonCall(t.Context(), entry)
	defer finishBlocked()
	if !errors.Is(blocked.Err(), context.Canceled) {
		t.Fatalf("new old-alias call admitted: %v", blocked.Err())
	}
	release()
	resumed, finishResumed := source.beginDaemonCall(t.Context(), cleared)
	defer finishResumed()
	if resumed.Err() != nil {
		t.Fatalf("call blocked after recovery release: %v", resumed.Err())
	}
}
