package hub

import (
	"context"
	"errors"
	"strings"
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
