package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/buildinfo"
	"primeradiant.com/evener/envvars"
	"primeradiant.com/evener/internal/selfupdate"
)

// Test seams: the GitHub check and the exec-in-place restart.
var (
	runHubUpdateCheck  = selfupdate.Check
	scheduleHubRestart = scheduleHubRestartAfterResponse
)

// hubRestartDelay gives the appwire response time to reach the browser
// before the process image is replaced; the frontend needs the success
// result to start its health poll.
const hubRestartDelay = 500 * time.Millisecond

// isDevBuild reports whether this hub was built without a release channel
// (a worktree build). Such a hub is never self-updated: replacing it with a
// release binary would silently discard whatever the developer is running.
func isDevBuild() bool { return buildinfo.BuildChannel() == "dev" }

func updateChannel(requested string) string {
	return envvars.FirstNonEmpty(requested, buildinfo.UpgradeChannel())
}

// hubUpdateCheck answers evener/update/check: compare the running build to
// the channel's current commit. Dev builds answer locally.
func hubUpdateCheck(ctx context.Context, params appwire.UpdateCheckParams) (appwire.UpdateCheckResponse, error) {
	resp := appwire.UpdateCheckResponse{
		Channel:        updateChannel(params.Channel),
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
	channel := updateChannel(params.Channel)
	result, err := runHubSelfUpgrade(ctx, selfupdate.Options{
		Requested:      channel,
		CurrentChannel: buildinfo.UpgradeChannel(),
	})
	if err != nil {
		return appwire.UpdateApplyResponse{}, err
	}
	if len(result.Installed) == 0 {
		return appwire.UpdateApplyResponse{}, fmt.Errorf("upgrade to %s installed no binaries", channel)
	}
	scheduleHubRestart(result.Installed[0], hubProcessArgs()[1:])
	return appwire.UpdateApplyResponse{
		Release:    result.Release,
		Channel:    result.Channel,
		Installed:  result.Installed,
		Restarting: true,
	}, nil
}

// scheduleHubRestartAfterResponse execs binary with the hub's own arguments
// after hubRestartDelay. On exec failure the old hub keeps running and the
// failure is logged; there is nothing else to roll back.
func scheduleHubRestartAfterResponse(binary string, args []string) {
	go func() {
		time.Sleep(hubRestartDelay)
		_, _ = fmt.Fprintf(os.Stderr, "[hub] self-update: restarting as %s\n", binary)
		if err := selfupdate.Restart(binary, args); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[hub] self-update: restart failed, still running the previous binary: %v\n", err)
		}
	}()
}
