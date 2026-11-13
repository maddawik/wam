package install

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/errs"
)

func noOwner(string) (string, bool) { return "", false }

// writeZip writes a zip of name→content pairs to a temp file. Names ending in
// "/" are directories.
func writeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// addOns makes an AddOns dir holding folder→file→content.
func addOns(t *testing.T, tree map[string]map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Interface", "AddOns")
	for folder, files := range tree {
		if err := os.MkdirAll(filepath.Join(dir, folder), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, folder, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// entries lists the names in dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// assertNoLeftovers fails if dir holds anything wam uses only while working.
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	for _, n := range entries(t, dir) {
		if strings.HasPrefix(n, ".wam-") {
			t.Errorf("%s left in %s", n, dir)
		}
	}
}

// failRename makes rename fail for moves into addonsDir out of an extraction
// temp dir, i.e. step 2 of a swap, for the rest of the test.
func failRename(t *testing.T, fail func(oldpath, newpath string) error) {
	t.Helper()
	t.Cleanup(func() { rename = os.Rename })
	rename = func(oldpath, newpath string) error {
		if err := fail(oldpath, newpath); err != nil {
			return err
		}
		return os.Rename(oldpath, newpath)
	}
}

func placing(oldpath string) bool {
	return strings.Contains(filepath.Base(filepath.Dir(oldpath)), tmpPrefix)
}

func TestZipUpgradeReplacesOwnedFolders(t *testing.T) {
	dir := addOns(t, map[string]map[string]string{
		"Alpha":    {"a.lua": "old", "gone.lua": "old"},
		"AlphaOld": {"x.lua": "old"},
	})
	zp := writeZip(t, map[string]string{"Alpha/a.lua": "new", "AlphaNew/y.lua": "new"})

	folders, err := Zip(context.Background(), zp, dir, []string{"Alpha", "AlphaOld"}, noOwner)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Errorf("folders = %v", folders)
	}
	if got := readFile(t, filepath.Join(dir, "Alpha", "a.lua")); got != "new" {
		t.Errorf("a.lua = %q", got)
	}
	for _, p := range []string{"Alpha/gone.lua", "AlphaOld"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			t.Errorf("%s survived the upgrade", p)
		}
	}
	assertNoLeftovers(t, dir)
}

func TestZipRestoresOriginalWhenPlacingFails(t *testing.T) {
	dir := addOns(t, map[string]map[string]string{"Alpha": {"a.lua": "old"}, "AlphaOptions": {"o.lua": "old"}})
	zp := writeZip(t, map[string]string{"Alpha/a.lua": "new", "AlphaOptions/o.lua": "new"})
	placed := 0
	failRename(t, func(oldpath, _ string) error {
		if !placing(oldpath) {
			return nil
		}
		if placed++; placed == 2 {
			return errors.New("disk on fire")
		}
		return nil
	})

	_, err := Zip(context.Background(), zp, dir, []string{"Alpha", "AlphaOptions"}, noOwner)
	if err == nil {
		t.Fatal("Zip succeeded although placing failed")
	}
	for _, p := range []string{"Alpha/a.lua", "AlphaOptions/o.lua"} {
		if got := readFile(t, filepath.Join(dir, filepath.FromSlash(p))); got != "old" {
			t.Errorf("%s = %q, want the original", p, got)
		}
	}
	assertNoLeftovers(t, dir)
}

func TestZipRejectsEscapingEntriesBeforeWritingAnything(t *testing.T) {
	for _, bad := range []string{"../evil.lua", "Alpha/../../evil.lua", "/abs/evil.lua", `..\evil.lua`, `C:\evil.lua`, "C:/evil.lua"} {
		t.Run(bad, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "Interface", "AddOns")
			// The good entry comes first: nothing may be written even so.
			zp := writeZip(t, map[string]string{"Alpha/a.lua": "a", bad: "x"})

			_, err := Zip(context.Background(), zp, dir, nil, noOwner)
			if e, ok := errs.As(err); !ok || e.Kind != errs.Conflict {
				t.Fatalf("err = %v, want a conflict", err)
			}
			if got := entries(t, root); len(got) != 0 {
				t.Errorf("wrote %v although the zip was refused", got)
			}
		})
	}
}

func TestZipStopsWhenCancelledAndLeavesNoTempDirs(t *testing.T) {
	dir := addOns(t, map[string]map[string]string{"Alpha": {"a.lua": "old"}})
	zp := writeZip(t, map[string]string{"Alpha/a.lua": "new"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Zip(ctx, zp, dir, []string{"Alpha"}, noOwner)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := readFile(t, filepath.Join(dir, "Alpha", "a.lua")); got != "old" {
		t.Errorf("a.lua = %q, want the original", got)
	}
	assertNoLeftovers(t, dir)
}

// crash is what a killed process leaves: rename panics on the nth call made
// for which match is true, so nothing after it, rollback included, runs.
type crash struct{}

func zipUntilCrash(t *testing.T, dir, zp string, owned []string, match func(oldpath string) bool, n int) {
	t.Helper()
	calls := 0
	failRename(t, func(oldpath, _ string) error {
		if match(oldpath) {
			if calls++; calls == n {
				panic(crash{})
			}
		}
		return nil
	})
	defer func() {
		if r := recover(); r != (crash{}) {
			t.Fatalf("Zip did not crash: %v", r)
		}
		rename = os.Rename
	}()
	_, _ = Zip(context.Background(), zp, dir, owned, noOwner)
}

func TestRecoverPutsBackFoldersAfterACrash(t *testing.T) {
	inAddOns := func(oldpath string) bool { return !placing(oldpath) }
	for _, tc := range []struct {
		name  string
		match func(string) bool
		n     int
	}{
		{"while moving the old folders aside", inAddOns, 2},
		{"while placing the new folders", placing, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := addOns(t, map[string]map[string]string{"Alpha": {"a.lua": "old"}, "AlphaOptions": {"o.lua": "old"}})
			zp := writeZip(t, map[string]string{"Alpha/a.lua": "new", "AlphaOptions/o.lua": "new", "AlphaExtra/e.lua": "new"})
			zipUntilCrash(t, dir, zp, []string{"Alpha", "AlphaOptions"}, tc.match, tc.n)

			if err := Recover(dir); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{"Alpha/a.lua", "AlphaOptions/o.lua"} {
				if got := readFile(t, filepath.Join(dir, filepath.FromSlash(p))); got != "old" {
					t.Errorf("%s = %q, want the original", p, got)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "AlphaExtra")); err == nil {
				t.Error("a half-placed new folder survived recovery")
			}
			assertNoLeftovers(t, dir)
		})
	}
}

func TestRecoverDeletesStaleTempFiles(t *testing.T) {
	dir := addOns(t, map[string]map[string]string{tmpPrefix + "123": {"x": "x"}, "Alpha": {"a.lua": "a"}})
	if err := os.WriteFile(filepath.Join(dir, tmpPrefix+"456.zip"), []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Recover(dir); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, dir); len(got) != 1 || got[0] != "Alpha" {
		t.Errorf("AddOns = %v, want just Alpha", got)
	}
}
