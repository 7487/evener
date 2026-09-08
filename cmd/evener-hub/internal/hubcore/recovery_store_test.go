package hubcore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/spf13/afero"
)

func TestPersistentRecoverySurvivesRecreationAndClearsOnlyItsGroup(t *testing.T) {
	root := t.TempDir()
	locks, err := NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	finish := locks.BeginForceStop([]string{"A", "B"})
	if err := locks.PersistForceStop([]string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	finish(false) // Committed intent survives a failed signal.
	locks, err = NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"A", "B"} {
		if state := locks.RecoveryState(id); !state.ResumeRequired || state.Stopping != 0 {
			t.Fatalf("lost intent %s: %+v", id, state)
		}
	}
	finish = locks.BeginForceStop([]string{"B", "C"})
	if err := locks.PersistForceStop([]string{"B", "C"}); err != nil {
		t.Fatal(err)
	}
	finish(true)
	locks, err = NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := locks.ExplicitResumeCompleted("A", locks.RecoveryState("A").Epoch); err != nil {
		t.Fatal(err)
	}
	locks, err = NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	if locks.RecoveryState("A").ResumeRequired || !locks.RecoveryState("B").ResumeRequired || !locks.RecoveryState("C").ResumeRequired {
		t.Fatal("older group clear removed newer overlap")
	}
	if err := locks.ExplicitResumeCompleted("B", locks.RecoveryState("B").Epoch); err != nil {
		t.Fatal(err)
	}
	locks, err = NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	if locks.RecoveryState("B").ResumeRequired || locks.RecoveryState("C").ResumeRequired {
		t.Fatal("explicit resume failed to durably clear aliases")
	}
}

func TestPersistentRecoveryRejectsCorruptAuthority(t *testing.T) {
	for _, raw := range []string{`{`, `{"version":2,"records":[]}`, `{"version":1,"records":[],"unknown":true}`, `{"version":1,"records":[]} {}`, `{"version":1,"records":[{"alias":"A","group":""}]}`, `{"version":1,"records":[{"alias":"A","group":"one"},{"alias":"A","group":"two"}]}`} {
		t.Run(raw, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "recovery"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "recovery", "state.json"), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewPersistentResumeLocks(root); err == nil {
				t.Fatal("corrupt authority silently reset")
			}
		})
	}
	if _, err := NewPersistentResumeLocks(""); err == nil {
		t.Fatal("persistent constructor accepted missing root")
	}
}

func TestPersistentRecoveryWriteFailurePolicy(t *testing.T) {
	for _, clear := range []bool{false, true} {
		for _, renamed := range []bool{false, true} {
			t.Run(map[bool]string{false: "intent", true: "clear"}[clear]+map[bool]string{false: " before rename", true: " after rename"}[renamed], func(t *testing.T) {
				root := t.TempDir()
				locks, err := NewPersistentResumeLocks(root)
				if err != nil {
					t.Fatal(err)
				}
				finish := locks.BeginForceStop([]string{"A", "B"})
				if clear {
					if err := locks.PersistForceStop([]string{"A", "B"}); err != nil {
						t.Fatal(err)
					}
					finish(true)
				}
				boom := errors.New("disk failure")
				fail := func() error { return boom }
				if renamed {
					locks.store.faults.AfterRename = fail
				} else {
					locks.store.faults.BeforeRename = fail
				}
				if clear {
					err = locks.ExplicitResumeCompleted("A", locks.RecoveryState("A").Epoch)
				} else {
					err = locks.PersistForceStop([]string{"A", "B"})
					finish(false)
				}
				if !errors.Is(err, boom) {
					t.Fatalf("lost write error: %v", err)
				}
				if got := locks.RecoveryState("A").ResumeRequired; got != (clear || renamed) {
					t.Fatalf("memory obligation=%v", got)
				}
				reopened, err := NewPersistentResumeLocks(root)
				if err != nil {
					t.Fatal(err)
				}
				wantDisk := renamed != clear
				if got := reopened.RecoveryState("A").ResumeRequired; got != wantDisk {
					t.Fatalf("reopened obligation=%v want=%v", got, wantDisk)
				}
				locks.store.faults = recoveryStoreFaults{}
				if clear {
					if err := locks.ExplicitResumeCompleted("A", locks.RecoveryState("A").Epoch); err != nil {
						t.Fatal(err)
					}
					if locks.RecoveryState("A").ResumeRequired {
						t.Fatal("clear retry retained fence")
					}
				}
			})
		}
	}
}

func TestRecoveryAdmissionRemainsResponsiveDuringDurableClear(t *testing.T) {
	root := t.TempDir()
	locks, err := NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	finish := locks.BeginForceStop([]string{"A", "B"})
	if err := locks.PersistForceStop([]string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	finish(true)
	entered, release := make(chan struct{}), make(chan struct{})
	locks.store.faults.BeforeRename = func() error { close(entered); <-release; return nil }
	done := make(chan error, 1)
	go func() { done <- locks.ExplicitResumeCompleted("A", locks.RecoveryState("A").Epoch) }()
	<-entered
	admission := make(chan func(bool), 1)
	go func() {
		next := locks.BeginForceStop([]string{"B", "C"})
		_ = locks.RecoverySequence()
		_ = locks.RecoveryState("B")
		admission <- next
	}()
	var finishNew func(bool)
	select {
	case finishNew = <-admission:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("disk clear blocked admission")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !locks.RecoveryState("B").ResumeRequired || locks.RecoveryState("B").Stopping != 1 {
		t.Fatal("old clear removed newer memory fence")
	}
	locks.store.faults = recoveryStoreFaults{}
	if err := locks.PersistForceStop([]string{"B", "C"}); err != nil {
		t.Fatal(err)
	}
	finishNew(true)
	reopened, err := NewPersistentResumeLocks(root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.RecoveryState("A").ResumeRequired || !reopened.RecoveryState("B").ResumeRequired || !reopened.RecoveryState("C").ResumeRequired {
		t.Fatal("new commit lost overlap after held clear")
	}
}

// recoverySyncFS observes real directory and temp-file sync boundaries while
// retaining ordinary file contents and atomic rename behavior.
type recoverySyncFS struct {
	afero.Fs
	sync func(string) error
}
type recoverySyncFile struct {
	afero.File
	sync func(string) error
}

func (f recoverySyncFile) Sync() error {
	if err := f.sync(f.Name()); err != nil {
		return err
	}
	return f.File.Sync()
}
func (fs recoverySyncFS) Open(name string) (afero.File, error) {
	file, err := fs.Fs.Open(name)
	if err != nil {
		return nil, err
	}
	return recoverySyncFile{file, fs.sync}, nil
}
func (fs recoverySyncFS) OpenFile(name string, flag int, mode os.FileMode) (afero.File, error) {
	file, err := fs.Fs.OpenFile(name, flag, mode)
	if err != nil {
		return nil, err
	}
	return recoverySyncFile{file, fs.sync}, nil
}

func TestRecoveryIntentRequiresFileAndDirectoryDurability(t *testing.T) {
	for _, stage := range []string{"new directory parent", "temporary file", "renamed directory"} {
		t.Run(stage, func(t *testing.T) {
			base := afero.NewMemMapFs()
			boom := errors.New("sync unsupported")
			fail := true
			observed := map[string]int{}
			fs := recoverySyncFS{Fs: base, sync: func(path string) error {
				observed[path]++
				matches := stage == "new directory parent" && path == "/state" || stage == "temporary file" && strings.Contains(filepath.Base(path), ".tmp-") || stage == "renamed directory" && path == "/state/recovery"
				if fail && matches {
					return boom
				}
				return nil
			}}
			store, err := openRecoveryStore(fs, "/state")
			if err != nil {
				t.Fatal(err)
			}
			locks := NewResumeLocks()
			locks.store = store
			finish := locks.BeginForceStop([]string{"A"})
			if err := locks.PersistForceStop([]string{"A"}); !errors.Is(err, boom) {
				t.Fatalf("sync failure did not prohibit signal: %v", err)
			}
			if got := locks.RecoveryState("A").ResumeRequired; got != (stage == "renamed directory") {
				t.Fatalf("uncertain intent memory=%v", got)
			}
			fail = false
			if err := locks.PersistForceStop([]string{"A"}); err != nil {
				t.Fatal(err)
			}
			finish(false)
			if observed["/"] == 0 || observed["/state"] < 2 || observed["/state/recovery"] == 0 {
				t.Fatalf("missing first-use or retry directory syncs: %v", observed)
			}
			reopened, err := openRecoveryStore(base, "/state")
			if err != nil || reopened.state["A"] == "" {
				t.Fatalf("retry did not preserve intent: store=%+v err=%v", reopened, err)
			}
		})
	}
}

func TestExplicitResumeDoesNotClearChangedEpoch(t *testing.T) {
	locks, err := NewPersistentResumeLocks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	finish := locks.BeginForceStop([]string{"A"})
	if err := locks.PersistForceStop([]string{"A"}); err != nil {
		t.Fatal(err)
	}
	finish(true)
	epoch := locks.RecoveryState("A").Epoch
	finish = locks.BeginForceStop([]string{"A"})
	if err := locks.PersistForceStop([]string{"A"}); err != nil {
		t.Fatal(err)
	}
	finish(true)
	if err := locks.ExplicitResumeCompleted("A", epoch); err != nil {
		t.Fatal(err)
	}
	if !locks.RecoveryState("A").ResumeRequired {
		t.Fatal("stale completion cleared newer durable intent")
	}
}

func TestPersistentRecoveryFailsOnUnreadableState(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "recovery", "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPersistentResumeLocks(root); err == nil {
		t.Fatal("unreadable authority silently reset")
	}
}

func TestRecoveryIntentNormalizesAliasesAndPreservesMemorySemantics(t *testing.T) {
	locks := NewResumeLocks()
	finish := locks.BeginForceStop([]string{"A", "B"})
	if err := locks.PersistForceStop([]string{"B", "A", "B"}); err != nil {
		t.Fatal(err)
	}
	finish(false)
	if !locks.RecoveryState("A").ResumeRequired || !locks.RecoveryState("B").ResumeRequired {
		t.Fatal("failed signal discarded committed memory intent")
	}
	if err := locks.ExplicitResumeCompleted("A", locks.RecoveryState("A").Epoch); err != nil {
		t.Fatal(err)
	}
	if locks.RecoveryState("A").ResumeRequired || locks.RecoveryState("B").ResumeRequired {
		t.Fatal("memory aliases failed to clear")
	}
	for _, aliases := range [][]string{nil, {""}, {"../A"}, {" local:A "}} {
		if err := locks.PersistForceStop(aliases); err == nil {
			t.Fatalf("invalid authority accepted: %v", aliases)
		}
	}
}
