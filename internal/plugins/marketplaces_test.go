package plugins

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeMarketplaceRepo builds a git repo containing a .claude-plugin/marketplace.json
// naming one plugin, and returns its path.
func makeMarketplaceRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mkt-"+name)
	mj := `{"name":"` + name + `","owner":{"name":"o"},"plugins":[` +
		`{"name":"widget","description":"a widget","source":"./plugins/widget"}]}`
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"), []byte(mj), 0o644); err != nil {
		t.Fatal(err)
	}
	makeGitRepo(t, dir, "README.md", "mkt") // also commits marketplace.json via `git add .`
	return dir
}

func TestAddListRemoveMarketplace(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	src := makeMarketplaceRepo(t, "acme")
	m := NewManager(t.TempDir())

	ref, err := m.AddMarketplace(context.Background(), "", Source{Kind: SourceURL, URL: src})
	if err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if ref.InstallLocation == "" {
		t.Fatal("empty InstallLocation")
	}

	list, err := m.ListMarketplaces()
	if err != nil {
		t.Fatalf("ListMarketplaces: %v", err)
	}
	if _, ok := list["acme"]; !ok {
		t.Fatalf("marketplace 'acme' not listed: %v", list)
	}

	if err := m.RemoveMarketplace(context.Background(), "acme"); err != nil {
		t.Fatalf("RemoveMarketplace: %v", err)
	}
	list, _ = m.ListMarketplaces()
	if _, ok := list["acme"]; ok {
		t.Fatal("marketplace still present after remove")
	}
	if _, err := os.Stat(m.marketplaceDir("acme")); !os.IsNotExist(err) {
		t.Fatal("clone dir not deleted after remove")
	}
}

func TestAddMarketplace_GitSubdirBrowse(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	repo := filepath.Join(t.TempDir(), "monorepo")
	if err := os.MkdirAll(filepath.Join(repo, "mkt", ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "mkt", ".claude-plugin", "marketplace.json"),
		[]byte(`{"name":"acme","owner":{"name":"o"},"plugins":[{"name":"widget","source":"./plugins/widget"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	makeGitRepo(t, repo, "README.md", "root") // commits everything incl. mkt/

	m := NewManager(t.TempDir())
	if _, err := m.AddMarketplace(context.Background(), "", Source{Kind: SourceGitSubdir, URL: repo, Path: "mkt"}); err != nil {
		t.Fatalf("AddMarketplace git-subdir: %v", err)
	}
	cat, err := m.Browse(context.Background(), "acme")
	if err != nil {
		t.Fatalf("Browse git-subdir marketplace: %v", err)
	}
	if cat.Name != "acme" || len(cat.Plugins) != 1 {
		t.Fatalf("catalog = %+v", cat)
	}
}

func TestAddMarketplace_RejectsTraversalName(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	repo := filepath.Join(t.TempDir(), "evilmkt")
	if err := os.MkdirAll(filepath.Join(repo, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// marketplace.json whose name escapes the store
	if err := os.WriteFile(filepath.Join(repo, ".claude-plugin", "marketplace.json"),
		[]byte(`{"name":"../../evil","owner":{"name":"o"},"plugins":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	makeGitRepo(t, repo, "README.md", "x")

	m := NewManager(t.TempDir())
	if _, err := m.AddMarketplace(context.Background(), "", Source{Kind: SourceURL, URL: repo}); err == nil {
		t.Fatal("AddMarketplace accepted a traversing marketplace name")
	}
}

func TestRemoveMarketplace_DirectorySourceKeepsContents(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	dir := makeMarketplaceRepo(t, "local")
	m := NewManager(t.TempDir())
	if _, err := m.AddMarketplace(context.Background(), "", Source{Kind: SourceDirectory, Path: dir}); err != nil {
		t.Fatalf("AddMarketplace directory: %v", err)
	}
	if err := m.RemoveMarketplace(context.Background(), "local"); err != nil {
		t.Fatalf("RemoveMarketplace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "marketplace.json")); err != nil {
		t.Fatalf("directory source contents deleted on remove: %v", err)
	}
}

func TestRefreshMarketplace_ClonesUnfetchedSeed(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	mktRepo := makeMarketplaceRepo(t, "acme")
	m := NewManager(t.TempDir())
	if err := m.saveMarketplaces(Marketplaces{"acme": {Source: Source{Kind: SourceURL, URL: mktRepo}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.RefreshMarketplace(context.Background(), "acme"); err != nil {
		t.Fatalf("refresh unfetched seed: %v", err)
	}
	mk, _ := m.ListMarketplaces()
	if mk["acme"].InstallLocation == "" {
		t.Fatal("refresh did not clone the unfetched seed")
	}
}

// TestRefreshMarketplace_RecoversWedgedCloneByStagedReclone pins the
// self-heal path: when git pull fails in the persistent clone (here wedged
// exactly the way a SIGKILLed git wedges it — a stale .git/index.lock),
// refresh falls back to a staged reclone and recovers instead of failing
// forever until the marketplace is removed and re-added.
func TestRefreshMarketplace_RecoversWedgedCloneByStagedReclone(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	src := makeMarketplaceRepo(t, "acme")
	m := NewManager(t.TempDir())
	ref, err := m.AddMarketplace(context.Background(), "", Source{Kind: SourceURL, URL: src})
	if err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	// Upstream gains a commit, so a refresh must move the index (and would
	// visibly pull new content).
	advanceGitRepo(t, src, "README.md", "new upstream content")
	// Wedge the clone: a stale lock makes every pull fail.
	if err := os.WriteFile(filepath.Join(ref.InstallLocation, ".git", "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.RefreshMarketplace(context.Background(), "acme"); err != nil {
		t.Fatalf("refresh did not self-heal a wedged clone: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(ref.InstallLocation, "README.md")); err != nil || string(b) != "new upstream content" {
		t.Fatalf("recovered clone lacks fresh upstream content: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(ref.InstallLocation, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Fatalf("stale index.lock survived the reclone: %v", err)
	}
	for _, leftover := range []string{".staging", ".old"} {
		if _, err := os.Stat(m.marketplaceDir(leftover)); !os.IsNotExist(err) {
			t.Fatalf("reclone left %s behind: %v", leftover, err)
		}
	}
}

// TestRefreshMarketplace_FailedRecloneLeavesCloneUntouched pins the safety
// constraint of the staged reclone: if the fresh clone cannot be downloaded,
// the existing (possibly wedged) clone is left exactly as it was — never
// wiped — and the error reports both failures loudly.
func TestRefreshMarketplace_FailedRecloneLeavesCloneUntouched(t *testing.T) {
	m := NewManager(t.TempDir())
	installLoc := m.marketplaceDir("acme")
	if err := os.MkdirAll(installLoc, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(installLoc, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("still here"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := time.Unix(1000, 0).UTC()
	if err := m.saveMarketplaces(Marketplaces{"acme": {
		Source:          Source{Kind: SourceURL, URL: "https://example.invalid/repo.git"},
		InstallLocation: installLoc,
		LastUpdated:     before,
	}}); err != nil {
		t.Fatal(err)
	}
	origPull, origClone := marketplaceGitPull, marketplaceGitClone
	t.Cleanup(func() { marketplaceGitPull, marketplaceGitClone = origPull, origClone })
	marketplaceGitPull = func(context.Context, string) error { return errors.New("pull wedged") }
	marketplaceGitClone = func(context.Context, string, string, string, string) error { return errors.New("network down") }

	err := m.RefreshMarketplace(context.Background(), "acme")
	if err == nil {
		t.Fatal("refresh succeeded; want loud error when pull and reclone both fail")
	}
	if !strings.Contains(err.Error(), "pull wedged") || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("err = %v, want both the pull and reclone failures reported", err)
	}
	b, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(b) != "still here" {
		t.Fatalf("old clone touched by failed reclone: %q, %v", b, readErr)
	}
	if _, err := os.Stat(m.marketplaceDir(".staging")); !os.IsNotExist(err) {
		t.Fatalf("failed reclone left .staging behind: %v", err)
	}
	mk, _ := m.ListMarketplaces()
	if !mk["acme"].LastUpdated.Equal(before) {
		t.Fatalf("LastUpdated advanced on failed refresh: %v", mk["acme"].LastUpdated)
	}
}

// makeMarketplaceRepoWithPlugin builds a git repo whose marketplace.json is
// named name and lists one installable plugin at ./plugins/<plugin>.
func makeMarketplaceRepoWithPlugin(t *testing.T, name, plugin string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mkt-"+name+"-"+plugin)
	mj := `{"name":"` + name + `","owner":{"name":"o"},"plugins":[{"name":"` + plugin + `","source":"./plugins/` + plugin + `"}]}`
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"), []byte(mj), 0o644); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, filepath.Join(dir, "plugins", plugin), plugin, nil)
	makeGitRepo(t, dir, "README.md", "mkt")
	return dir
}

// makeDirectoryMarketplace builds a directory-source marketplace (no git)
// named name with one installable plugin.
func makeDirectoryMarketplace(t *testing.T, name, plugin string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dir-"+name)
	mj := `{"name":"` + name + `","owner":{"name":"o"},"plugins":[{"name":"` + plugin + `","source":"./plugins/` + plugin + `"}]}`
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "marketplace.json"), []byte(mj), 0o644); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, filepath.Join(dir, "plugins", plugin), plugin, nil)
	return dir
}

func TestEditMarketplace_RenameMovesCloneCacheAndRegistry(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	mktRepo, name := makeInstallableMarketplace(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceURL, URL: mktRepo}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if _, err := m.Install(ctx, "widget", name); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// LastUpdated is a fetch-freshness stamp and a rename fetches nothing; the
	// fixed clock makes any bump unmistakable.
	before, err := m.ListMarketplaces()
	if err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return time.Date(2031, 4, 1, 0, 0, 0, 0, time.UTC) }

	ref, err := m.EditMarketplace(ctx, name, "acme2", nil)
	if err != nil {
		t.Fatalf("EditMarketplace: %v", err)
	}
	if !ref.LastUpdated.Equal(before[name].LastUpdated) {
		t.Fatalf("a pure rename advanced LastUpdated: %v → %v", before[name].LastUpdated, ref.LastUpdated)
	}
	if ref.InstallLocation != m.marketplaceDir("acme2") {
		t.Fatalf("InstallLocation = %q, want %q", ref.InstallLocation, m.marketplaceDir("acme2"))
	}
	if _, err := os.Stat(m.marketplaceDir("acme2")); err != nil {
		t.Fatalf("renamed clone missing: %v", err)
	}
	if _, err := os.Stat(m.marketplaceDir(name)); !os.IsNotExist(err) {
		t.Fatal("old clone directory survived the rename")
	}
	list, err := m.ListMarketplaces()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := list["acme2"]; !ok {
		t.Fatalf("acme2 not registered: %v", list)
	}
	if _, ok := list[name]; ok {
		t.Fatal("the old name is still registered")
	}
	reg, err := m.loadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	entries, ok := reg.Plugins[registryKey("widget", "acme2")]
	if !ok || len(entries) != 1 {
		t.Fatalf("registry not re-keyed: %v", reg.Plugins)
	}
	if _, still := reg.Plugins[registryKey("widget", name)]; still {
		t.Fatal("the old registry key survived")
	}
	wantPrefix := filepath.Join(m.cacheDir(), "acme2") + string(filepath.Separator)
	if !strings.HasPrefix(entries[0].InstallPath, wantPrefix) {
		t.Fatalf("InstallPath = %q, want it under %q", entries[0].InstallPath, wantPrefix)
	}
	if _, err := os.Stat(entries[0].InstallPath); err != nil {
		t.Fatalf("the re-keyed install path does not exist: %v", err)
	}
	items, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Plugin != "widget" || items[0].Marketplace != "acme2" {
		t.Fatalf("List = %+v", items)
	}
	cat, err := m.Browse(ctx, "acme2")
	if err != nil || len(cat.Plugins) != 1 {
		t.Fatalf("Browse acme2 = %+v, %v", cat, err)
	}
}

func TestEditMarketplace_DirectorySourceRenameKeepsThePath(t *testing.T) {
	dir := makeDirectoryMarketplace(t, "acme", "widget")
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceDirectory, Path: dir}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if _, err := m.Install(ctx, "widget", "acme"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	ref, err := m.EditMarketplace(ctx, "acme", "beta", nil)
	if err != nil {
		t.Fatalf("EditMarketplace: %v", err)
	}
	if ref.InstallLocation != dir {
		t.Fatalf("a directory source is referenced in place; InstallLocation = %q", ref.InstallLocation)
	}
	reg, _ := m.loadRegistry()
	entries, ok := reg.Plugins[registryKey("widget", "beta")]
	if !ok || len(entries) != 1 {
		t.Fatalf("registry not re-keyed: %v", reg.Plugins)
	}
	if _, err := os.Stat(entries[0].InstallPath); err != nil {
		t.Fatalf("install path after rename: %v", err)
	}
}

func TestEditMarketplace_ResourceSwapsTheClone(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	repoA := makeMarketplaceRepoWithPlugin(t, "acme", "widget")
	repoB := makeMarketplaceRepoWithPlugin(t, "acme", "gadget")
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceURL, URL: repoA}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	stamp := time.Date(2031, 4, 1, 0, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return stamp }
	ref, err := m.EditMarketplace(ctx, "acme", "", &Source{Kind: SourceURL, URL: repoB})
	if err != nil {
		t.Fatalf("EditMarketplace: %v", err)
	}
	if ref.Source.URL != repoB {
		t.Fatalf("Source = %+v, want repoB", ref.Source)
	}
	if !ref.LastUpdated.Equal(stamp) {
		t.Fatalf("a re-source did not advance LastUpdated: %v, want %v", ref.LastUpdated, stamp)
	}
	cat, err := m.Browse(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Plugins) != 1 || cat.Plugins[0].Name != "gadget" {
		t.Fatalf("catalog after re-source = %+v, want gadget", cat.Plugins)
	}
	if _, err := os.Stat(m.marketplaceDir(".staging")); !os.IsNotExist(err) {
		t.Fatal("staging directory survived")
	}
}

func TestEditMarketplace_RenameAndResourceTogether(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	repoA := makeMarketplaceRepoWithPlugin(t, "acme", "widget")
	repoB := makeMarketplaceRepoWithPlugin(t, "acme", "gadget")
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceURL, URL: repoA}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if _, err := m.Install(ctx, "widget", "acme"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	ref, err := m.EditMarketplace(ctx, "acme", "beta", &Source{Kind: SourceURL, URL: repoB})
	if err != nil {
		t.Fatalf("EditMarketplace: %v", err)
	}
	if ref.InstallLocation != m.marketplaceDir("beta") || ref.Source.URL != repoB {
		t.Fatalf("ref = %+v", ref)
	}
	list, _ := m.ListMarketplaces()
	if _, ok := list["beta"]; !ok || len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	cat, err := m.Browse(ctx, "beta")
	if err != nil || len(cat.Plugins) != 1 || cat.Plugins[0].Name != "gadget" {
		t.Fatalf("Browse beta = %+v, %v", cat, err)
	}
	reg, _ := m.loadRegistry()
	entries, ok := reg.Plugins[registryKey("widget", "beta")]
	if !ok || len(entries) != 1 {
		t.Fatalf("registry = %v", reg.Plugins)
	}
	if _, err := os.Stat(entries[0].InstallPath); err != nil {
		t.Fatalf("the installed plugin is unaffected by a re-source, but its path is gone: %v", err)
	}
}

// A re-source from git to a directory is the one edit that deletes a directory
// after the files are saved — the clone the directory source makes redundant.
// Renaming in the same call moves that clone first, so the removal has to name
// the new name; naming the old one would leave the clone behind, and naming the
// directory source would destroy the marketplace itself.
func TestEditMarketplace_ResourceToDirectoryDropsTheRenamedClone(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	mktRepo, name := makeInstallableMarketplace(t)
	dir := makeDirectoryMarketplace(t, "beta", "gadget")
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceURL, URL: mktRepo}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if _, err := m.Install(ctx, "widget", name); err != nil {
		t.Fatalf("Install: %v", err)
	}

	ref, err := m.EditMarketplace(ctx, name, "beta", &Source{Kind: SourceDirectory, Path: dir})
	if err != nil {
		t.Fatalf("EditMarketplace: %v", err)
	}
	if ref.InstallLocation != dir {
		t.Fatalf("InstallLocation = %q, want the directory source %q", ref.InstallLocation, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "marketplace.json")); err != nil {
		t.Fatalf("the directory source itself was removed: %v", err)
	}
	for _, gone := range []string{name, "beta"} {
		if _, err := os.Stat(m.marketplaceDir(gone)); !os.IsNotExist(err) {
			t.Fatalf("clone directory %s outlived the re-source: %v", m.marketplaceDir(gone), err)
		}
	}
	reg, _ := m.loadRegistry()
	entries, ok := reg.Plugins[registryKey("widget", "beta")]
	if !ok || len(entries) != 1 {
		t.Fatalf("registry not re-keyed: %v", reg.Plugins)
	}
	if _, err := os.Stat(entries[0].InstallPath); err != nil {
		t.Fatalf("the installed plugin lives under the cache, not the clone, but its path is gone: %v", err)
	}
	cat, err := m.Browse(ctx, "beta")
	if err != nil || len(cat.Plugins) != 1 || cat.Plugins[0].Name != "gadget" {
		t.Fatalf("Browse beta = %+v, %v", cat, err)
	}
}

func TestEditMarketplace_FetchFailureChangesNothing(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	mktRepo, name := makeInstallableMarketplace(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceURL, URL: mktRepo}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if _, err := m.Install(ctx, "widget", name); err != nil {
		t.Fatalf("Install: %v", err)
	}
	bad := &Source{Kind: SourceURL, URL: filepath.Join(t.TempDir(), "does-not-exist")}
	if _, err := m.EditMarketplace(ctx, name, "beta", bad); err == nil {
		t.Fatal("expected the fetch to fail")
	}
	list, _ := m.ListMarketplaces()
	if _, ok := list[name]; !ok || len(list) != 1 {
		t.Fatalf("list changed after a failed fetch: %v", list)
	}
	if _, err := os.Stat(m.marketplaceDir(name)); err != nil {
		t.Fatalf("the clone moved after a failed fetch: %v", err)
	}
	if _, err := os.Stat(m.marketplaceDir("beta")); !os.IsNotExist(err) {
		t.Fatal("a beta directory appeared")
	}
	reg, _ := m.loadRegistry()
	if _, ok := reg.Plugins[registryKey("widget", name)]; !ok {
		t.Fatalf("registry changed after a failed fetch: %v", reg.Plugins)
	}
	if _, err := os.Stat(m.marketplaceDir(".staging")); !os.IsNotExist(err) {
		t.Fatal("staging directory survived")
	}
}

func TestEditMarketplace_RestoresDirectoriesWhenARenameStepFails(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	mktRepo, name := makeInstallableMarketplace(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceURL, URL: mktRepo}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if _, err := m.Install(ctx, "widget", name); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// The clone directory renames; the cache directory refuses. The undo
	// then runs through the same seam, so only the second call fails.
	orig := marketplaceRename
	calls := 0
	marketplaceRename = func(from, to string) error {
		calls++
		if calls == 2 {
			return errors.New("boom")
		}
		return orig(from, to)
	}
	t.Cleanup(func() { marketplaceRename = orig })

	if _, err := m.EditMarketplace(ctx, name, "beta", nil); err == nil {
		t.Fatal("expected the rename to fail")
	}
	if _, err := os.Stat(m.marketplaceDir(name)); err != nil {
		t.Fatalf("the clone was not restored: %v", err)
	}
	if _, err := os.Stat(m.marketplaceDir("beta")); !os.IsNotExist(err) {
		t.Fatal("a beta clone remained")
	}
	list, _ := m.ListMarketplaces()
	if _, ok := list[name]; !ok || len(list) != 1 {
		t.Fatalf("list changed after a failed rename: %v", list)
	}
	reg, _ := m.loadRegistry()
	if _, ok := reg.Plugins[registryKey("widget", name)]; !ok {
		t.Fatalf("registry changed after a failed rename: %v", reg.Plugins)
	}
}

// Once the staged clone is swapped in, the old clone's contents are gone, so
// the undo cannot restore them — renaming the directory back would leave the
// new source's files wearing the old name while the store still recorded the
// old source, and RefreshMarketplace pulls the clone's own origin, so every
// later refresh would serve the new remote forever. Removing the swapped-in
// clone leaves the recorded install location missing, which a refresh heals by
// recloning the recorded source.
func TestEditMarketplace_RegistrySaveFailureDropsTheSwappedInClone(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	repoA := makeMarketplaceRepoWithPlugin(t, "acme", "widget")
	repoB := makeMarketplaceRepoWithPlugin(t, "acme", "gadget")
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceURL, URL: repoA}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if _, err := m.Install(ctx, "widget", "acme"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// The last step before the marketplaces file, and the only one that runs
	// after the swap on a rename.
	origSave := installSaveRegistry
	installSaveRegistry = func(string, Registry) error { return errors.New("boom") }
	t.Cleanup(func() { installSaveRegistry = origSave })

	if _, err := m.EditMarketplace(ctx, "acme", "beta", &Source{Kind: SourceURL, URL: repoB}); err == nil {
		t.Fatal("expected the registry save to fail")
	}
	installSaveRegistry = origSave

	for _, dir := range []string{"acme", "beta"} {
		if _, err := os.Stat(m.marketplaceDir(dir)); !os.IsNotExist(err) {
			t.Fatalf("clone directory %s outlived the failed save: %v", m.marketplaceDir(dir), err)
		}
	}
	list, _ := m.ListMarketplaces()
	ref, ok := list["acme"]
	if !ok || len(list) != 1 || ref.Source.URL != repoA {
		t.Fatalf("the failed edit changed the store: %v", list)
	}
	reg, _ := m.loadRegistry()
	if _, ok := reg.Plugins[registryKey("widget", "acme")]; !ok {
		t.Fatalf("registry changed after a failed save: %v", reg.Plugins)
	}
	// The recorded source is what a refresh brings back, not the source the
	// failed edit had already fetched.
	if err := m.RefreshMarketplace(ctx, "acme"); err != nil {
		t.Fatalf("the refresh did not heal the missing clone: %v", err)
	}
	cat, err := m.Browse(ctx, "acme")
	if err != nil || len(cat.Plugins) != 1 || cat.Plugins[0].Name != "widget" {
		t.Fatalf("Browse acme = %+v, %v; want the recorded source's catalog", cat, err)
	}
}

// The marketplaces file is saved last and deliberately runs no undo — on a
// rename the registry is already committed to the new cache directory — but a
// clone the swap has already replaced still has to go, or a re-source that
// failed at that save keeps serving the new remote while recording the old.
func TestEditMarketplace_MarketplacesSaveFailureDropsTheSwappedInClone(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	repoA := makeMarketplaceRepoWithPlugin(t, "acme", "widget")
	repoB := makeMarketplaceRepoWithPlugin(t, "acme", "gadget")
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceURL, URL: repoA}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}

	// saveMarketplaces is the only writer through this seam, so the edit gets
	// all the way past the swap before it fails.
	origWrite := marketplaceAtomicWriteFile
	marketplaceAtomicWriteFile = func(string, []byte, os.FileMode) error { return errors.New("boom") }
	t.Cleanup(func() { marketplaceAtomicWriteFile = origWrite })

	if _, err := m.EditMarketplace(ctx, "acme", "", &Source{Kind: SourceURL, URL: repoB}); err == nil {
		t.Fatal("expected the save to fail")
	}
	marketplaceAtomicWriteFile = origWrite

	if _, err := os.Stat(m.marketplaceDir("acme")); !os.IsNotExist(err) {
		t.Fatalf("the swapped-in clone outlived the failed save: %v", err)
	}
	if err := m.RefreshMarketplace(ctx, "acme"); err != nil {
		t.Fatalf("the refresh did not heal the missing clone: %v", err)
	}
	cat, err := m.Browse(ctx, "acme")
	if err != nil || len(cat.Plugins) != 1 || cat.Plugins[0].Name != "widget" {
		t.Fatalf("Browse acme = %+v, %v; want the recorded source's catalog", cat, err)
	}
}

func TestEditMarketplace_RefusesALeftoverPluginCache(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available")
	}
	mktRepo, name := makeInstallableMarketplace(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()
	if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceURL, URL: mktRepo}); err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	if _, err := m.Install(ctx, "widget", name); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// What removing a marketplace named beta leaves behind: RemoveMarketplace
	// drops the clone and the registration, never the plugin cache.
	leftover := filepath.Join(m.cacheDir(), "beta")
	if err := os.MkdirAll(filepath.Join(leftover, "widget", "deadsha"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := m.EditMarketplace(ctx, name, "beta", nil)
	if err == nil {
		t.Fatal("expected the rename to be refused")
	}
	// Where an os.Rename LinkError said only "directory not empty".
	if want := fmt.Sprintf("plugin cache %s already exists", leftover); !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
	if _, err := os.Stat(m.marketplaceDir(name)); err != nil {
		t.Fatalf("the clone was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.cacheDir(), name)); err != nil {
		t.Fatalf("the plugin cache moved anyway: %v", err)
	}
	list, _ := m.ListMarketplaces()
	if _, ok := list[name]; !ok || len(list) != 1 {
		t.Fatalf("list changed after a refused rename: %v", list)
	}
}

func TestEditMarketplace_Refusals(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := context.Background()
	for _, name := range []string{"acme", "beta"} {
		dir := makeDirectoryMarketplace(t, name, "widget")
		if _, err := m.AddMarketplace(ctx, "", Source{Kind: SourceDirectory, Path: dir}); err != nil {
			t.Fatalf("AddMarketplace %s: %v", name, err)
		}
	}
	if _, err := m.EditMarketplace(ctx, "acme", "beta", nil); !errors.Is(err, ErrMarketplaceExists) {
		t.Fatalf("rename onto a taken name = %v, want ErrMarketplaceExists", err)
	}
	if _, err := m.EditMarketplace(ctx, "nope", "x", nil); !errors.Is(err, ErrMarketplaceNotFound) {
		t.Fatalf("unknown marketplace = %v, want ErrMarketplaceNotFound", err)
	}
	if _, err := m.EditMarketplace(ctx, "acme", "../escape", nil); err == nil {
		t.Fatal("a traversing name must be refused")
	}
}

func TestEditMarketplace_NoOpReturnsTheCurrentRef(t *testing.T) {
	dir := makeDirectoryMarketplace(t, "acme", "widget")
	m := NewManager(t.TempDir())
	ctx := context.Background()
	before, err := m.AddMarketplace(ctx, "", Source{Kind: SourceDirectory, Path: dir})
	if err != nil {
		t.Fatalf("AddMarketplace: %v", err)
	}
	same := Source{Kind: SourceDirectory, Path: dir}
	after, err := m.EditMarketplace(ctx, "acme", "acme", &same)
	if err != nil {
		t.Fatalf("EditMarketplace: %v", err)
	}
	if after != before {
		t.Fatalf("a no-op edit changed the ref: %+v → %+v", before, after)
	}
}

func TestRekeyRegistry(t *testing.T) {
	oldCache, newCache := filepath.Join("cache", "acme"), filepath.Join("cache", "beta")
	reg := Registry{Version: 2, Plugins: map[string][]InstallEntry{
		registryKey("widget", "acme"):    {{InstallPath: filepath.Join(oldCache, "widget", "abc")}},
		registryKey("other", "zeta"):     {{InstallPath: filepath.Join("cache", "zeta", "other", "def")}},
		registryKey("elsewhere", "acme"): {{InstallPath: filepath.Join("somewhere", "else")}},
		// An orphan a removed marketplace named beta left behind: removal drops
		// the registration but not the registry entries, so the rename target's
		// key is already taken and the live install has to win it.
		registryKey("widget", "beta"): {{InstallPath: filepath.Join(newCache, "widget", "ghost")}},
	}}
	got := rekeyRegistry(reg, "acme", "beta", oldCache, newCache)
	if _, still := got.Plugins[registryKey("widget", "acme")]; still {
		t.Fatal("old key survived")
	}
	if p := got.Plugins[registryKey("widget", "beta")][0].InstallPath; p != filepath.Join(newCache, "widget", "abc") {
		t.Fatalf("InstallPath = %q, want the moved entry to beat the orphan already under that key", p)
	}
	if p := got.Plugins[registryKey("other", "zeta")][0].InstallPath; p != filepath.Join("cache", "zeta", "other", "def") {
		t.Fatalf("an unrelated entry changed: %q", p)
	}
	if p := got.Plugins[registryKey("elsewhere", "beta")][0].InstallPath; p != filepath.Join("somewhere", "else") {
		t.Fatalf("a path outside the cache changed: %q", p)
	}
	if got.Version != 2 {
		t.Fatalf("Version = %d", got.Version)
	}
}
