//go:build unix

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"primeradiant.com/evener/appwire"
)

// TestUpdateApplyEndToEnd installs the current snapshot into a temp prefix,
// runs that hub, applies an update through the RPC, and proves the process
// replaced itself in place: same PID, /api/health back. Opt-in only: it
// downloads from GitHub (AGENTS.md forbids network in default tests). The
// snapshot installed at test start IS the latest snapshot, so the exec
// replaces the process with the same build -- the version reported before
// and after may be identical. What this proves is that the exec happened
// and the process survived it under the same PID, not that the version
// changed.
//
//	EVENER_UPDATE_E2E=1 go test ./cmd/evener-hub/ -run TestUpdateApplyEndToEnd -v
func TestUpdateApplyEndToEnd(t *testing.T) {
	if os.Getenv("EVENER_UPDATE_E2E") == "" {
		t.Skip("set EVENER_UPDATE_E2E=1 to run (downloads from GitHub)")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}

	prefix := t.TempDir()
	install := exec.Command("sh", filepath.Join(repoRoot, "install.sh"))
	install.Env = append(os.Environ(), "PREFIX="+prefix, "EVENER_INSTALL_VERSION=snapshot")
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	evenerBin := filepath.Join(prefix, "bin", "evener")

	// No model call is made, so the fake provider only needs to exist in
	// config -- base_url is never dialed.
	stack := startHubStackOnProviderWithEvener(t, `
default = "fake"

[providers.fake]
base     = "openai-compatible"
base_url = "http://127.0.0.1:1"
api_key  = "fakellm-not-a-secret"
`, "fake/does-not-matter", evenerBin)

	before := healthVersion(t, stack.addr)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := stack.dialRPC(ctx, t)

	resp, err := client.UpdateApply(ctx, appwire.UpdateApplyParams{Channel: "snapshot"})
	if err != nil {
		t.Fatalf("UpdateApply: %v", err)
	}
	if !resp.Restarting {
		t.Fatalf("resp = %+v", resp)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		r, err := http.Get("http://" + stack.addr + "/api/health")
		if err != nil {
			continue
		}
		var body struct {
			Version string `json:"version"`
		}
		decodeErr := json.NewDecoder(r.Body).Decode(&body)
		_ = r.Body.Close()
		if decodeErr != nil || body.Version == "" {
			continue
		}
		if !processAlive(stack.pid) {
			t.Fatalf("hub pid %d died", stack.pid)
		}
		t.Logf("before=%s after=%s pid=%d", before, body.Version, stack.pid)
		return
	}
	t.Fatal("hub did not come back within 30s")
}

func healthVersion(t *testing.T, addr string) string {
	t.Helper()
	r, err := http.Get("http://" + addr + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	defer r.Body.Close()
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode /api/health: %v", err)
	}
	return body.Version
}

// processAlive reports whether pid still refers to a live process, using the
// null-signal probe: FindProcess always succeeds on unix, so Signal(0) is
// what actually checks.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
