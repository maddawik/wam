package manifest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/maddawik/wam/internal/manifest"
)

func TestSaveThenLoadRoundTripsAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wam.toml")
	want := &manifest.Manifest{
		Game: manifest.Game{Flavor: "mainline"},
		Addons: []manifest.Addon{
			{Key: "github:acme/alpha", Source: "github", ID: "acme/alpha", Version: "latest"},
			{Key: "github:acme/beta", Source: "github", ID: "acme/beta", Version: "latest"},
		},
	}
	if err := manifest.Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Game != want.Game || len(got.Addons) != 2 || got.Addons[0] != want.Addons[0] || got.Addons[1] != want.Addons[1] {
		t.Errorf("got %+v, want %+v", got, want)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want only wam.toml", len(entries))
	}
}

func TestLoadRejectsManifestWithoutFlavor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wam.toml")
	if err := os.WriteFile(path, []byte("[addons]\n\"github:a/b\" = \"latest\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Load(path); err == nil {
		t.Error("want an error for a manifest with no game.flavor")
	}
}
