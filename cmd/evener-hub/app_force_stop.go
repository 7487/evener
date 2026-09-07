package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/cmd/evener-hub/internal/daemonprocess"
	"primeradiant.com/evener/cmd/evener-hub/internal/hubcore"
	"primeradiant.com/evener/rendezvous"
)

// forceStopThread bypasses daemon RPC only after verifying the local process.
// The session remains reserved until the exact process has exited.
func forceStopThread(ctx context.Context, cfg hubcore.WebConfig, params appwire.ThreadForceStopParams) error {
	ref, err := appwire.ParseRef(params.Ref)
	if err != nil || ref.SourceID != "local" {
		return appwire.InvalidParams("force stop requires a local session ref")
	}
	if cfg.RunDir == "" || cfg.ResumeLocks == nil {
		return appwire.Unavailable("local session ownership is not configured")
	}
	entry, err := forceStopEntry(cfg.RunDir, ref.ThreadID)
	if err != nil {
		return appwire.Unavailable(err.Error())
	}
	// Clear gives one daemon stable and current session aliases. Lock both so
	// resume or deletion through either alias cannot race exit confirmation.
	aliases := []string{ref.ThreadID, entry.SessionID, entry.ThreadID}
	if workspace, e := appwire.ParseRef(entry.WorkspaceRef); e == nil && workspace.SourceID == "local" {
		aliases = append(aliases, workspace.ThreadID)
	}
	slices.Sort(aliases)
	aliases = slices.Compact(aliases)
	for _, id := range aliases {
		if id != "" {
			cfg.ResumeLocks.For(id).Lock()
			defer cfg.ResumeLocks.For(id).Unlock()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := forceStopEntry(cfg.RunDir, ref.ThreadID)
	if err != nil {
		return appwire.Unavailable(err.Error())
	}
	if current != entry {
		return appwire.Unavailable("daemon ownership changed; refresh the session before force stopping")
	}
	if err := deletionFenceError(cfg, params.Ref, ref.ThreadID, ""); err != nil {
		return err
	}
	controller := cfg.DaemonProcesses
	if controller == nil {
		controller = daemonprocess.NewController()
	}
	sessionID := entry.SessionID
	if sessionID == "" {
		sessionID = entry.ThreadID
	}
	process, err := controller.Open(daemonprocess.Target{PID: entry.PID, SessionID: sessionID, StateDir: entry.StateDir, StartedAt: entry.StartedAt})
	if errors.Is(err, daemonprocess.ErrExited) {
		refreshAfterForceStop(ctx, cfg)
		return nil
	}
	if err != nil {
		return appwire.Unavailable(fmt.Sprintf("cannot verify daemon for force stop: %v", err))
	}
	defer func() {
		if err := process.Close(); err != nil {
			log.Printf("force stop process handle cleanup: %v", err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := process.Kill(); err != nil && !errors.Is(err, daemonprocess.ErrExited) {
		return appwire.Unavailable(fmt.Sprintf("cannot force stop daemon: %v", err))
	}
	exitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := process.Wait(exitCtx); err != nil {
		return appwire.Unavailable(fmt.Sprintf("daemon exit is not confirmed: %v", err))
	}
	refreshAfterForceStop(ctx, cfg)
	return nil
}

func forceStopEntry(runDir, sessionID string) (rendezvous.Entry, error) {
	entries, err := rendezvous.ListStrict(runDir)
	if err != nil {
		return rendezvous.Entry{}, err
	}
	var match rendezvous.Entry
	found := false
	for _, entry := range entries {
		if entry.SessionID != sessionID && entry.ThreadID != sessionID && entry.WorkspaceRef != "local:"+sessionID {
			continue
		}
		if entry.SourceID != "" && entry.SourceID != "local" {
			return rendezvous.Entry{}, errors.New("daemon claims a foreign session source")
		}
		if found {
			return rendezvous.Entry{}, errors.New("multiple daemons claim this session; cannot choose a force-stop target")
		}
		match, found = entry, true
	}
	if !found {
		return rendezvous.Entry{}, errors.New("no direct daemon ownership claim for this session")
	}
	return match, nil
}

func refreshAfterForceStop(ctx context.Context, cfg hubcore.WebConfig) {
	if cfg.Roster != nil {
		if err := hubRosterRefresh(ctx, cfg.Roster); err != nil {
			log.Printf("daemon stopped; roster refresh remains incomplete: %v", err)
		}
	}
	if cfg.Inputs != nil {
		cfg.Inputs.Bump()
	}
	if cfg.PokeAttention != nil {
		cfg.PokeAttention()
	}
}
