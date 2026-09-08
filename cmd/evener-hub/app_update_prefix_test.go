package hub

import (
	"context"
	"testing"

	"primeradiant.com/evener/appwire"
	"primeradiant.com/evener/internal/selfupdate"
)

// TestHubUpdateApplyUpgradesTheRunningPrefix proves a hub executing from a
// system prefix upgrades that prefix: the apply path derives Prefix,
// BinDir, and ShareBinDir from the running binary instead of letting
// Upgrade fall back to ~/.local. Fails today because hubUpdateApply passes
// no install dirs at all.
func TestHubUpdateApplyUpgradesTheRunningPrefix(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	previousExe := hubExecutable
	hubExecutable = func() (string, error) { return "/usr/local/share/evener/bin/evener", nil }
	t.Cleanup(func() { hubExecutable = previousExe })

	var gotOpts selfupdate.Options
	stubHubSelfUpgrade(t, func(_ context.Context, opts selfupdate.Options) (selfupdate.Result, error) {
		gotOpts = opts
		return selfupdate.Result{
			Release: "snapshot", Channel: "snapshot",
			Installed: []string{"/usr/local/share/evener/bin/evener", "/usr/local/share/evener/bin/evener-dev"},
		}, nil
	})
	stubScheduleRestart(t)

	if _, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{Channel: "snapshot"}); err != nil {
		t.Fatalf("hubUpdateApply: %v", err)
	}
	if gotOpts.Prefix != "/usr/local" {
		t.Fatalf("Prefix = %q, want %q", gotOpts.Prefix, "/usr/local")
	}
	if gotOpts.BinDir != "/usr/local/bin" {
		t.Fatalf("BinDir = %q, want %q", gotOpts.BinDir, "/usr/local/bin")
	}
	if gotOpts.ShareBinDir != "/usr/local/share/evener/bin" {
		t.Fatalf("ShareBinDir = %q, want %q", gotOpts.ShareBinDir, "/usr/local/share/evener/bin")
	}
}

// TestHubUpdateApplyDefaultsInstallDirsForUnknownLayouts proves a hub
// running from a worktree build (no install layout) still upgrades with
// Upgrade's own defaults: empty install dirs, not garbage.
func TestHubUpdateApplyDefaultsInstallDirsForUnknownLayouts(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	previousExe := hubExecutable
	hubExecutable = func() (string, error) { return "/tmp/worktree-evener-build/evener-hub", nil }
	t.Cleanup(func() { hubExecutable = previousExe })

	var gotOpts selfupdate.Options
	stubHubSelfUpgrade(t, func(_ context.Context, opts selfupdate.Options) (selfupdate.Result, error) {
		gotOpts = opts
		return selfupdate.Result{
			Release: "snapshot", Channel: "snapshot",
			Installed: []string{"/home/u/.local/share/evener/bin/evener"},
		}, nil
	})
	stubScheduleRestart(t)

	if _, err := hubUpdateApply(context.Background(), appwire.UpdateApplyParams{Channel: "snapshot"}); err != nil {
		t.Fatalf("hubUpdateApply: %v", err)
	}
	if gotOpts.Prefix != "" || gotOpts.BinDir != "" || gotOpts.ShareBinDir != "" {
		t.Fatalf("opts = %+v, want empty install dirs for an unknown layout", gotOpts)
	}
}

// TestHubUpgradeUpgradesTheRunningPrefix proves evener/upgrade (the TUI
// path) gets the same treatment: without it, fixing only apply would leave
// the older RPC installing into the wrong prefix.
func TestHubUpgradeUpgradesTheRunningPrefix(t *testing.T) {
	setBuild(t, "3b1c5f8", "snapshot")
	previousExe := hubExecutable
	hubExecutable = func() (string, error) { return "/usr/local/share/evener/bin/evener", nil }
	t.Cleanup(func() { hubExecutable = previousExe })

	var gotOpts selfupdate.Options
	stubHubSelfUpgrade(t, func(_ context.Context, opts selfupdate.Options) (selfupdate.Result, error) {
		gotOpts = opts
		return selfupdate.Result{Release: "snapshot", Channel: "snapshot", Installed: []string{"/x/evener"}}, nil
	})

	if _, err := hubUpgrade(context.Background(), appwire.UpgradeParams{}); err != nil {
		t.Fatalf("hubUpgrade: %v", err)
	}
	if gotOpts.Prefix != "/usr/local" || gotOpts.BinDir != "/usr/local/bin" || gotOpts.ShareBinDir != "/usr/local/share/evener/bin" {
		t.Fatalf("opts = %+v, want the running /usr/local layout", gotOpts)
	}
}
