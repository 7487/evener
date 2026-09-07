package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/cmd/evener-hub/internal/daemonprocess"
	"primeradiant.com/evener/cmd/evener-hub/internal/hubcore"
	"primeradiant.com/evener/rendezvous"
)

type forceStopControllerFunc func(daemonprocess.Target) (daemonprocess.Process, error)

func (f forceStopControllerFunc) Open(target daemonprocess.Target) (daemonprocess.Process, error) {
	return f(target)
}

type forceStopProcess struct {
	events           *[]string
	killErr, waitErr error
}

func (p *forceStopProcess) Kill() error { *p.events = append(*p.events, "kill"); return p.killErr }
func (p *forceStopProcess) Wait(context.Context) error {
	*p.events = append(*p.events, "wait")
	return p.waitErr
}
func (p *forceStopProcess) Close() error { *p.events = append(*p.events, "close"); return nil }

func TestHubForceStopConfirmsExitAndPreservesSavedData(t *testing.T) {
	for _, protocol := range []string{"evener-appwire-v3", appwire.ProtocolVersion} {
		t.Run(protocol, func(t *testing.T) {
			stateDir, runDir := t.TempDir(), t.TempDir()
			sessionID := buildRPCParentSession(t, stateDir)
			saved, err := os.ReadDir(filepath.Join(stateDir, "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			contents := map[string][]byte{}
			for _, file := range saved {
				if !file.IsDir() {
					contents[file.Name()], err = os.ReadFile(filepath.Join(stateDir, "sessions", file.Name()))
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			entry := rendezvous.Entry{PID: 4242, SessionID: sessionID, ThreadID: sessionID, WorkspaceRef: "local:" + sessionID, StateDir: stateDir, Protocol: protocol, Endpoint: "ws://127.0.0.1:1/rpc", StartedAt: time.Now()}
			writeRendezvous(t, runDir, entry)
			var events []string
			controller := forceStopControllerFunc(func(target daemonprocess.Target) (daemonprocess.Process, error) {
				events = append(events, "open")
				if target.PID != entry.PID || target.SessionID != sessionID || target.StateDir != stateDir || !target.StartedAt.Equal(entry.StartedAt) {
					t.Errorf("target=%+v", target)
				}
				return &forceStopProcess{events: &events}, nil
			})
			hub := newHubRPCTestServer(t, hubcore.WebConfig{RunDir: runDir, ResumeLocks: hubcore.NewResumeLocks(), DaemonProcesses: controller})
			defer hub.Close()
			client := dialHubRPC(t, hub)
			defer client.Close()
			if _, err := client.Initialize(t.Context(), appwire.InitializeParams{}); err != nil {
				t.Fatal(err)
			}
			var response appwire.EmptyResponse
			if err := client.Request(t.Context(), "evener/thread/forceStop", map[string]string{"ref": "local:" + sessionID}, &response); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(events, []string{"open", "kill", "wait", "close"}) {
				t.Fatalf("events=%v", events)
			}
			for name, want := range contents {
				got, err := os.ReadFile(filepath.Join(stateDir, "sessions", name))
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("saved file %s changed: %v", name, err)
				}
			}
			if entries, err := rendezvous.ListStrict(runDir); err != nil || len(entries) != 1 {
				t.Fatalf("stop removed ownership data instead of confirming exit: %v %v", entries, err)
			}
		})
	}
}

func TestForceStopFailuresDoNotPretendExit(t *testing.T) {
	for _, stage := range []string{"identity", "signal", "exit", "alreadyExited"} {
		t.Run(stage, func(t *testing.T) {
			runDir := t.TempDir()
			entry := rendezvous.Entry{PID: 4242, SessionID: webTestSessionID, ThreadID: webTestSessionID, StateDir: t.TempDir(), StartedAt: time.Now()}
			writeRendezvous(t, runDir, entry)
			var events []string
			controller := forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) {
				events = append(events, "open")
				if stage == "identity" {
					return nil, errors.New("identity changed")
				}
				if stage == "alreadyExited" {
					return nil, daemonprocess.ErrExited
				}
				p := &forceStopProcess{events: &events}
				if stage == "signal" {
					p.killErr = errors.New("signal denied")
				}
				if stage == "exit" {
					p.waitErr = context.DeadlineExceeded
				}
				return p, nil
			})
			err := forceStopThread(t.Context(), hubcore.WebConfig{RunDir: runDir, ResumeLocks: hubcore.NewResumeLocks(), DaemonProcesses: controller}, appwire.ThreadForceStopParams{Ref: "local:" + webTestSessionID})
			if (err == nil) != (stage == "alreadyExited") {
				t.Fatalf("error=%v", err)
			}
			want := map[string][]string{"identity": {"open"}, "signal": {"open", "kill", "close"}, "exit": {"open", "kill", "wait", "close"}, "alreadyExited": {"open"}}[stage]
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events=%v want=%v", events, want)
			}
		})
	}
}

func TestForceStopRejectsAmbiguousAndForeignTargets(t *testing.T) {
	for _, tc := range []struct {
		name, ref string
		entries   []rendezvous.Entry
	}{
		{"foreign", "remote:owner", nil}, {"invalid", "owner", nil}, {"missing", "local:owner", nil},
		{"multiple", "local:owner", []rendezvous.Entry{{PID: 4242, SessionID: "owner"}, {PID: 4243, SessionID: "owner"}}},
		{"foreign claim", "local:owner", []rendezvous.Entry{{PID: 4242, SessionID: "owner", SourceID: "remote"}}},
		{"descendant", "local:child", []rendezvous.Entry{{PID: 4242, SessionID: "owner", ThreadID: "owner"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runDir := t.TempDir()
			for _, entry := range tc.entries {
				writeRendezvous(t, runDir, entry)
			}
			controller := forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) {
				t.Error("unsafe target reached process controller")
				return nil, errors.New("unexpected")
			})
			if err := forceStopThread(t.Context(), hubcore.WebConfig{RunDir: runDir, ResumeLocks: hubcore.NewResumeLocks(), DaemonProcesses: controller}, appwire.ThreadForceStopParams{Ref: tc.ref}); err == nil {
				t.Fatal("unsafe target accepted")
			}
		})
	}
}

type waitingForceStopProcess struct{ entered, release chan struct{} }

func (p *waitingForceStopProcess) Kill() error { return nil }
func (p *waitingForceStopProcess) Wait(ctx context.Context) error {
	close(p.entered)
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *waitingForceStopProcess) Close() error { return nil }

func TestForceStopHoldsResumeAndDeletionAliasesUntilExit(t *testing.T) {
	runDir := t.TempDir()
	entry := rendezvous.Entry{PID: 4242, SessionID: "current", ThreadID: "current", WorkspaceRef: "local:stable", StateDir: t.TempDir(), StartedAt: time.Now()}
	writeRendezvous(t, runDir, entry)
	process := &waitingForceStopProcess{entered: make(chan struct{}), release: make(chan struct{})}
	cfg := hubcore.WebConfig{RunDir: runDir, ResumeLocks: hubcore.NewResumeLocks(), DaemonProcesses: forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) { return process, nil })}
	stopped := make(chan error, 1)
	go func() {
		stopped <- forceStopThread(t.Context(), cfg, appwire.ThreadForceStopParams{Ref: "local:stable"})
	}()
	select {
	case <-process.entered:
	case err := <-stopped:
		t.Fatalf("force stop returned before exit confirmation: %v", err)
	}
	for _, alias := range []string{"current", "stable"} {
		lock := cfg.ResumeLocks.For(alias)
		if lock.TryLock() {
			lock.Unlock()
			t.Errorf("ownership lock for %s released before exit", alias)
		}
	}
	close(process.release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"current", "stable"} {
		lock := cfg.ResumeLocks.For(alias)
		if !lock.TryLock() {
			t.Errorf("ownership lock for %s retained after exit", alias)
		} else {
			lock.Unlock()
		}
	}
}
