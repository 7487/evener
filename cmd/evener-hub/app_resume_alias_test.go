package hub

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/cmd/evener-hub/internal/hubcore"
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
			if err := locks.PersistForceStop(aliases); err != nil {
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

func TestResumeMissingMarkerDoesNotGuessRecoveryTarget(t *testing.T) {
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
				finish := locks.BeginForceStop(aliases)
				if err := locks.PersistForceStop(aliases); err != nil {
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
				cfg := hubcore.WebConfig{RunDir: t.TempDir(), ResumeLocks: locks, Spawner: &fakeRPCSpawner{resume: func(context.Context, hubcore.ResumeRequest) (rendezvous.Entry, error) {
					called = true
					return rendezvous.Entry{}, errors.New("launcher observed")
				}}}
				_, err = hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:a-stable"})
				if err == nil {
					t.Fatal("expected refusal or launcher error")
				}
				if called == multiple {
					t.Fatalf("launch=%v multiple aliases=%v", called, multiple)
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
	finish := locks.BeginForceStop([]string{stable, current})
	if err := locks.PersistForceStop([]string{stable, current}); err != nil {
		t.Fatal(err)
	}
	finish(true)
	// Restart recovery authority while preserving the daemon's stable/current identity.
	cfg.ResumeLocks, err = hubcore.NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	writeRendezvous(t, cfg.RunDir, rendezvous.Entry{PID: 106, StartedAt: time.Now(), SessionID: current, ThreadID: current, WorkspaceRef: "local:" + stable})
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
