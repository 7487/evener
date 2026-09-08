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
	for _, tc := range []struct {
		method     string
		duringSend bool
	}{
		{appwire.MethodThreadRead, false},
		{appwire.MethodTurnStart, false},
		{appwire.MethodThreadRead, true},
		{appwire.MethodTurnStart, true},
	} {
		boundary := "initialized"
		if tc.duringSend {
			boundary = "send"
		}
		t.Run(tc.method+"/"+boundary, func(t *testing.T) {
			source := NewLocalDaemonSource("local", nil, nil)
			entry := rendezvousEntry("ws://daemon")
			accepted := 0
			attempted := 0
			var transport *scriptedAppwireTransport
			transport = newScriptedAppwireTransport(func(ctx context.Context, msg appwire.Message) error {
				if msg.Request != nil && msg.Request.Method == tc.method {
					attempted++
					if tc.duringSend {
						// Cancellation while a send is pending must reach its context.
						t.Cleanup(source.BeginRecovery(entry))
					}
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if !tc.duringSend && msg.Notification != nil && msg.Notification.Method == appwire.MethodInitialized {
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
			if tc.method == appwire.MethodThreadRead {
				_, err = source.ReadThreadAtEntry(t.Context(), entry, appwire.ThreadReadParams{ThreadID: "thread"})
			} else {
				_, err = source.StartTurnAtEntry(t.Context(), entry, appwire.TurnStartParams{ThreadID: "thread", ClientMutationID: "mutation"})
			}
			if !tc.duringSend && attempted != 0 {
				t.Errorf("attempted %d RPCs after initialization was canceled", attempted)
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
