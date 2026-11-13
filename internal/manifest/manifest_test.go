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

func TestLoadParsesShortAndInlineTableFormsToSameSpec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wam.toml")
	body := `[game]
flavor = "forever"

[addons]
"github:a/short" = "v1"
"github:a/table" = { version = "v1" }
"github:a/glob" = { version = "latest", asset = "*-nolib.zip" }
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]manifest.Addon{}
	for _, a := range m.Addons {
		byKey[a.Key] = a
	}
	if s, tb := byKey["github:a/short"], byKey["github:a/table"]; s.Version != tb.Version || s.Asset != tb.Asset || s.Asset != "" {
		t.Errorf("short %+v and table %+v differ", s, tb)
	}
	if g := byKey["github:a/glob"]; g.Version != "latest" || g.Asset != "*-nolib.zip" {
		t.Errorf("glob = %+v", g)
	}
	// Round trip keeps the asset.
	if err := manifest.Save(path, m); err != nil {
		t.Fatal(err)
	}
	m2, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range m.Addons {
		if m.Addons[i] != m2.Addons[i] {
			t.Errorf("round trip %+v != %+v", m2.Addons[i], m.Addons[i])
		}
	}
}

func TestLoadRejectsBadInlineTables(t *testing.T) {
	for name, entry := range map[string]string{
		"no version":  `{ asset = "*.zip" }`,
		"bad glob":    `{ version = "latest", asset = "[" }`,
		"unknown key": `{ version = "latest", flavor = "x" }`,
		"not a table": `42`,
	} {
		path := filepath.Join(t.TempDir(), "wam.toml")
		body := "[game]\nflavor = \"mainline\"\n[addons]\n\"github:a/b\" = " + entry + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := manifest.Load(path); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
