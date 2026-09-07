package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/cmd/evener-hub/internal/appsource"
	"primeradiant.com/evener/cmd/evener-hub/internal/daemonprocess"
	"primeradiant.com/evener/cmd/evener-hub/internal/hubcore"
	"primeradiant.com/evener/internal/appserver"
	"primeradiant.com/evener/rendezvous"
)

type forceStopControllerFunc func(daemonprocess.Target) (daemonprocess.Process, error)

func (f forceStopControllerFunc) Open(target daemonprocess.Target) (daemonprocess.Process, error) {
	return f(target)
}

type forceStopProcess struct {
	events           *[]string
	killErr, waitErr error
	onWait           func()
}

func (p *forceStopProcess) Kill() error { *p.events = append(*p.events, "kill"); return p.killErr }
func (p *forceStopProcess) Wait(context.Context) error {
	*p.events = append(*p.events, "wait")
	if p.onWait != nil {
		p.onWait()
	}
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
			err := forceStopThread(t.Context(), hubcore.WebConfig{RunDir: runDir, ResumeLocks: hubcore.NewResumeLocks(), DaemonProcesses: controller}, appwire.ThreadForceStopParams{Ref: "local:" + webTestSessionID}, nil)
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
		{"overlapping aliases", "local:stable", []rendezvous.Entry{{PID: 4242, SessionID: "current", ThreadID: "current", WorkspaceRef: "local:stable"}, {PID: 4243, SessionID: "current", ThreadID: "current"}}},
		{"foreign claim", "local:owner", []rendezvous.Entry{{PID: 4242, SessionID: "owner", SourceID: "remote"}}},
		{"descendant", "local:child", []rendezvous.Entry{{PID: 4242, SessionID: "owner", ThreadID: "owner"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runDir := t.TempDir()
			for _, entry := range tc.entries {
				writeRendezvous(t, runDir, entry)
			}
			controller := forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) {
				if tc.name != "multiple" && tc.name != "overlapping aliases" {
					t.Error("unsafe target reached process controller")
				}
				return nil, errors.New("unexpected")
			})
			if err := forceStopThread(t.Context(), hubcore.WebConfig{RunDir: runDir, ResumeLocks: hubcore.NewResumeLocks(), DaemonProcesses: controller}, appwire.ThreadForceStopParams{Ref: tc.ref}, nil); err == nil {
				t.Fatal("unsafe target accepted")
			}
		})
	}
}

type waitingForceStopProcess struct {
	entered, release chan struct{}
	confirmed        atomic.Bool
}

func (p *waitingForceStopProcess) Kill() error { return nil }
func (p *waitingForceStopProcess) Wait(ctx context.Context) error {
	close(p.entered)
	select {
	case <-p.release:
		p.confirmed.Store(true)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *waitingForceStopProcess) Close() error { return nil }

func TestForceStopRejectsDiscoveryChangeDuringLockedRevalidation(t *testing.T) {
	runDir := t.TempDir()
	entry := rendezvous.Entry{PID: 4242, SessionID: "current", WorkspaceRef: "local:stable"}
	writeRendezvous(t, runDir, entry)
	previous, err := forceStopEntry(runDir, "stable", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry.InstanceID = "replacement"
	writeRendezvous(t, runDir, entry)
	if err := forceStopOwnershipUnchanged(runDir, "stable", previous, nil); err == nil {
		t.Fatal("changed discovery accepted")
	}
}

// A failed ancillary discovery refresh cannot undo the verified exit.
func TestForceStopPreservesSuccessAfterRosterRefreshFailure(t *testing.T) {
	runDir := t.TempDir()
	writeRendezvous(t, runDir, rendezvous.Entry{PID: 4242, SessionID: "owner"})
	roster := hubcore.NewRoster(runDir, failedRPCProber{})
	poked := false
	var events []string
	cfg := hubcore.WebConfig{RunDir: runDir, ResumeLocks: hubcore.NewResumeLocks(), Roster: roster,
		PokeAttention: func() { poked = true },
		DaemonProcesses: forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) {
			return &forceStopProcess{events: &events, onWait: func() {
				if err := os.WriteFile(filepath.Join(runDir, "4243.json"), []byte("{"), 0600); err != nil {
					t.Error(err)
				}
			}}, nil
		}),
	}
	if err := forceStopThread(t.Context(), cfg, appwire.ThreadForceStopParams{Ref: "local:owner"}, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"kill", "wait", "close"}) {
		t.Fatalf("stop events=%v", events)
	}
	if !poked {
		t.Fatal("successful stop did not invalidate attention")
	}
	if err := roster.RefreshAndWait(t.Context()); err == nil {
		t.Fatal("fixture did not fail discovery")
	}
}

type forceStopProberFunc func(rendezvous.Entry) hubcore.ProbeResult

func (f forceStopProberFunc) Probe(entry rendezvous.Entry) hubcore.ProbeResult { return f(entry) }

type fixtureExitProcess struct {
	input   io.Closer
	command *exec.Cmd
}

func (p *fixtureExitProcess) Kill() error                { return p.input.Close() }
func (p *fixtureExitProcess) Wait(context.Context) error { return p.command.Wait() }
func (p *fixtureExitProcess) Close() error               { return nil }

func TestHubForceStopUnconfirmedRootReadAndExplicitResume(t *testing.T) {
	var sessionID string
	shutdownCalls := 0
	cfg, sid, resumes := parityResumeFixture(t, func(daemon *appserver.Server) {
		appserver.HandleTyped(daemon.Router(), appwire.MethodThreadRead, func(_ context.Context, params appwire.ThreadReadParams) (appwire.ThreadReadResponse, error) {
			return appwire.ThreadReadResponse{Thread: appwire.Thread{ID: sessionID, SessionID: sessionID, Source: "local", Status: appwire.ThreadStatus{Type: "idle"}, Evener: appwire.EvenerThread{Ref: params.Ref, InstanceID: sessionID, Capabilities: appwire.ThreadCapabilities{Send: true, Shutdown: true}}}}, nil
		})
		appserver.HandleTyped(daemon.Router(), appwire.MethodThreadShutdown, func(context.Context, appwire.ThreadShutdownParams) (appwire.EmptyResponse, error) {
			shutdownCalls++
			return appwire.EmptyResponse{}, nil
		})
	})
	sessionID = sid
	// cat owns only this test's pipe and exits when the fake controller closes
	// it. Real process liveness drives the roster's unconfirmed/crash states.
	command := exec.CommandContext(t.Context(), "cat")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		if command.ProcessState == nil {
			_ = command.Wait()
		}
	})
	pe, _ := cfg.Past.Find(sessionID)
	entry := rendezvous.Entry{PID: command.Process.Pid, SessionID: sessionID, ThreadID: sessionID, WorkspaceRef: "local:" + sessionID, StateDir: pe.StateDir, Protocol: appwire.ProtocolVersion, StartedAt: time.Now()}
	writeRendezvous(t, cfg.RunDir, entry)
	cfg.Roster = hubcore.NewRoster(cfg.RunDir, forceStopProberFunc(func(e rendezvous.Entry) hubcore.ProbeResult {
		if e.PID == entry.PID {
			return hubcore.ProbeResult{}
		}
		return hubcore.ProbeResult{OK: true, SessionID: sessionID, Status: "idle"}
	}))
	cfg.ResumeLocks = hubcore.NewResumeLocks()
	opens := 0
	cfg.DaemonProcesses = forceStopControllerFunc(func(target daemonprocess.Target) (daemonprocess.Process, error) {
		opens++
		if target.PID == 106 {
			return &forceStopProcess{events: new([]string)}, nil
		}
		if target.PID == command.Process.Pid && command.ProcessState != nil {
			return nil, daemonprocess.ErrExited
		}
		if target.PID != command.Process.Pid {
			t.Fatalf("unexpected target: %+v", target)
		}
		return &fixtureExitProcess{input: input, command: command}, nil
	})
	hub := newHubRPCTestServer(t, cfg)
	defer hub.Close()
	client := dialHubRPC(t, hub)
	defer client.Close()
	if _, err := client.Initialize(t.Context(), appwire.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	ref := "local:" + sessionID
	if _, err := client.ThreadRead(t.Context(), appwire.ThreadReadParams{Ref: ref, IncludeTurns: true}); err == nil {
		t.Fatal("unconfirmed live root unexpectedly hydrated")
	}
	var stopped appwire.EmptyResponse
	if err := client.Request(t.Context(), "evener/thread/forceStop", appwire.ThreadForceStopParams{Ref: ref}, &stopped); err != nil {
		t.Fatal(err)
	}
	read, err := client.ThreadRead(t.Context(), appwire.ThreadReadParams{Ref: ref, IncludeTurns: true})
	if err != nil {
		t.Fatal(err)
	}
	if read.Thread.Status.Type != "notLoaded" || len(read.Thread.Turns) != 2 || read.Thread.Evener.Capabilities.Send || !read.Thread.Evener.ResumeRequired {
		t.Fatalf("stopped read: %+v", read.Thread)
	}
	if *resumes != 0 {
		t.Fatal("stop/read automatically resumed")
	}
	entries, err := rendezvous.ListStrict(cfg.RunDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("retained crash marker=%v err=%v", entries, err)
	}
	if _, err := client.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if *resumes != 1 {
		t.Fatalf("explicit resumes=%d", *resumes)
	}
	if err := client.ThreadShutdown(t.Context(), appwire.ThreadShutdownParams{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	if shutdownCalls != 1 || opens != 1 {
		t.Fatalf("normal shutdown route=%d process opens=%d", shutdownCalls, opens)
	}
	if err := client.Request(t.Context(), appwire.MethodEvenerThreadForceStop, appwire.ThreadForceStopParams{Ref: ref}, nil); err != nil {
		t.Fatalf("stop after explicit resume: %v", err)
	}
	if *resumes != 1 {
		t.Fatalf("second stop resumed: %d", *resumes)
	}
}

func TestForceStopSerializesResumeAndDeletionEntryPoints(t *testing.T) {
	for _, operation := range []string{"resume", "delete"} {
		for _, savedAlias := range []string{"stable", "current"} {
			t.Run(operation+"/"+savedAlias, func(t *testing.T) {
				stateDir := filepath.Join(t.TempDir(), "force-stop-0000000000")
				sessionID := buildRPCParentSession(t, stateDir)
				past := hubcore.NewPastIndex(stateDir)
				if _, err := past.Rebuild(); err != nil {
					t.Fatal(err)
				}
				currentID, stableID := sessionID, "02wMz5Txv1C3Hut0M8GCeC"
				if savedAlias == "stable" {
					currentID, stableID = stableID, currentID
				}
				runDir := t.TempDir()
				writeRendezvous(t, runDir, rendezvous.Entry{PID: 4242, SessionID: currentID, ThreadID: currentID, WorkspaceRef: "local:" + stableID, StateDir: stateDir})
				process := &waitingForceStopProcess{entered: make(chan struct{}), release: make(chan struct{})}
				spawned := make(chan struct{}, 1)
				cfg := hubcore.WebConfig{RunDir: runDir, Past: past, ResumeLocks: hubcore.NewResumeLocks(),
					DaemonProcesses: forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) { return process, nil }),
					Spawner: &fakeRPCSpawner{resume: func(context.Context, hubcore.ResumeRequest) (rendezvous.Entry, error) {
						if !process.confirmed.Load() {
							t.Error("resume spawned before exit confirmation")
						}
						spawned <- struct{}{}
						return rendezvous.Entry{}, errors.New("fixture spawn boundary")
					}},
				}
				web := NewWebServer(cfg)
				stopped := make(chan error, 1)
				go func() {
					stopped <- forceStopThread(t.Context(), cfg, appwire.ThreadForceStopParams{Ref: "local:" + stableID}, nil)
				}()
				<-process.entered
				for _, alias := range []string{currentID, stableID} {
					lock := cfg.ResumeLocks.For(alias)
					if lock.TryLock() {
						lock.Unlock()
						t.Errorf("%s lock released before exit", alias)
					}
				}
				started, finished := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(finished)
					close(started)
					if operation == "resume" {
						if _, err := hubThreadResume(t.Context(), cfg, nil, appwire.ThreadResumeParams{Ref: "local:" + sessionID}); err == nil {
							t.Error("fixture spawn error lost")
						}
					} else {
						response, err := web.sessionDelete(t.Context(), appwire.SessionDeleteParams{Ref: "local:" + sessionID})
						if err != nil || len(response.Deleted) != 1 {
							t.Errorf("delete response=%+v err=%v", response, err)
						}
						if !process.confirmed.Load() {
							t.Error("deletion completed before exit confirmation")
						}
					}
				}()
				<-started
				if _, err := os.Stat(filepath.Join(stateDir, "sessions", sessionID+".transcript.jsonl")); err != nil {
					t.Fatal(err)
				}
				select {
				case <-spawned:
					t.Error("spawned while force stop holds ownership")
				default:
				}
				close(process.release)
				if err := <-stopped; err != nil {
					t.Fatal(err)
				}
				<-finished
				if operation == "resume" {
					select {
					case <-spawned:
					default:
						t.Error("resume never reached spawner after exit")
					}
				} else if _, err := os.Stat(filepath.Join(stateDir, "sessions", sessionID+".transcript.jsonl")); !os.IsNotExist(err) {
					t.Fatalf("delete did not remove saved data: %v", err)
				}
			})
		}
	}
}

func TestHubForceStopInterruptsStalledRPCOnSameConnection(t *testing.T) {
	for _, method := range []string{appwire.MethodThreadShutdown, appwire.MethodThreadModelSet} {
		t.Run(method, func(t *testing.T) {
			var sessionID string
			entered := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			cfg, sid, resumes := parityResumeFixture(t, func(daemon *appserver.Server) {
				appserver.HandleTyped(daemon.Router(), appwire.MethodThreadRead, func(_ context.Context, params appwire.ThreadReadParams) (appwire.ThreadReadResponse, error) {
					return appwire.ThreadReadResponse{Thread: appwire.Thread{ID: sessionID, SessionID: sessionID, Source: "local", Evener: appwire.EvenerThread{Ref: params.Ref, InstanceID: sessionID, Capabilities: appwire.ThreadCapabilities{Shutdown: true, ChangeModel: true}}}}, nil
				})
				daemon.Router().Handle(method, func(ctx context.Context, _ json.RawMessage) (any, error) {
					close(entered)
					select {
					case <-ctx.Done():
					case <-release:
					}
					return appwire.EmptyResponse{}, ctx.Err()
				})
			})
			sessionID = sid
			cfg.ResumeLocks = hubcore.NewResumeLocks()
			var events []string
			cfg.DaemonProcesses = forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) {
				return &forceStopProcess{events: &events}, nil
			})
			hub := newHubRPCTestServer(t, cfg)
			defer hub.Close()
			client := dialHubRPC(t, hub)
			defer client.Close()
			if _, err := client.Initialize(t.Context(), appwire.InitializeParams{}); err != nil {
				t.Fatal(err)
			}
			ref := "local:" + sessionID
			if _, err := client.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: ref}); err != nil {
				t.Fatal(err)
			}
			shutdown := make(chan error, 1)
			go func() {
				shutdown <- client.Request(t.Context(), method, appwire.ThreadModelSetParams{Ref: ref, ModelProvider: "test", Model: "test"}, nil)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown never reached daemon")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := client.Request(ctx, appwire.MethodEvenerThreadForceStop, appwire.ThreadForceStopParams{Ref: ref}, nil); err != nil {
				t.Fatalf("force stop behind stalled shutdown: %v", err)
			}
			select {
			case err := <-shutdown:
				if err == nil {
					t.Fatal("interrupted RPC returned success")
				}
			case <-ctx.Done():
				t.Fatal("stalled shutdown did not return")
			}
			if !reflect.DeepEqual(events, []string{"kill", "wait", "close"}) {
				t.Fatalf("process events=%v", events)
			}
			if *resumes != 1 {
				t.Fatalf("recovery automatically resumed session: %d", *resumes)
			}
		})
	}
}

func TestForceStopSkipsVerifiedExitedClaims(t *testing.T) {
	for _, aliasOnly := range []bool{false, true} {
		t.Run(strconv.FormatBool(aliasOnly), func(t *testing.T) {
			runDir := t.TempDir()
			old := rendezvous.Entry{PID: 4242, SessionID: "stable", ThreadID: "current", StateDir: t.TempDir(), StartedAt: time.Now()}
			writeRendezvous(t, runDir, old)
			var events []string
			dead := map[int]bool{}
			cfg := hubcore.WebConfig{RunDir: runDir, ResumeLocks: hubcore.NewResumeLocks(), DaemonProcesses: forceStopControllerFunc(func(target daemonprocess.Target) (daemonprocess.Process, error) {
				if dead[target.PID] {
					return nil, daemonprocess.ErrExited
				}
				return &forceStopProcess{events: &events, onWait: func() { dead[target.PID] = true }}, nil
			})}
			params := appwire.ThreadForceStopParams{Ref: "local:stable"}
			if err := forceStopThread(t.Context(), cfg, params, nil); err != nil {
				t.Fatal(err)
			}
			resumed := old
			resumed.PID++
			if aliasOnly {
				resumed.SessionID = "resumed"
				params.Ref = "local:resumed"
			}
			writeRendezvous(t, runDir, resumed)
			if err := forceStopThread(t.Context(), cfg, params, nil); err != nil {
				t.Fatalf("second force stop: %v", err)
			}
			if err := forceStopThread(t.Context(), cfg, params, nil); err != nil {
				t.Fatalf("retry with all owners exited: %v", err)
			}
			entries, err := rendezvous.ListStrict(runDir)
			if err != nil || len(entries) != 2 {
				t.Fatalf("crash markers not preserved: %v %v", entries, err)
			}
		})
	}
}

func TestHubForceStopInterruptsStalledSpawnedResumeRead(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	cfg, sessionID, resumes := parityResumeFixture(t, func(daemon *appserver.Server) {
		appserver.HandleTyped(daemon.Router(), appwire.MethodThreadRead, func(ctx context.Context, _ appwire.ThreadReadParams) (appwire.ThreadReadResponse, error) {
			close(entered)
			select {
			case <-ctx.Done():
			case <-release:
			}
			return appwire.ThreadReadResponse{}, ctx.Err()
		})
	})
	spawner := cfg.Spawner.(*fakeRPCSpawner)
	spawn := spawner.resume
	spawner.resume = func(ctx context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
		entry, err := spawn(ctx, req)
		entry.StartedAt = time.Now()
		entry.StateDir = req.StateDir
		writeRendezvous(t, cfg.RunDir, entry)
		return entry, err
	}
	cfg.Roster = hubcore.NewRoster(cfg.RunDir, forceStopProberFunc(func(rendezvous.Entry) hubcore.ProbeResult { return hubcore.ProbeResult{} }))
	cfg.ResumeLocks = hubcore.NewResumeLocks()
	var events []string
	cfg.DaemonProcesses = forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) {
		return &forceStopProcess{events: &events}, nil
	})
	hub := newHubRPCTestServer(t, cfg)
	defer hub.Close()
	client := dialHubRPC(t, hub)
	defer client.Close()
	if _, err := client.Initialize(t.Context(), appwire.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	ref := "local:" + sessionID
	resume := make(chan error, 1)
	go func() {
		_, err := client.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: ref})
		resume <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("spawned read never reached daemon")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := client.Request(ctx, appwire.MethodEvenerThreadForceStop, appwire.ThreadForceStopParams{Ref: ref}, nil); err != nil {
		t.Fatalf("force stop behind spawned read: %v", err)
	}
	select {
	case err := <-resume:
		if err == nil {
			t.Fatal("canceled resume succeeded")
		}
	case <-ctx.Done():
		t.Fatal("resume retained ownership")
	}
	if *resumes != 1 {
		t.Fatalf("spawns=%d", *resumes)
	}
	if !reflect.DeepEqual(events, []string{"kill", "wait", "close"}) {
		t.Fatalf("process events=%v", events)
	}
}

func TestHubForceStopRejectsWaitingMutationUntilExplicitResume(t *testing.T) {
	for _, method := range []string{appwire.MethodThreadModelSet, appwire.MethodThreadCompactStart} {
		t.Run(method, func(t *testing.T) {
			var sessionID string
			var stopped atomic.Bool
			cfg, sid, resumes := parityResumeFixture(t, func(daemon *appserver.Server) {
				appserver.HandleTyped(daemon.Router(), appwire.MethodThreadRead, func(_ context.Context, params appwire.ThreadReadParams) (appwire.ThreadReadResponse, error) {
					if stopped.Load() {
						return appwire.ThreadReadResponse{}, appwire.SessionUnavailable("daemon exited")
					}
					return appwire.ThreadReadResponse{Thread: appwire.Thread{ID: sessionID, SessionID: sessionID, Source: "local", Status: appwire.ThreadStatus{Type: "idle"}, Evener: appwire.EvenerThread{Ref: params.Ref, InstanceID: sessionID, Capabilities: appwire.ThreadCapabilities{Send: true, ChangeModel: true, Compact: true}}}}, nil
				})
				daemon.Router().Handle(method, func(context.Context, json.RawMessage) (any, error) { return appwire.EmptyResponse{}, nil })
			})
			sessionID = sid
			spawner := cfg.Spawner.(*fakeRPCSpawner)
			spawn := spawner.resume
			spawner.resume = func(ctx context.Context, req hubcore.ResumeRequest) (rendezvous.Entry, error) {
				stopped.Store(false)
				entry, err := spawn(ctx, req)
				process := exec.CommandContext(ctx, "true")
				if err := process.Run(); err != nil {
					return rendezvous.Entry{}, err
				}
				if err := os.Remove(filepath.Join(cfg.RunDir, strconv.Itoa(entry.PID)+".json")); err != nil {
					return rendezvous.Entry{}, err
				}
				entry.PID = process.Process.Pid
				entry.StartedAt = time.Now()
				writeRendezvous(t, cfg.RunDir, entry)
				return entry, err
			}
			cfg.Roster = hubcore.NewRoster(cfg.RunDir, forceStopProberFunc(func(rendezvous.Entry) hubcore.ProbeResult {
				if stopped.Load() {
					return hubcore.ProbeResult{}
				}
				return hubcore.ProbeResult{OK: true, SessionID: sessionID, Status: "idle"}
			}))
			cfg.ResumeLocks = hubcore.NewResumeLocks()
			waiting := make(chan struct{})
			exit := make(chan struct{})
			process := &waitingForceStopProcess{entered: waiting, release: exit}
			cfg.DaemonProcesses = forceStopControllerFunc(func(daemonprocess.Target) (daemonprocess.Process, error) { return process, nil })
			hub := newHubRPCTestServer(t, cfg)
			defer hub.Close()
			client := dialHubRPC(t, hub)
			defer client.Close()
			if _, err := client.Initialize(t.Context(), appwire.InitializeParams{}); err != nil {
				t.Fatal(err)
			}
			ref := "local:" + sessionID
			if _, err := client.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: ref}); err != nil {
				t.Fatal(err)
			}
			stop := make(chan error, 1)
			go func() {
				stop <- client.Request(t.Context(), appwire.MethodEvenerThreadForceStop, appwire.ThreadForceStopParams{Ref: ref}, nil)
			}()
			select {
			case <-waiting:
			case err := <-stop:
				t.Fatalf("force stop returned before confirmation: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("force stop did not reach exit confirmation")
			}
			mutation := make(chan error, 1)
			started := make(chan struct{})
			sources := newHubSourceRegistry(cfg)
			go func() {
				close(started)
				if method == appwire.MethodThreadModelSet {
					mutation <- setThreadModelWithResume(t.Context(), cfg, sources, appwire.ThreadModelSetParams{Ref: ref, Model: "test", ModelProvider: "test"})
				} else {
					mutation <- compactThreadWithResume(t.Context(), cfg, sources, appwire.ThreadCompactStartParams{Ref: ref})
				}
			}()
			<-started
			stopped.Store(true)
			close(exit)
			if err := <-stop; err != nil {
				t.Fatal(err)
			}
			if err := <-mutation; err == nil {
				t.Fatal("waiting mutation resumed after force stop")
			}
			if *resumes != 1 {
				t.Fatalf("automatic resumes after stop=%d", *resumes-1)
			}
			read, err := client.ThreadRead(t.Context(), appwire.ThreadReadParams{Ref: ref})
			if err != nil {
				t.Fatal(err)
			}
			if read.Thread.Status.Type != "notLoaded" || !read.Thread.Evener.ResumeRequired || read.Thread.Evener.Capabilities.Send {
				t.Fatalf("stopped snapshot=%+v", read.Thread)
			}
			// An outside controller can restart the process without acknowledging
			// the hub's action fence. Live reads must still advertise that fence.
			req, err := resumeRequestForConfig(cfg, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cfg.Spawner.Resume(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if err := cfg.Roster.RefreshAndWait(t.Context()); err != nil {
				t.Fatal(err)
			}
			live, err := client.ThreadRead(t.Context(), appwire.ThreadReadParams{Ref: ref})
			if err != nil {
				t.Fatal(err)
			}
			if live.Thread.Status.Type != "idle" || !live.Thread.Evener.ResumeRequired || live.Thread.Evener.Capabilities.Send {
				t.Fatalf("live fenced snapshot=%+v", live.Thread)
			}
			if _, err := client.ThreadResume(t.Context(), appwire.ThreadResumeParams{Ref: ref}); err != nil {
				t.Fatalf("explicit resume: %v", err)
			}
			fresh, err := client.ThreadRead(t.Context(), appwire.ThreadReadParams{Ref: ref})
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Thread.Evener.ResumeRequired || !fresh.Thread.Evener.Capabilities.Send {
				t.Fatalf("explicit resume did not clear live fence: %+v", fresh.Thread)
			}

			if err := client.Request(t.Context(), method, appwire.ThreadModelSetParams{Ref: ref, Model: "test", ModelProvider: "test"}, nil); err != nil {
				t.Fatalf("fresh action after explicit resume: %v", err)
			}
		})
	}
}

func TestSessionRecoveryRejectsOldActionsAfterExplicitResume(t *testing.T) {
	cfg := hubcore.WebConfig{ResumeLocks: hubcore.NewResumeLocks()}
	oldEpoch := sessionRecoveryState(cfg, "local:stable", "").Epoch
	finish := cfg.ResumeLocks.BeginForceStop([]string{"stable", "current"})
	finish(true)
	epoch := sessionRecoveryState(cfg, "local:current", "").Epoch
	cfg.ResumeLocks.ExplicitResumeCompleted("current", epoch)
	if state := sessionRecoveryState(cfg, "local:stable", ""); state.ResumeRequired || state.Stopping != 0 {
		t.Fatalf("stable alias still fenced after explicit resume: %+v", state)
	}
	if err := sessionActionRecoveryError(cfg, "local:stable", "", oldEpoch); err == nil {
		t.Fatal("old ownership waiter admitted after explicit resume")
	}
	if err := sessionActionRecoveryError(cfg, "local:current", "", epoch); err != nil {
		t.Fatalf("fresh action refused: %v", err)
	}
	finish = cfg.ResumeLocks.BeginForceStop([]string{"stable", "current"})
	finish(false)
	if state := sessionRecoveryState(cfg, "local:stable", ""); state.ResumeRequired || state.Stopping != 0 {
		t.Fatalf("failed stop declared session stopped: %+v", state)
	}
}

func TestTurnStartDoesNotRetryRecoveryRejectionAfterExplicitResume(t *testing.T) {
	cfg := hubcore.WebConfig{ResumeLocks: hubcore.NewResumeLocks()}
	const ref = "local:recovery-waiter"
	oldEpoch := sessionRecoveryState(cfg, ref, "").Epoch
	finish := cfg.ResumeLocks.BeginForceStop([]string{"recovery-waiter"})
	finish(true)
	cfg.ResumeLocks.ExplicitResumeCompleted("recovery-waiter", cfg.ResumeLocks.RecoveryState("recovery-waiter").Epoch)
	stale := sessionActionRecoveryError(cfg, ref, "", oldEpoch)
	if stale == nil {
		t.Fatal("fixture did not reject the stale admission")
	}
	oldResolve, oldResume := resolveTurnStartSource, resumeTurnStartThread
	t.Cleanup(func() { resolveTurnStartSource, resumeTurnStartThread = oldResolve, oldResume })
	resolved, resumed, submitted := 0, 0, 0
	source := &scriptedAppSource{id: "local", startTurn: func(context.Context, appwire.TurnStartParams) (appwire.TurnStartResponse, error) {
		submitted++
		return appwire.TurnStartResponse{}, nil
	}}
	resolveTurnStartSource = func(*appsource.Registry, string, string) (appsource.Source, error) {
		resolved++
		if resolved == 1 {
			return nil, stale
		}
		return source, nil
	}
	resumeTurnStartThread = func(context.Context, hubcore.WebConfig, *appsource.Registry, appwire.ThreadResumeParams) (appwire.ThreadResumeResponse, error) {
		resumed++
		return appwire.ThreadResumeResponse{}, nil
	}
	server := newHubAppServer(cfg, appsource.NewRegistry())
	_, err := exactDispatch(t.Context(), t, server, appwire.MethodTurnStart, appwire.TurnStartParams{Ref: ref, ClientMutationID: "pending-before-recovery", Input: []appwire.InputItem{{Type: "text", Text: "old queued input"}}})
	if err == nil {
		t.Fatal("stale turn/start was accepted after explicit resume")
	}
	wire := appserver.WireError(err)
	if wire.Code != appwire.CodeUnavailable || evenerErrorInfoFromData(wire.Data) != string(appwire.ErrorActionUnavailable) {
		t.Fatalf("recovery rejection lost wire classification: %+v", wire)
	}
	if wire.Message != stale.Error() {
		t.Fatalf("wire message changed: %q vs %q", wire.Message, stale.Error())
	}
	if resumed != 0 || submitted != 0 || resolved != 1 {
		t.Fatalf("stale input retried: resolutions=%d resumes=%d submissions=%d", resolved, resumed, submitted)
	}
}
