package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/cmd/evener-hub/internal/appsource"
	"primeradiant.com/evener/cmd/evener-hub/internal/daemonprocess"
	"primeradiant.com/evener/cmd/evener-hub/internal/hubcore"
	"primeradiant.com/evener/rendezvous"
)

// forceStopThread bypasses daemon RPC only after verifying the local process.
// The session remains reserved until the exact process has exited.
func forceStopThread(ctx context.Context, cfg hubcore.WebConfig, params appwire.ThreadForceStopParams, sources *appsource.Registry) (stopErr error) {
	ref, err := appwire.ParseRef(params.Ref)
	if err != nil || ref.SourceID != "local" {
		return appwire.InvalidParams("force stop requires a local session ref")
	}
	if cfg.RunDir == "" || cfg.ResumeLocks == nil {
		return appwire.Unavailable("local session ownership is not configured")
	}
	entry, err := forceStopEntry(cfg.RunDir, ref.ThreadID, cfg.DaemonProcesses, nil)
	if err != nil {
		return appwire.Unavailable(err.Error())
	}
	// Verify the process before interrupting RPCs. Kill verifies this retained
	// process handle again after ownership and deletion reservations are held.
	controller := cfg.DaemonProcesses
	if controller == nil {
		controller = daemonprocess.NewController()
	}
	sessionID := entry.SessionID
	if sessionID == "" {
		sessionID = entry.ThreadID
	}
	process, err := controller.Open(daemonprocess.Target{PID: entry.PID, SessionID: sessionID, StateDir: entry.StateDir, StartedAt: entry.StartedAt})
	exited := errors.Is(err, daemonprocess.ErrExited)
	if err != nil && !exited {
		return appwire.Unavailable(fmt.Sprintf("cannot verify daemon for force stop: %v", err))
	}
	defer func() {
		if process == nil {
			return
		}
		if err := process.Close(); err != nil {
			log.Printf("force stop process handle cleanup: %v", err)
		}
	}()
	aliases := forceStopAliases(entry)
	finishRecovery := cfg.ResumeLocks.BeginForceStop(aliases)
	defer func() { finishRecovery(stopErr == nil) }()
	if sources != nil {
		if source, ok := sources.Source("local"); ok {
			if local, ok := source.(*appsource.LocalDaemonSource); ok {
				release := local.BeginRecovery(entry)
				defer release()
			}
		}
	}
	// Clear gives one daemon stable and current session aliases. Lock both so
	// resume or deletion through either alias cannot race exit confirmation.
	for _, id := range aliases {
		cfg.ResumeLocks.For(id).Lock()
	}
	defer func() {
		for _, alias := range slices.Backward(aliases) {
			cfg.ResumeLocks.For(alias).Unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := forceStopOwnershipUnchanged(cfg.RunDir, ref.ThreadID, entry, cfg.DaemonProcesses); err != nil {
		return appwire.Unavailable(err.Error())
	}
	if err := deletionFenceError(cfg, params.Ref, ref.ThreadID, ""); err != nil {
		return err
	}
	if exited {
		refreshAfterForceStop(ctx, cfg)
		return nil
	}
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

// forceStopOwnershipUnchanged revalidates discovery after acquiring every
// alias lock, before signaling the verified process.
func forceStopOwnershipUnchanged(runDir, sessionID string, previous rendezvous.Entry, controller daemonprocess.Controller) error {
	current, err := forceStopEntry(runDir, sessionID, controller, &previous)
	if err != nil {
		return err
	}
	if current != previous {
		return errors.New("daemon ownership changed; refresh the session before force stopping")
	}
	return nil
}

func forceStopEntry(runDir, sessionID string, controller daemonprocess.Controller, previous *rendezvous.Entry) (rendezvous.Entry, error) {
	entries, err := rendezvous.ListStrict(runDir)
	if err != nil {
		return rendezvous.Entry{}, err
	}
	// Retained crash markers are evidence, not competing live ownership. Only
	// inspect overlapping claims, and retain every unresolved process identity.
	var targetAliases []string
	for _, entry := range entries {
		if slices.Contains(forceStopAliases(entry), sessionID) {
			targetAliases = append(targetAliases, forceStopAliases(entry)...)
		}
	}
	overlaps := func(entry rendezvous.Entry) bool {
		for _, alias := range forceStopAliases(entry) {
			if slices.Contains(targetAliases, alias) {
				return true
			}
		}
		return false
	}
	claims := 0
	for _, entry := range entries {
		if overlaps(entry) {
			claims++
		}
	}
	if claims > 1 {
		if controller == nil {
			controller = daemonprocess.NewController()
		}
		var exitedMatch rendezvous.Entry
		exitedDirect := false
		entries = slices.DeleteFunc(entries, func(entry rendezvous.Entry) bool {
			if !overlaps(entry) {
				return false
			}
			id := entry.SessionID
			if id == "" {
				id = entry.ThreadID
			}
			process, err := controller.Open(daemonprocess.Target{PID: entry.PID, SessionID: id, StateDir: entry.StateDir, StartedAt: entry.StartedAt})
			if err == nil {
				_ = process.Close()
			}
			exited := errors.Is(err, daemonprocess.ErrExited)
			if exited && slices.Contains(forceStopAliases(entry), sessionID) && (!exitedDirect || (previous != nil && entry == *previous)) {
				exitedMatch, exitedDirect = entry, true
			}
			return exited
		})
		// Keep one verified-exited direct claim only when no unresolved or live
		// overlap remains. Prefer the reserved identity during revalidation so an
		// exit racing recovery remains an idempotent success.
		if exitedDirect && !slices.ContainsFunc(entries, overlaps) {
			entries = append(entries, exitedMatch)
		}
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
	aliases := forceStopAliases(match)
	claims = 0
	for _, entry := range entries {
		for _, alias := range forceStopAliases(entry) {
			if slices.Contains(aliases, alias) {
				claims++
				break
			}
		}
	}
	if claims != 1 {
		return rendezvous.Entry{}, errors.New("multiple daemons claim this session; cannot choose a force-stop target")
	}
	return match, nil
}

func forceStopAliases(entry rendezvous.Entry) []string {
	aliases := []string{entry.SessionID, entry.ThreadID}
	if workspace, err := appwire.ParseRef(entry.WorkspaceRef); err == nil && workspace.SourceID == "local" {
		aliases = append(aliases, workspace.ThreadID)
	}
	slices.Sort(aliases)
	return slices.DeleteFunc(slices.Compact(aliases), func(id string) bool { return id == "" })
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
