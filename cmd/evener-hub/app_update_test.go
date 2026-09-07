package hub

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/buildinfo"
	"primeradiant.com/evener/internal/selfupdate"
)

func setBuild(t *testing.T, sha, channel string) {
	t.Helper()
	prevSHA, prevChannel := buildinfo.GitSHA, buildinfo.Channel
	buildinfo.GitSHA, buildinfo.Channel = sha, channel
	t.Cleanup(func() { buildinfo.GitSHA, buildinfo.Channel = prevSHA, prevChannel })
	// hubUpdateApply deliberately leaves hubUpdateMu locked after a
	// successful apply; reset it so tests stay independent of run order.
	hubUpdateMu = sync.Mutex{}
}

func stubUpdateCheck(t *testing.T, fn func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error)) *int {
	t.Helper()
	calls := 0
	previous := runHubUpdateCheck
	runHubUpdateCheck = func(ctx context.Context, opts selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		calls++
		return fn(ctx, opts)
	}
	t.Cleanup(func() { runHubUpdateCheck = previous })
	return &calls
}

func stubHubSelfUpgrade(t *testing.T, fn func(context.Context, selfupdate.Options) (selfupdate.Result, error)) *int {
	t.Helper()
	calls := 0
	previous := runHubSelfUpgrade
	runHubSelfUpgrade = func(ctx context.Context, opts selfupdate.Options) (selfupdate.Result, error) {
		calls++
		return fn(ctx, opts)
	}
	t.Cleanup(func() { runHubSelfUpgrade = previous })
	return &calls
}

type restartCall struct {
	binary string
	args   []string
}

func stubScheduleRestart(t *testing.T) *[]restartCall {
	t.Helper()
	var calls []restartCall
	previous := scheduleHubRestart
	scheduleHubRestart = func(binary string, args []string) { calls = append(calls, restartCall{binary, args}) }
	t.Cleanup(func() { scheduleHubRestart = previous })
	return &calls
}

func TestHubUpdateCheckDevBuildIsNotApplicable(t *testing.T) {
	setBuild(t, "", "")
	calls := stubUpdateCheck(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		t.Fatal("dev build must not call Check")
		return selfupdate.CheckResult{}, nil
	})
	got, err := hubUpdateCheck(context.Background(), appwire.UpdateCheckParams{})
	if err != nil {
		t.Fatalf("hubUpdateCheck: %v", err)
	}
	if got.Applicable || got.UpdateAvailable || got.BuildChannel != "dev" || got.CurrentVersion != "dev" {
		t.Fatalf("got %+v", got)
	}
	if *calls != 0 {
		t.Fatalf("Check called %d times", *calls)
	}
}

func TestHubUpdateCheckDefaultsToBuildChannelAndFillsCurrent(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	var gotOpts selfupdate.CheckOptions
	stubUpdateCheck(t, func(_ context.Context, opts selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		gotOpts = opts
		return selfupdate.CheckResult{Channel: opts.Channel, LatestTag: "snapshot", LatestCommit: "be70029abc", UpdateAvailable: true}, nil
	})
	got, err := hubUpdateCheck(context.Background(), appwire.UpdateCheckParams{})
	if err != nil {
		t.Fatalf("hubUpdateCheck: %v", err)
	}
	if gotOpts.Channel != "snapshot" || gotOpts.CurrentSHA != "3b1c5f8" {
		t.Fatalf("opts = %+v", gotOpts)
	}
	want := appwire.UpdateCheckResponse{
		Channel: "snapshot", BuildChannel: "snapshot", CurrentVersion: "3b1c5f8", CurrentCommit: "3b1c5f8",
		LatestTag: "snapshot", LatestCommit: "be70029abc", UpdateAvailable: true, Applicable: true,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestHubUpdateCheckHonoursRequestedChannel(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	var gotChannel string
	stubUpdateCheck(t, func(_ context.Context, opts selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		gotChannel = opts.Channel
		return selfupdate.CheckResult{Channel: opts.Channel, LatestTag: "v0.1.0", LatestCommit: "3b1c5f8ffff"}, nil
	})
	got, err := hubUpdateCheck(context.Background(), appwire.UpdateCheckParams{Channel: "release"})
	if err != nil {
		t.Fatalf("hubUpdateCheck: %v", err)
	}
	if gotChannel != "release" || got.Channel != "release" || got.UpdateAvailable {
		t.Fatalf("channel=%q got=%+v", gotChannel, got)
	}
}

func TestHubUpdateCheckPropagatesError(t *testing.T) {
	setBuild(t, "3b1c5f8", "release")
	stubUpdateCheck(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		return selfupdate.CheckResult{}, errors.New("GET x: 403 Forbidden: API rate limit exceeded")
	})
	_, err := hubUpdateCheck(context.Background(), appwire.UpdateCheckParams{})
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("err = %v", err)
	}
}

func TestHubUpdateApplyDevBuildRefused(t *testing.T) {
	setBuild(t, "", "")
	upgrades := stubHubSelfUpgrade(t, func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		return selfupdate.Result{}, nil
	})
	restarts := stubScheduleRestart(t)
	_, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{})
	if err == nil || !strings.Contains(err.Error(), "dev build") {
		t.Fatalf("err = %v", err)
	}
	if *upgrades != 0 || len(*restarts) != 0 {
		t.Fatalf("upgrades=%d restarts=%d", *upgrades, len(*restarts))
	}
}

func TestHubUpdateApplyInstallsThenSchedulesRestartWithHubArgs(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	previousArgs := hubProcessArgs
	hubProcessArgs = func() []string { return []string{"/old/evener", "hub", "-addr", "0.0.0.0:9180"} }
	t.Cleanup(func() { hubProcessArgs = previousArgs })

	var gotOpts selfupdate.Options
	stubHubSelfUpgrade(t, func(_ context.Context, opts selfupdate.Options) (selfupdate.Result, error) {
		gotOpts = opts
		return selfupdate.Result{
			Release: "snapshot", Channel: "snapshot",
			Installed: []string{"/home/u/.local/share/evener/bin/evener", "/home/u/.local/share/evener/bin/evener-dev"},
		}, nil
	})
	restarts := stubScheduleRestart(t)

	got, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{Channel: "snapshot"})
	if err != nil {
		t.Fatalf("hubUpdateApply: %v", err)
	}
	if gotOpts.Requested != "snapshot" || gotOpts.CurrentChannel != "snapshot" {
		t.Fatalf("opts = %+v", gotOpts)
	}
	if !got.Restarting || got.Release != "snapshot" || len(got.Installed) != 2 {
		t.Fatalf("got %+v", got)
	}
	if len(*restarts) != 1 {
		t.Fatalf("restarts = %v", *restarts)
	}
	call := (*restarts)[0]
	if call.binary != "/home/u/.local/share/evener/bin/evener" {
		t.Fatalf("binary = %q", call.binary)
	}
	if strings.Join(call.args, " ") != "hub -addr 0.0.0.0:9180" {
		t.Fatalf("args = %v", call.args)
	}
}

func TestHubUpdateApplyDefaultChannelIsBuildChannel(t *testing.T) {
	setBuild(t, "3b1c5f8", "release")
	var gotOpts selfupdate.Options
	stubHubSelfUpgrade(t, func(_ context.Context, opts selfupdate.Options) (selfupdate.Result, error) {
		gotOpts = opts
		return selfupdate.Result{Release: "latest", Channel: "release", Installed: []string{"/x/evener", "/x/evener-dev"}}, nil
	})
	stubScheduleRestart(t)
	if _, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{}); err != nil {
		t.Fatalf("hubUpdateApply: %v", err)
	}
	if gotOpts.Requested != "release" {
		t.Fatalf("Requested = %q, want release", gotOpts.Requested)
	}
}

func TestHubUpdateApplyUpgradeFailureDoesNotRestart(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	stubHubSelfUpgrade(t, func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		return selfupdate.Result{}, errors.New("download failed")
	})
	restarts := stubScheduleRestart(t)
	_, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{})
	if err == nil || !strings.Contains(err.Error(), "download failed") {
		t.Fatalf("err = %v", err)
	}
	if len(*restarts) != 0 {
		t.Fatalf("restart scheduled after failed upgrade: %v", *restarts)
	}
}

func TestHubUpdateApplyRejectsResultWithoutInstalledBinary(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	stubHubSelfUpgrade(t, func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		return selfupdate.Result{Release: "snapshot", Channel: "snapshot"}, nil
	})
	restarts := stubScheduleRestart(t)
	_, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{})
	if err == nil {
		t.Fatal("expected error for empty Installed")
	}
	if len(*restarts) != 0 {
		t.Fatalf("restart scheduled: %v", *restarts)
	}
}

func TestHubUpdateApplyPicksEvenerFromInstalled(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	previousArgs := hubProcessArgs
	hubProcessArgs = func() []string { return []string{"/old/evener", "hub"} }
	t.Cleanup(func() { hubProcessArgs = previousArgs })
	stubHubSelfUpgrade(t, func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		return selfupdate.Result{
			Release: "snapshot", Channel: "snapshot",
			// evener-dev listed before evener: picking Installed[0] would be wrong.
			Installed: []string{"/x/evener-dev", "/x/evener"},
		}, nil
	})
	restarts := stubScheduleRestart(t)
	if _, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{}); err != nil {
		t.Fatalf("hubUpdateApply: %v", err)
	}
	if len(*restarts) != 1 || (*restarts)[0].binary != "/x/evener" {
		t.Fatalf("restarts = %v", *restarts)
	}
}

func TestHubUpdateApplyErrorsWhenInstalledHasNoEvener(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	stubHubSelfUpgrade(t, func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		return selfupdate.Result{Release: "snapshot", Channel: "snapshot", Installed: []string{"/x/evener-dev"}}, nil
	})
	restarts := stubScheduleRestart(t)
	_, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{})
	if err == nil || !strings.Contains(err.Error(), "evener") {
		t.Fatalf("err = %v", err)
	}
	if len(*restarts) != 0 {
		t.Fatalf("restart scheduled: %v", *restarts)
	}
}

func TestHubUpdateApplyRejectsUnknownChannel(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	upgrades := stubHubSelfUpgrade(t, func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		t.Fatal("unknown channel must not reach the upgrade seam")
		return selfupdate.Result{}, nil
	})
	restarts := stubScheduleRestart(t)
	_, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{Channel: "v0.0.1"})
	if err == nil || !strings.Contains(err.Error(), `unknown update channel "v0.0.1"`) {
		t.Fatalf("err = %v", err)
	}
	if *upgrades != 0 || len(*restarts) != 0 {
		t.Fatalf("upgrades=%d restarts=%d", *upgrades, len(*restarts))
	}
}

func TestHubUpdateApplyRejectsNightlyChannel(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	upgrades := stubHubSelfUpgrade(t, func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		t.Fatal("unknown channel must not reach the upgrade seam")
		return selfupdate.Result{}, nil
	})
	_, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{Channel: "nightly"})
	if err == nil || !strings.Contains(err.Error(), `unknown update channel "nightly"`) {
		t.Fatalf("err = %v", err)
	}
	if *upgrades != 0 {
		t.Fatalf("upgrades=%d", *upgrades)
	}
}

func TestHubUpdateCheckRejectsUnknownChannel(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	calls := stubUpdateCheck(t, func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error) {
		t.Fatal("unknown channel must not reach the check seam")
		return selfupdate.CheckResult{}, nil
	})
	_, err := hubUpdateCheck(context.Background(), appwire.UpdateCheckParams{Channel: "v0.0.1"})
	if err == nil || !strings.Contains(err.Error(), `unknown update channel "v0.0.1"`) {
		t.Fatalf("err = %v", err)
	}
	if *calls != 0 {
		t.Fatalf("Check called %d times", *calls)
	}
}

func TestHubUpdateApplySerializesConcurrentCalls(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	stubHubSelfUpgrade(t, func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		entered <- struct{}{}
		<-block
		return selfupdate.Result{}, errors.New("boom")
	})
	stubScheduleRestart(t)

	done := make(chan error, 1)
	go func() {
		_, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{})
		done <- err
	}()
	<-entered

	_, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{})
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second apply err = %v", err)
	}

	close(block)
	if firstErr := <-done; firstErr == nil || !strings.Contains(firstErr.Error(), "boom") {
		t.Fatalf("first apply err = %v", firstErr)
	}

	// After a failed upgrade the lock must be released so a later apply proceeds.
	stubHubSelfUpgrade(t, func(context.Context, selfupdate.Options) (selfupdate.Result, error) {
		return selfupdate.Result{Release: "snapshot", Channel: "snapshot", Installed: []string{"/x/evener"}}, nil
	})
	if _, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{}); err != nil {
		t.Fatalf("apply after failure: %v", err)
	}
}
