package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/buildinfo"
	"primeradiant.com/evener/envvars"
	"primeradiant.com/evener/internal/selfupdate"
)

// Test seams: the GitHub check, the exec-in-place restart, and the restart
// goroutine's log destination.
var (
	runHubUpdateCheck            = selfupdate.Check
	scheduleHubRestart           = scheduleHubRestartAfterResponse
	execHubBinary                = selfupdate.Restart
	hubUpdateStderr    io.Writer = os.Stderr
)

// hubRestartDelay gives the appwire response time to reach the browser
// before the process image is replaced; the frontend needs the success
// result to start its health poll. A var, not a const, so tests can set it
// to 0.
var hubRestartDelay = 500 * time.Millisecond

// hubUpdateMu serializes evener/update/apply: copyExecutable in
// internal/selfupdate writes a fixed dst+".tmp" path, so a second apply
// racing the first would corrupt the binary a restart is about to exec.
// It is deliberately left locked after a successful upgrade schedules a
// restart -- the process is about to be replaced, so a second apply
// afterward must also be refused, not merely serialized.
var hubUpdateMu sync.Mutex

// tryLockHubUpdate acquires hubUpdateMu for the two RPCs that can install a
// new hub binary (evener/update/apply and evener/upgrade), returning the
// shared "already in progress" error when the other one already holds it.
func tryLockHubUpdate() error {
	if !hubUpdateMu.TryLock() {
		return errors.New("a hub update is already in progress")
	}
	return nil
}

// isDevBuild reports whether this hub was built without a release channel
// (a worktree build). Such a hub is never self-updated: replacing it with a
// release binary would silently discard whatever the developer is running.
func isDevBuild() bool { return buildinfo.BuildChannel() == "dev" }

// validateUpdateChannel resolves requested (defaulting to the build's own
// upgrade channel) and rejects anything but "release" or "snapshot" --
// unlike selfupdate.Upgrade's ResolveTarget, which also accepts "current",
// "latest", and arbitrary "v*" tags. Applying an arbitrary release tag would
// let evener/update/apply install and exec whatever the caller names.
func validateUpdateChannel(requested string) (string, error) {
	ch := envvars.FirstNonEmpty(requested, buildinfo.UpgradeChannel())
	switch ch {
	case "release", "snapshot":
		return ch, nil
	default:
		return "", fmt.Errorf("unknown update channel %q", ch)
	}
}

// hubUpdateCheck answers evener/update/check: compare the running build to
// the channel's current commit. Dev builds answer locally.
func hubUpdateCheck(ctx context.Context, params appwire.UpdateCheckParams) (appwire.UpdateCheckResponse, error) {
	channel, err := validateUpdateChannel(params.Channel)
	if err != nil {
		return appwire.UpdateCheckResponse{}, err
	}
	resp := appwire.UpdateCheckResponse{
		Channel:        channel,
		BuildChannel:   buildinfo.BuildChannel(),
		CurrentVersion: buildinfo.Version(),
		CurrentCommit:  buildinfo.GitSHA,
	}
	if isDevBuild() {
		return resp, nil
	}
	result, err := runHubUpdateCheck(ctx, selfupdate.CheckOptions{
		Channel:    resp.Channel,
		CurrentSHA: buildinfo.GitSHA,
	})
	if err != nil {
		return appwire.UpdateCheckResponse{}, err
	}
	resp.LatestTag = result.LatestTag
	resp.LatestCommit = result.LatestCommit
	resp.UpdateAvailable = result.UpdateAvailable
	resp.Applicable = true
	return resp, nil
}

// hubUpdateApply answers evener/update/apply: install the channel's build,
// then exec it in place once this response has gone out.
func hubUpdateApply(ctx context.Context, params appwire.UpdateApplyParams) (appwire.UpdateApplyResponse, error) {
	if isDevBuild() {
		return appwire.UpdateApplyResponse{}, errors.New("this hub is a dev build; rebuild with make build-hub instead of self-updating")
	}
	channel, err := validateUpdateChannel(params.Channel)
	if err != nil {
		return appwire.UpdateApplyResponse{}, err
	}
	if err := tryLockHubUpdate(); err != nil {
		return appwire.UpdateApplyResponse{}, err
	}
	unlockOnReturn := true
	defer func() {
		if unlockOnReturn {
			hubUpdateMu.Unlock()
		}
	}()

	result, err := runHubSelfUpgrade(ctx, selfupdate.Options{
		Requested:      channel,
		CurrentChannel: buildinfo.UpgradeChannel(),
	})
	if err != nil {
		return appwire.UpdateApplyResponse{}, err
	}
	binary, err := evenerBinaryFrom(channel, result.Installed)
	if err != nil {
		return appwire.UpdateApplyResponse{}, err
	}
	// The process is about to be replaced: leave hubUpdateMu held so a
	// second apply after this one is also refused.
	unlockOnReturn = false
	scheduleHubRestart(binary, hubProcessArgs()[1:])
	return appwire.UpdateApplyResponse{
		Release:    result.Release,
		Channel:    result.Channel,
		Installed:  result.Installed,
		Restarting: true,
	}, nil
}

// evenerBinaryFrom picks the installed "evener" binary out of an upgrade
// result. installExtractedBinaries also installs "evener-dev" alongside it,
// so the entry to exec into can't be assumed to be Installed[0].
func evenerBinaryFrom(channel string, installed []string) (string, error) {
	for _, path := range installed {
		if filepath.Base(path) == "evener" {
			return path, nil
		}
	}
	return "", fmt.Errorf("upgrade to %s installed no evener binary", channel)
}

// scheduleHubRestartAfterResponse execs binary with the hub's own arguments
// after hubRestartDelay. hubUpdateMu is held across the exec attempt (see
// its doc comment); on failure the old hub keeps running, so the lock is
// released here too, or every later apply/upgrade would be refused forever.
func scheduleHubRestartAfterResponse(binary string, args []string) {
	go func() {
		time.Sleep(hubRestartDelay)
		_, _ = fmt.Fprintf(hubUpdateStderr, "[hub] self-update: restarting as %s\n", binary)
		if err := execHubBinary(binary, args); err != nil {
			hubUpdateMu.Unlock()
			_, _ = fmt.Fprintf(hubUpdateStderr, "[hub] self-update: restart failed, still running the previous binary: %v\n", err)
		}
	}()
}
