package appsource

import (
	"context"
	"errors"
	"testing"
	"time"

	"primeradiant.com/evener/appwire"
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

func TestRecoveryCancelsInitializedDaemonRPC(t *testing.T) {
	for _, method := range []string{appwire.MethodThreadRead, appwire.MethodTurnStart} {
		t.Run(method, func(t *testing.T) {
			source := NewLocalDaemonSource("local", nil, nil)
			entry := rendezvousEntry("ws://daemon")
			accepted := 0
			var transport *scriptedAppwireTransport
			transport = newScriptedAppwireTransport(func(ctx context.Context, msg appwire.Message) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if msg.Notification != nil && msg.Notification.Method == appwire.MethodInitialized {
					// Recovery begins after the handshake write succeeds, before
					// the caller starts its RPC. Transport teardown may lag it.
					t.Cleanup(source.BeginRecovery(entry))
					return nil
				}
				if msg.Request != nil {
					if msg.Request.Method == appwire.MethodInitialize {
						transport.recv <- appwire.ResponseMessage(msg.Request.ID, appwire.InitializeResponse{ProtocolVersion: appwire.ProtocolVersion})
					} else {
						accepted++
						transport.recv <- appwire.ResponseMessage(msg.Request.ID, map[string]any{})
					}
				}
				return nil
			})
			source.dial = dialTransport(transport)
			var err error
			if method == appwire.MethodThreadRead {
				_, err = source.ReadThreadAtEntry(t.Context(), entry, appwire.ThreadReadParams{ThreadID: "thread"})
			} else {
				_, err = source.StartTurnAtEntry(t.Context(), entry, appwire.TurnStartParams{ThreadID: "thread", ClientMutationID: "mutation"})
			}
			if accepted != 0 {
				t.Errorf("accepted %d RPCs after recovery canceled the daemon call", accepted)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("RPC error = %v, want context.Canceled", err)
			}
		})
	}
}
