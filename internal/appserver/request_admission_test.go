package appserver

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"primeradiant.com/evener/appwire"
)

type requestAdmissionTestKey struct{}

func TestRequestAdmissionContextSurvivesSerialQueue(t *testing.T) {
	var epoch atomic.Int32
	epoch.Store(1)
	admitted := make(chan struct{})
	observed := make(chan context.Context, 1)
	server := NewServer(ServerConfig{RequestAdmissionContext: func(ctx context.Context, message appwire.Message) context.Context {
		if message.Request.Method != appwire.MethodThreadModelSet {
			return ctx
		}
		ctx = context.WithValue(ctx, requestAdmissionTestKey{}, epoch.Load())
		close(admitted)
		return ctx
	}})
	serialStarted, release := parkThreadList(t, server)
	HandleTyped(server.Router(), appwire.MethodThreadModelSet, func(ctx context.Context, _ appwire.ThreadModelSetParams) (appwire.EmptyResponse, error) {
		observed <- ctx
		return appwire.EmptyResponse{}, nil
	})
	client := dialAppWireClient(t, serveWebSocketHTTP(t, server))
	go func() { _, _ = client.ThreadList(t.Context(), appwire.ThreadListParams{}) }()
	waitFor(t, "serial handler", serialStarted)
	done := make(chan error, 1)
	go func() {
		done <- client.Request(t.Context(), appwire.MethodThreadModelSet, appwire.ThreadModelSetParams{Ref: "local:A", Model: "test", ModelProvider: "test"}, nil)
	}()
	waitFor(t, "request admission", admitted)
	epoch.Store(2)
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ctx := <-observed
	if ctx.Value(requestAdmissionTestKey{}) != int32(1) {
		t.Fatalf("queued request recaptured metadata: %v", ctx.Value(requestAdmissionTestKey{}))
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("admitted context lost connection cancellation")
	}
}

func TestRequestAdmissionFailureKeepsConnectionUsable(t *testing.T) {
	for _, mode := range []string{"panic", "nil context"} {
		t.Run(mode, func(t *testing.T) {
			server := NewServer(ServerConfig{RequestAdmissionContext: func(ctx context.Context, message appwire.Message) context.Context {
				if message.Request.Method != appwire.MethodThreadModelSet {
					return ctx
				}
				if mode == "panic" {
					panic("fixture admission failure")
				}
				return nil
			}})
			client := dialAppWireClient(t, serveWebSocketHTTP(t, server))
			err := client.Request(t.Context(), appwire.MethodThreadModelSet, appwire.ThreadModelSetParams{Ref: "local:A", Model: "test", ModelProvider: "test"}, nil)
			if err == nil {
				t.Fatal("failed admission accepted")
			}
			if err := client.Request(t.Context(), appwire.MethodPing, appwire.EmptyParams{}, nil); err != nil {
				t.Fatalf("admission failure broke connection: %v", err)
			}
		})
	}
}
