package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/cmd/evener-hub/internal/daemonprocess"
	"primeradiant.com/evener/cmd/evener-hub/internal/hubcore"
	"primeradiant.com/evener/cmd/evener-hub/internal/hubtest"
	"primeradiant.com/evener/internal/appserver"
	"primeradiant.com/evener/rendezvous"
)

func TestResumeUsesRetainedCurrentSessionAndReservesAliases(t *testing.T) {
	for _, recreated := range []bool{false, true} {
		t.Run(map[bool]string{false: "same hub", true: "recreated hub"}[recreated], func(t *testing.T) {
			root := t.TempDir()
			locks, err := hubcore.NewPersistentResumeLocks(root)
			if err != nil {
				t.Fatal(err)
			}
			aliases := []string{"a-stable", "z-current"}
			finish := locks.BeginForceStop(aliases)
			if err := locks.PersistForceStop(aliases, "z-current"); err != nil {
				t.Fatal(err)
			}
			finish(true)
			if recreated {
				locks, err = hubcore.NewPersistentResumeLocks(root)
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks}
			writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 4242, SessionID: "z-current", ThreadID: "z-current", WorkspaceRef: "local:a-stable"})
			reached := errors.New("launcher observed")
			var launches atomic.Int32
			cfg.Spawner = &fakeRPCSpawner{resume: func(_ context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
				launches.Add(1)
				if req.SessionID != "z-current" {
					t.Errorf("launched superseded alias %q", req.SessionID)
				}
				for _, id := range aliases {
					lock := locks.For(id)
					if lock.TryLock() {
						lock.Unlock()
						t.Errorf("resume did not reserve alias %s", id)
					}
				}
				return rendezvous.Entry{}, reached
			}}
			// Both real lifecycle calls exercise their shared ownership reservation.
			var wg sync.WaitGroup
			for _, id := range aliases {
				wg.Go(func() {
					_, err := hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:" + id})
					if err == nil {
						t.Error("launcher failure lost")
					}
				})
			}
			wg.Wait()
			if launches.Load() != 2 {
				t.Fatalf("launcher calls=%d", launches.Load())
			}
		})
	}
}

func TestResumeMissingMarkerUsesDurableRecoveryTarget(t *testing.T) {
	for _, recreated := range []bool{false, true} {
		for _, multiple := range []bool{false, true} {
			t.Run(map[bool]string{false: "same", true: "recreated"}[recreated]+map[bool]string{false: " single", true: " aliases"}[multiple], func(t *testing.T) {
				root := t.TempDir()
				locks, err := hubcore.NewPersistentResumeLocks(root)
				if err != nil {
					t.Fatal(err)
				}
				aliases := []string{"a-stable"}
				if multiple {
					aliases = append(aliases, "z-current")
				}
				target := "a-stable"
				if multiple {
					target = "z-current"
				}
				finish := locks.BeginForceStop(aliases)
				if err := locks.PersistForceStop(aliases, target); err != nil {
					t.Fatal(err)
				}
				finish(true)
				if recreated {
					locks, err = hubcore.NewPersistentResumeLocks(root)
					if err != nil {
						t.Fatal(err)
					}
				}
				called := false
				cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks, Spawner: &fakeRPCSpawner{resume: func(_ context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
					called = true
					want := "a-stable"
					if multiple {
						want = "z-current"
					}
					if req.SessionID != want {
						t.Errorf("resume target=%s want=%s", req.SessionID, want)
					}
					return rendezvous.Entry{}, errors.New("launcher observed")
				}}}
				for _, alias := range aliases {
					called = false
					_, err = hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:" + alias})
					if err == nil {
						t.Fatal("expected launcher error")
					}
					if !called {
						t.Fatalf("alias %s did not launch: %v", alias, err)
					}
				}

				if !locks.RecoveryState("a-stable").ResumeRequired {
					t.Fatal("failed resume cleared recovery")
				}
			})
		}
	}
}

func TestConcurrentAliasResumeSpawnsOneCurrentDaemon(t *testing.T) {
	var current string
	cfg, id, calls := parityResumeFixture(t, func(daemon *appserver.Server) {
		appserver.HandleTyped(daemon.Router(), appwire.MethodThreadList, func(context.Context, appwire.ThreadListParams) (appwire.ThreadListResponse, error) {
			return appwire.ThreadListResponse{Data: []appwire.Thread{{ID: current, SessionID: current, Source: "local", Status: appwire.ThreadStatus{Type: "idle"}, Evener: appwire.EvenerThread{Ref: "local:" + current, InstanceID: current}}}}, nil
		})
		appserver.HandleTyped(daemon.Router(), appwire.MethodThreadRead, func(_ context.Context, params appwire.ThreadReadParams) (appwire.ThreadReadResponse, error) {
			return appwire.ThreadReadResponse{Thread: appwire.Thread{ID: current, SessionID: current, Source: "local", Status: appwire.ThreadStatus{Type: "idle"}, Evener: appwire.EvenerThread{Ref: params.Ref, InstanceID: current}}}, nil
		})
	})
	current = id
	cfg.Roster = hubcore.NewRoster(cfg.RunDir, &hubcore.StatusProber{})
	stable := "stable-workspace"
	root := t.TempDir()
	locks, err := hubcore.NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ResumeLocks = locks
	cfg.DaemonProcesses = forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) { return nil, daemonprocess.ErrExited })
	writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 106, StartedAt: time.Now().Add(-365 * 24 * time.Hour), SessionID: current, ThreadID: current, WorkspaceRef: "local:" + stable})
	if err := forceStopThread(t.Context(), cfg, appwire.ThreadForceStopParams{Ref: "local:" + stable}, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := rendezvous.ListStrict(cfg.RunDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired crash marker retained: %v %v", entries, err)
	}
	// Recreate authority after force stop's refresh removed the expired marker.
	cfg.ResumeLocks, err = hubcore.NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}

	original := cfg.Spawner
	entered := make(chan struct{})
	var enterOnce sync.Once
	release := make(chan struct{})
	cfg.Spawner = &fakeRPCSpawner{resume: func(ctx context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
		for _, alias := range []string{stable, current} {
			lock := cfg.ResumeLocks.For(alias)
			if lock.TryLock() {
				lock.Unlock()
				t.Errorf("launcher did not reserve %s", alias)
			}
		}
		enterOnce.Do(func() { close(entered) })
		<-release
		entry, err := original.Resume(ctx, req)
		if err == nil {
			entry.WorkspaceRef = "local:" + stable
			writeRendezvous(t, cfg.RunDir, entry)
		}
		return entry, err
	}}
	hub := newHubRPCTestServer(t, cfg)
	defer hub.Close()
	first := dialHubRPC(t, hub)
	defer first.Close()
	second := dialHubRPC(t, hub)
	defer second.Close()
	if _, err := first.Initialize(t.Context(), appwire.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Initialize(t.Context(), appwire.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() {
		_, err := first.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: "local:" + stable})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("first resume did not launch: %v", err)
	}
	go func() {
		_, err := second.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: "local:" + current})
		done <- err
	}()
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 1 {
		t.Fatalf("resume launches=%d", *calls)
	}
	for _, alias := range []string{stable, current} {
		if cfg.ResumeLocks.RecoveryState(alias).ResumeRequired {
			t.Fatalf("successful resume retained recovery for %s", alias)
		}
	}
	// Normal daemon shutdown removes its marker after recovery completed. Both
	// identities must remain resumable without recreating the hub's lock registry.
	for _, alias := range []string{stable, current} {
		if err := rendezvous.Remove(cfg.RunDir, 106); err != nil {
			t.Fatal(err)
		}
		if _, err := first.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: "local:" + alias}); err != nil {
			t.Fatalf("resume after marker removal through %s: %v", alias, err)
		}
	}
	if *calls != 3 {
		t.Fatalf("repeated markerless launches=%d", *calls)
	}

}

func TestResumeRejectsConflictingRetainedTranscriptIdentities(t *testing.T) {
	cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: hubcore.NewResumeLocks()}
	writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 101, SessionID: "first-current", ThreadID: "first-current", WorkspaceRef: "local:stable"})
	writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 102, SessionID: "second-current", ThreadID: "second-current", WorkspaceRef: "local:stable"})
	cfg.Spawner = &fakeRPCSpawner{resume: func(context.Context, hubcore.ResumeRequest) (rendezvous.Entry, error) {
		t.Error("ambiguous transcript reached launcher")
		return rendezvous.Entry{}, errors.New("launcher observed")
	}}
	if _, err := hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:stable"}); err == nil {
		t.Fatal("ambiguous retained transcripts accepted")
	}
}

func TestResumeIgnoresOnlyVerifiedExitedTranscriptClaims(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "dead old and live current", true: "unresolved old and live current"}[uncertain], func(t *testing.T) {
			cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: hubcore.NewResumeLocks()}
			// A previous force-stop and Resume obligation is already cleared. A later
			// daemon clear keeps the workspace alias but advances its current transcript.
			finish := cfg.ResumeLocks.BeginForceStop([]string{"stable", "old"})
			finish(true)
			if err := cfg.ResumeLocks.ExplicitResumeCompleted("stable", cfg.ResumeLocks.RecoveryState("stable").Epoch); err != nil {
				t.Fatal(err)
			}
			writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 101, SessionID: "old", ThreadID: "old", WorkspaceRef: "local:stable"})
			writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 102, SessionID: "current", ThreadID: "current", WorkspaceRef: "local:stable"})
			cfg.DaemonProcesses = forceStopControllerFunc(func(target daemonprocess.Target) (daemonprocess.Process, error) {
				if target.PID == 101 {
					if uncertain {
						return nil, errors.New("unverified identity")
					}
					return nil, daemonprocess.ErrExited
				}
				return &forceStopProcess{events: new([]string)}, nil
			})
			called := false
			cfg.Spawner = &fakeRPCSpawner{resume: func(_ context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
				called = true
				if req.SessionID != "current" {
					t.Errorf("launched %q", req.SessionID)
				}
				return rendezvous.Entry{}, errors.New("launcher observed")
			}}
			if _, err := hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:stable"}); err == nil {
				t.Fatal("expected launcher error or refusal")
			}
			if called == uncertain {
				t.Fatalf("launch=%v uncertain=%v", called, uncertain)
			}
		})
	}
}

func TestResumeRejectsTargetRedirectedByNewerRecovery(t *testing.T) {
	root := t.TempDir()
	locks, err := hubcore.NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	finish := locks.BeginForceStop([]string{"A", "B"})
	if err := locks.PersistForceStop([]string{"A", "B"}, "A"); err != nil {
		t.Fatal(err)
	}
	finish(true)
	finish = locks.BeginForceStop([]string{"A", "C"})
	if err := locks.PersistForceStop([]string{"A", "C"}, "C"); err != nil {
		t.Fatal(err)
	}
	finish(true)
	locks, err = hubcore.NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks, Spawner: &fakeRPCSpawner{resume: func(context.Context, hubcore.ResumeRequest) (rendezvous.Entry, error) {
		t.Error("superseded recovery target reached launcher")
		return rendezvous.Entry{}, errors.New("launcher observed")
	}}}
	if _, err := hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:B"}); err == nil {
		t.Fatal("redirected old target accepted")
	}
	for _, alias := range []string{"A", "B", "C"} {
		if !locks.RecoveryState(alias).ResumeRequired {
			t.Fatalf("failed resume cleared %s", alias)
		}
	}
}

func TestResumeUsesDurableTargetWhenAllRetainedClaimsExited(t *testing.T) {
	root := t.TempDir()
	locks, err := hubcore.NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	finish := locks.BeginForceStop([]string{"stable", "current"})
	if err := locks.PersistForceStop([]string{"stable", "current"}, "current"); err != nil {
		t.Fatal(err)
	}
	finish(true)
	locks, err = hubcore.NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks, DaemonProcesses: forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) { return nil, daemonprocess.ErrExited })}
	writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 101, SessionID: "old", ThreadID: "old", WorkspaceRef: "local:stable"})
	writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 102, SessionID: "current", ThreadID: "current", WorkspaceRef: "local:stable"})
	called := false
	cfg.Spawner = &fakeRPCSpawner{resume: func(_ context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
		called = true
		if req.SessionID != "current" {
			t.Errorf("launched %q", req.SessionID)
		}
		return rendezvous.Entry{}, errors.New("launcher observed")
	}}
	if _, err := hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:stable"}); err == nil {
		t.Fatal("expected launcher error")
	}
	if !called {
		t.Fatal("persisted current target did not reach launcher")
	}
}

func TestAliasResumeHonorsRequestedAndResolvedDeletionFences(t *testing.T) {
	for _, deletedTarget := range []bool{false, true} {
		t.Run(map[bool]string{false: "requested alias", true: "resolved target"}[deletedTarget], func(t *testing.T) {
			stable, current := hubtest.SessionID(t), hubtest.SessionID(t)
			store, err := hubcore.NewDeletionStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			deleted := stable
			if deletedTarget {
				deleted = current
			}
			if _, err := store.Begin(filepath.Base(hubtest.ProjectDir(t, t.TempDir(), "deleted")), []hubcore.DeletionTarget{{Ref: "local:" + deleted, ThreadID: deleted}}); err != nil {
				t.Fatal(err)
			}
			cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: hubcore.NewResumeLocks(), DeletionStore: store}
			writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 101, SessionID: current, ThreadID: current, WorkspaceRef: "local:" + stable})
			launches := 0
			cfg.Spawner = &fakeRPCSpawner{resume: func(context.Context, hubcore.ResumeRequest) (rendezvous.Entry, error) {
				launches++
				return rendezvous.Entry{}, errors.New("deleted session reached launcher")
			}}
			_, err = hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:" + stable})
			var wire appwire.WireError
			if !errors.As(err, &wire) {
				t.Fatalf("deletion fence error=%v", err)
			}
			data, ok := wire.Data.(appwire.ErrorData)
			if !ok || data.MutationOutcome != appwire.MutationOutcomeTargetDeleted {
				t.Errorf("deletion outcome=%#v", wire.Data)
			}
			if launches != 0 {
				t.Fatalf("deleted target launch count=%d", launches)
			}
		})
	}
}

func TestCompletedResumeMappingDefersToCurrentIdentity(t *testing.T) {
	for _, newerRecovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh marker", true: "newer recovery"}[newerRecovery], func(t *testing.T) {
			locks := hubcore.NewResumeLocks()
			finish := locks.BeginForceStop([]string{"stable", "B"})
			if err := locks.PersistForceStop([]string{"stable", "B"}, "B"); err != nil {
				t.Fatal(err)
			}
			finish(true)
			epoch := locks.RecoveryState("stable").Epoch
			if err := locks.ExplicitResumeCompleted("stable", epoch); err != nil {
				t.Fatal(err)
			}
			locks.RecordResolvedSession("stable", "B", epoch)
			cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks}
			if newerRecovery {
				finish := locks.BeginForceStop([]string{"B", "C"})
				if err := locks.PersistForceStop([]string{"B", "C"}, "C"); err != nil {
					t.Fatal(err)
				}
				finish(true)
			} else {
				writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 101, ThreadID: "B", SessionID: "C"})
			}
			launches := 0
			cfg.Spawner = &fakeRPCSpawner{resume: func(_ context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
				launches++
				if req.SessionID != "C" {
					t.Errorf("remembered B overrode current target: %s", req.SessionID)
				}
				return rendezvous.Entry{}, errors.New("launcher observed")
			}}
			if _, err := hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:stable"}); err == nil {
				t.Fatal("expected launcher error or newer recovery refusal")
			}
			want := 1
			if newerRecovery {
				want = 0
			}
			if launches != want {
				t.Fatalf("launches=%d want=%d", launches, want)
			}
		})
	}
}

func TestResumeConflictingRefSessionChecksDeletionIdentities(t *testing.T) {
	for _, distinctTarget := range []bool{false, true} {
		for _, deletedIdentity := range []string{"ref", "session", "target"} {
			t.Run(map[bool]string{false: "direct ", true: "redirected "}[distinctTarget]+deletedIdentity, func(t *testing.T) {
				refID, sessionID := hubtest.SessionID(t), hubtest.SessionID(t)
				targetID := sessionID
				if distinctTarget {
					targetID = hubtest.SessionID(t)
				}
				deleted := map[string]string{"ref": refID, "session": sessionID, "target": targetID}[deletedIdentity]
				store, err := hubcore.NewDeletionStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Begin(filepath.Base(hubtest.ProjectDir(t, t.TempDir(), "deleted")), []hubcore.DeletionTarget{{Ref: "local:" + deleted, ThreadID: deleted}}); err != nil {
					t.Fatal(err)
				}
				cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: hubcore.NewResumeLocks(), DeletionStore: store}
				writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 101, ThreadID: sessionID, SessionID: targetID, WorkspaceRef: "local:" + refID})
				launches := 0
				cfg.Spawner = &fakeRPCSpawner{resume: func(context.Context, hubcore.ResumeRequest) (rendezvous.Entry, error) {
					launches++
					return rendezvous.Entry{}, errors.New("deleted session reached launcher")
				}}
				_, err = hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:" + refID, Session: sessionID})
				var wire appwire.WireError
				if !errors.As(err, &wire) {
					t.Fatalf("deletion error=%v", err)
				}
				data, ok := wire.Data.(appwire.ErrorData)
				if !ok || data.MutationOutcome != appwire.MutationOutcomeTargetDeleted {
					t.Errorf("deletion outcome=%#v", wire.Data)
				}
				if launches != 0 {
					t.Fatalf("deleted identity reached launcher %d times", launches)
				}
			})
		}
	}
}

func TestResumeTraversesTwoCompletedRecoveryCycles(t *testing.T) {
	a, b, c := hubtest.SessionID(t), hubtest.SessionID(t), hubtest.SessionID(t)
	endpoints := make(map[string]string)
	for _, id := range []string{b, c} {
		daemon := appserver.NewServer(appserver.ServerConfig{ServerName: "daemon", SourceID: "local"})
		thread := appwire.Thread{ID: id, SessionID: id, Source: "local", Status: appwire.ThreadStatus{Type: "idle"}, Evener: appwire.EvenerThread{Ref: "local:" + id, InstanceID: id}}
		appserver.HandleTyped(daemon.Router(), appwire.MethodThreadList, func(context.Context, appwire.ThreadListParams) (appwire.ThreadListResponse, error) {
			return appwire.ThreadListResponse{Data: []appwire.Thread{thread}}, nil
		})
		appserver.HandleTyped(daemon.Router(), appwire.MethodThreadRead, func(context.Context, appwire.ThreadReadParams) (appwire.ThreadReadResponse, error) {
			return appwire.ThreadReadResponse{Thread: thread}, nil
		})
		server := httptest.NewServer(http.HandlerFunc(daemon.ServeWebSocket))
		t.Cleanup(server.Close)
		endpoints[id] = "ws" + strings.TrimPrefix(server.URL, "http")
	}
	locks, err := hubcore.NewPersistentResumeLocks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks}
	cfg.Roster = hubcore.NewRoster(cfg.RunDir, &hubcore.StatusProber{})
	cfg.DaemonProcesses = forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) {
		return &forceStopProcess{events: new([]string)}, nil
	})
	launches := 0
	cfg.Spawner = &fakeRPCSpawner{resume: func(_ context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
		launches++
		want := c
		if launches == 1 {
			want = b
		}
		if req.SessionID != want {
			return rendezvous.Entry{}, fmt.Errorf("launched superseded transcript %s, want %s", req.SessionID, want)
		}
		if launches == 3 {
			for _, alias := range []string{a, b, c} {
				lock := locks.For(alias)
				if lock.TryLock() {
					lock.Unlock()
					t.Errorf("traversed alias %s was not reserved", alias)
				}
			}
		}
		entry := rendezvous.Entry{PID: 103, SessionID: req.SessionID, ThreadID: req.SessionID, Protocol: appwire.ProtocolVersion, Endpoint: endpoints[req.SessionID], SourceID: "local", StartedAt: time.Now()}
		writeRendezvous(t, cfg.RunDir, entry)
		return entry, nil
	}}
	hub := newHubRPCTestServer(t, cfg)
	defer hub.Close()
	for _, pair := range [][2]string{{a, b}, {b, c}} {
		// Clear advances the daemon's transcript and stable workspace identity.
		writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 101, SessionID: pair[1], ThreadID: pair[1], WorkspaceRef: "local:" + pair[0], StartedAt: time.Now()})
		if err := forceStopThread(t.Context(), cfg, appwire.ThreadForceStopParams{Ref: "local:" + pair[0]}, nil); err != nil {
			t.Fatal(err)
		}
		if err := rendezvous.Remove(cfg.RunDir, 101); err != nil {
			t.Fatal(err)
		}
		client := dialHubRPC(t, hub)
		if _, err := client.Initialize(t.Context(), appwire.InitializeParams{}); err != nil {
			t.Fatal(err)
		}
		_, err := client.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: "local:" + pair[0]})
		client.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := rendezvous.Remove(cfg.RunDir, 103); err != nil {
			t.Fatal(err)
		}
	}
	client := dialHubRPC(t, hub)
	defer client.Close()
	if _, err := client.Initialize(t.Context(), appwire.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: "local:" + a}); err != nil {
		t.Fatal(err)
	}
	if launches != 3 {
		t.Fatalf("resume launches=%d", launches)
	}
}

func TestResumeRejectsCompletedRedirectCycle(t *testing.T) {
	locks := hubcore.NewResumeLocks()
	for _, pair := range [][2]string{{"A", "B"}, {"B", "C"}, {"C", "A"}} {
		finish := locks.BeginForceStop([]string{pair[0]})
		finish(true)
		epoch := locks.RecoveryState(pair[0]).Epoch
		if err := locks.ExplicitResumeCompleted(pair[0], epoch); err != nil {
			t.Fatal(err)
		}
		locks.RecordResolvedSession(pair[0], pair[1], epoch)
	}
	launches := 0
	cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks, Spawner: &fakeRPCSpawner{resume: func(context.Context, hubcore.ResumeRequest) (rendezvous.Entry, error) {
		launches++
		return rendezvous.Entry{}, errors.New("cyclic target reached launcher")
	}}}
	if _, err := hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:A"}); err == nil {
		t.Fatal("completed redirect cycle accepted")
	}
	if launches != 0 {
		t.Fatalf("cycle reached launcher %d times", launches)
	}
}

func TestResumeCompletedChainRefusesNewPendingRecovery(t *testing.T) {
	for _, stopping := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "stopping"}[stopping], func(t *testing.T) {
			locks := hubcore.NewResumeLocks()
			for _, pair := range [][2]string{{"A", "B"}, {"B", "C"}} {
				finish := locks.BeginForceStop([]string{pair[0], pair[1]})
				if err := locks.PersistForceStop([]string{pair[0], pair[1]}, pair[1]); err != nil {
					t.Fatal(err)
				}
				finish(true)
				epoch := locks.RecoveryState(pair[0]).Epoch
				if err := locks.ExplicitResumeCompleted(pair[0], epoch); err != nil {
					t.Fatal(err)
				}
				locks.RecordResolvedSession(pair[0], pair[1], epoch)
			}
			finish := locks.BeginForceStop([]string{"C", "D"})
			if err := locks.PersistForceStop([]string{"C", "D"}, "D"); err != nil {
				t.Fatal(err)
			}
			if stopping {
				defer finish(true)
			} else {
				finish(true)
			}
			before := locks.RecoveryState("C")
			launches := 0
			cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks, Spawner: &fakeRPCSpawner{resume: func(context.Context, hubcore.ResumeRequest) (rendezvous.Entry, error) {
				launches++
				return rendezvous.Entry{}, errors.New("pending chained recovery reached launcher")
			}}}
			if _, err := hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:A"}); err == nil {
				t.Fatal("new pending recovery accepted through old chain")
			}
			if launches != 0 {
				t.Fatalf("pending recovery reached launcher %d times", launches)
			}
			after := locks.RecoveryState("C")
			if !after.ResumeRequired || after.Epoch != before.Epoch || after.Stopping != before.Stopping {
				t.Fatalf("old chain changed newer recovery: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestResumeCompletedChainPreservesConnectionAdmission(t *testing.T) {
	locks := hubcore.NewResumeLocks()
	for _, pair := range [][2]string{{"A", "B"}, {"B", "C"}} {
		finish := locks.BeginForceStop([]string{pair[0], pair[1]})
		if err := locks.PersistForceStop([]string{pair[0], pair[1]}, pair[1]); err != nil {
			t.Fatal(err)
		}
		finish(true)
		epoch := locks.RecoveryState(pair[0]).Epoch
		if err := locks.ExplicitResumeCompleted(pair[0], epoch); err != nil {
			t.Fatal(err)
		}
		locks.RecordResolvedSession(pair[0], pair[1], epoch)
	}
	cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks}
	stale := admitSessionConnection(t.Context(), cfg)
	finish := locks.BeginForceStop([]string{"C"})
	if err := locks.PersistForceStop([]string{"C"}, "C"); err != nil {
		t.Fatal(err)
	}
	finish(true)
	epoch := locks.RecoveryState("C").Epoch
	if err := locks.ExplicitResumeCompleted("C", epoch); err != nil {
		t.Fatal(err)
	}
	locks.RecordResolvedSession("C", "C", epoch)
	launches := 0
	cfg.Spawner = &fakeRPCSpawner{resume: func(_ context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
		launches++
		if req.SessionID != "C" {
			t.Errorf("target=%s", req.SessionID)
		}
		return rendezvous.Entry{}, errors.New("launcher observed")
	}}
	if _, err := hubThreadResume(stale, cfg, nil, appwire.ThreadResumeParams{Ref: "local:A"}); err == nil {
		t.Fatal("stale connection admitted through completed chain")
	}
	if launches != 0 {
		t.Fatalf("stale connection launches=%d", launches)
	}
	if _, err := hubThreadResume(admitSessionConnection(t.Context(), cfg), cfg, nil, appwire.ThreadResumeParams{Ref: "local:A"}); err == nil {
		t.Fatal("expected launcher error")
	}
	if launches != 1 {
		t.Fatalf("fresh connection launches=%d", launches)
	}
}
