package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/clitest"
	"github.com/maddawik/wam/internal/locate"
	"github.com/maddawik/wam/internal/manifest"
)

// wowRoot makes an install root holding the given game folders.
func wowRoot(t *testing.T, folders ...string) *locate.Probe {
	t.Helper()
	root := t.TempDir()
	for _, f := range folders {
		if err := os.MkdirAll(filepath.Join(root, f, "Interface", "AddOns"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &locate.Probe{Roots: []string{root}}
}

func initRun(t *testing.T, o clitest.Opts) (clitest.Result, string) {
	t.Helper()
	cfg := t.TempDir()
	if o.Env == nil {
		o.Env = map[string]string{}
	}
	o.Env["XDG_CONFIG_HOME"] = cfg
	o.Args = append([]string{"init"}, o.Args...)
	return clitest.Run(t, o), filepath.Join(cfg, "wam", "wam.toml")
}

func TestInitWithOneFlavorWritesManifestWithoutPrompting(t *testing.T) {
	res, file := initRun(t, clitest.Opts{Probe: wowRoot(t, "_retail_"), StdinTTY: true})
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "mainline") || !strings.Contains(got, "[addons]") || strings.Contains(got, "dir =") {
		t.Errorf("manifest:\n%s", got)
	}
	if strings.Contains(res.Stderr, "?") {
		t.Errorf("prompted: %s", res.Stderr)
	}
}

func TestInitManifestIsAcceptedByLoad(t *testing.T) {
	res, file := initRun(t, clitest.Opts{Probe: wowRoot(t, "_retail_")})
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	m, err := manifest.Load(file)
	if err != nil || m.Game.Flavor != "mainline" || len(m.Addons) != 0 {
		t.Errorf("got %+v, %v", m, err)
	}
}

func TestInitWithSeveralFlavorsPromptsAtATerminal(t *testing.T) {
	res, file := initRun(t, clitest.Opts{Probe: wowRoot(t, "_retail_", "_classic_era_"), StdinTTY: true, Stdin: "2\n"})
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	m, err := manifest.Load(file)
	if err != nil || m.Game.Flavor != "classic" {
		t.Errorf("got %+v, %v", m, err)
	}
	if !strings.Contains(res.Stderr, "wam init --flavor") {
		t.Errorf("no --flavor hint: %s", res.Stderr)
	}
}

func TestInitPromptAcceptsAFlavorName(t *testing.T) {
	res, file := initRun(t, clitest.Opts{Probe: wowRoot(t, "_retail_", "_classic_era_"), StdinTTY: true, Stdin: "mainline\n"})
	m, err := manifest.Load(file)
	if res.ExitCode != 0 || err != nil || m.Game.Flavor != "mainline" {
		t.Errorf("exit %d, %+v, %v", res.ExitCode, m, err)
	}
}

func TestInitPromptRejectsAnUnlistedAnswer(t *testing.T) {
	res, _ := initRun(t, clitest.Opts{Probe: wowRoot(t, "_retail_", "_classic_era_"), StdinTTY: true, Stdin: "wrath\n"})
	if res.ExitCode != 2 {
		t.Errorf("exit %d: %s", res.ExitCode, res.Stderr)
	}
}

func TestInitWithSeveralFlavorsAndNoTerminalExits2NamingFlag(t *testing.T) {
	res, file := initRun(t, clitest.Opts{Probe: wowRoot(t, "_retail_", "_classic_era_")})
	if res.ExitCode != 2 || !strings.Contains(res.Stderr, "--flavor") {
		t.Errorf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("manifest written")
	}
}

func TestInitNoInputActsLikeNoTerminal(t *testing.T) {
	res, _ := initRun(t, clitest.Opts{Args: []string{"--no-input"}, Probe: wowRoot(t, "_retail_", "_classic_era_"), StdinTTY: true})
	if res.ExitCode != 2 {
		t.Errorf("exit %d", res.ExitCode)
	}
}

func TestInitFlavorFlagSkipsDetection(t *testing.T) {
	res, file := initRun(t, clitest.Opts{Args: []string{"--flavor", "classic"}, Probe: wowRoot(t, "_retail_", "_classic_era_")})
	m, err := manifest.Load(file)
	if res.ExitCode != 0 || err != nil || m.Game.Flavor != "classic" || m.Game.Dir != "" {
		t.Errorf("exit %d, %+v, %v: %s", res.ExitCode, m, err, res.Stderr)
	}
}

func TestInitUnknownFlavorIsUsageError(t *testing.T) {
	res, _ := initRun(t, clitest.Opts{Args: []string{"--flavor", "nope"}, Probe: wowRoot(t)})
	if res.ExitCode != 2 {
		t.Errorf("exit %d", res.ExitCode)
	}
}

func TestInitWritesDirWhenFlavorIsNotAutoDetected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "AddOns")
	res, file := initRun(t, clitest.Opts{Args: []string{"--flavor", "mainline", "--dir", dir}, Probe: wowRoot(t)})
	m, err := manifest.Load(file)
	if res.ExitCode != 0 || err != nil || m.Game.Dir != dir {
		t.Errorf("exit %d, %+v, %v: %s", res.ExitCode, m, err, res.Stderr)
	}
}

func TestInitOmitsDirWhenFlavorIsDetected(t *testing.T) {
	res, file := initRun(t, clitest.Opts{Args: []string{"--flavor", "mainline"}, Probe: wowRoot(t, "_retail_")})
	m, err := manifest.Load(file)
	if res.ExitCode != 0 || err != nil || m.Game.Dir != "" {
		t.Errorf("exit %d, %+v, %v", res.ExitCode, m, err)
	}
}

func TestInitWithNothingDetectedNeedsFlavor(t *testing.T) {
	res, _ := initRun(t, clitest.Opts{Probe: wowRoot(t), StdinTTY: true})
	if res.ExitCode != 2 || !strings.Contains(res.Stderr, "--flavor") {
		t.Errorf("exit %d: %s", res.ExitCode, res.Stderr)
	}
}

func TestInitRefusesToReplaceManifestWithExit5(t *testing.T) {
	cfg := t.TempDir()
	file := filepath.Join(cfg, "wam.toml")
	if err := os.WriteFile(file, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := clitest.Run(t, clitest.Opts{Args: []string{"init", "-f", file, "--flavor", "mainline"}, Probe: wowRoot(t)})
	if res.ExitCode != 5 || !strings.Contains(res.Stderr, "--overwrite") {
		t.Errorf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	if b, _ := os.ReadFile(file); string(b) != "keep" {
		t.Errorf("manifest changed: %s", b)
	}
	res = clitest.Run(t, clitest.Opts{Args: []string{"init", "-f", file, "--flavor", "mainline", "--overwrite"}, Probe: wowRoot(t)})
	m, err := manifest.Load(file)
	if res.ExitCode != 0 || err != nil || m.Game.Flavor != "mainline" {
		t.Errorf("exit %d, %+v, %v: %s", res.ExitCode, m, err, res.Stderr)
	}
}

func TestInitCreatesMissingConfigDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a", "b", "wam.toml")
	res := clitest.Run(t, clitest.Opts{Args: []string{"init", "-f", file}, Probe: wowRoot(t, "_retail_")})
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	if _, err := manifest.Load(file); err != nil {
		t.Error(err)
	}
}

func TestInitWritesAnAbsoluteDir(t *testing.T) {
	res, file := initRun(t, clitest.Opts{Args: []string{"--flavor", "mainline", "--dir", "AddOns"}, Probe: wowRoot(t)})
	m, err := manifest.Load(file)
	if res.ExitCode != 0 || err != nil || !filepath.IsAbs(m.Game.Dir) {
		t.Errorf("exit %d, %+v, %v", res.ExitCode, m, err)
	}
}
