package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStandaloneRootDiscovery(t *testing.T) {
	root := t.TempDir()
	imageSourceFixture(t, root)
	for _, dir := range []string{root, filepath.Join(root, "cli"), filepath.Join(root, "cli/internal/app")} {
		t.Chdir(dir)
		got, err := findRoot("")
		if err != nil || got != root {
			t.Fatalf("findRoot from %s = %q, %v", dir, got, err)
		}
	}
	outside := t.TempDir()
	t.Chdir(outside)
	if got, err := findRoot(root); err != nil || got != root {
		t.Fatalf("explicit root = %q, %v", got, err)
	}
	if got, err := findRoot(""); err != nil || got != outside {
		t.Fatalf("operator directory = %q, %v", got, err)
	}
	if got, err := findRoot(filepath.Join(root, "cli")); err != nil || got != filepath.Join(root, "cli") {
		t.Fatalf("explicit directory silently searched parent: %q, %v", got, err)
	}
	for _, path := range []string{filepath.Join(root, "missing"), filepath.Join(root, "cli/go.mod")} {
		if _, err := findRoot(path); err == nil {
			t.Fatalf("accepted invalid directory %s", path)
		}
	}
}

func TestStandaloneConfigDefaults(t *testing.T) {
	for _, key := range []string{"ACR", "ACR_NAMESPACE", "IMAGE_SYSTEM", "CTX", "ACS_SOPS_FILE", "ACS_DEPLOYMENT_FILE", "ACS_OUTPUT_DIR", "ACS_BUNDLE", "ACS_RUNNER_NAMESPACE", "ACS_KEDA_NAMESPACE"} {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c, err := loadConfig(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.DeploymentFile != filepath.Join(root, "acs-deployment.json") || c.SOPSFile != "" {
		t.Fatalf("incorrect standalone defaults: %+v", c)
	}
	mustWrite(t, filepath.Join(root, "acs-config.json"), `{"context":"standalone","sops_file":"secrets/credentials.yaml","output_dir":".output"}`)
	c, err = loadConfig(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != "standalone" || c.SOPSFile != filepath.Join(root, "secrets/credentials.yaml") || c.Bundle != filepath.Join(root, ".output/bundle") {
		t.Fatalf("root config not resolved: %+v", c)
	}
	secretPath := filepath.Join(t.TempDir(), "external.yaml")
	c, err = loadConfig(root, map[string]string{"sops-file": secretPath})
	if err != nil || c.SOPSFile != secretPath {
		t.Fatalf("absolute SOPS override = %q, %v", c.SOPSFile, err)
	}
}

func TestInstalledCLIWithoutCheckout(t *testing.T) {
	for _, key := range []string{"ACR", "ACR_NAMESPACE", "IMAGE_SYSTEM", "CTX", "ACS_SOPS_FILE", "ACS_DEPLOYMENT_FILE", "ACS_OUTPUT_DIR", "ACS_BUNDLE", "ACS_RUNNER_NAMESPACE", "ACS_KEDA_NAMESPACE"} {
		t.Setenv(key, "")
	}
	out := t.TempDir()
	t.Setenv("XDG_STATE_HOME", out)
	flake := t.TempDir()
	mustWrite(t, filepath.Join(flake, "flake.nix"), "{}")
	operator := t.TempDir()
	settings := `{"context":"configured","sops_file":"credentials.yaml","output_dir":".output"}`
	mustWrite(t, filepath.Join(operator, "profile.json"), settings)
	mustWrite(t, filepath.Join(operator, "acs-config.json"), settings)
	subdir := filepath.Join(operator, "reports")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cwd, root string
		flags           []string
		configured      bool
	}{
		{name: "unrelated flake", cwd: flake, root: flake},
		{name: "operator directory", cwd: operator, root: operator, configured: true},
		{name: "operator subdirectory", cwd: subdir, root: operator, configured: true},
		{name: "absolute profile", cwd: flake, root: operator, flags: []string{"--config", filepath.Join(operator, "profile.json")}, configured: true},
		{name: "relative profile", cwd: subdir, root: operator, flags: []string{"--config", "../profile.json"}, configured: true},
		{name: "explicit operator directory", cwd: flake, root: operator, flags: []string{"--repo", operator}, configured: true},
		{name: "repo overrides profile directory", cwd: flake, root: flake, flags: []string{"--repo", flake, "--config", filepath.Join(operator, "profile.json")}, configured: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			var stdout bytes.Buffer
			args := append(append([]string{}, tc.flags...), "config", "show")
			if err := Run(t.Context(), args, nil, &stdout, io.Discard); err != nil {
				t.Fatal(err)
			}
			var c Config
			if err := json.Unmarshal(stdout.Bytes(), &c); err != nil {
				t.Fatal(err)
			}
			if c.DeploymentFile != filepath.Join(tc.root, "acs-deployment.json") {
				t.Fatalf("wrong operator root: %+v", c)
			}
			if tc.configured && (c.Context != "configured" || c.SOPSFile != filepath.Join(tc.root, "credentials.yaml") || c.Bundle != filepath.Join(tc.root, ".output/bundle")) {
				t.Fatalf("profile paths not resolved: %+v", c)
			}
		})
	}
}

func TestSourceCommandsFailBeforeExternalTools(t *testing.T) {
	for _, command := range [][]string{{"shell"}, {"validate", "cli"}, {"images", "build", "go"}} {
		a, r := testApp(t)
		// An unrelated workstation flake must not be opened as the ACS shell.
		mustWrite(t, filepath.Join(a.Root, "flake.nix"), "{}")
		err := a.Dispatch(t.Context(), command)
		if err == nil || !strings.Contains(err.Error(), "ACS checkout") || !strings.Contains(err.Error(), "--repo") || len(r.calls) != 0 {
			t.Fatalf("source guard for %v: %v, calls: %v", command, err, r.calls)
		}
	}
}

func TestUnsetSOPSPathStopsBeforeExternalTools(t *testing.T) {
	a, r := testApp(t)
	a.Config.SOPSFile = ""
	if err := a.Dispatch(t.Context(), []string{"connect"}); err == nil || !strings.Contains(err.Error(), "sops_file") || len(r.calls) != 0 {
		t.Fatalf("unset SOPS path: %v, calls: %v", err, r.calls)
	}
}

func TestStandaloneToolWorkingDirectories(t *testing.T) {
	a, r := testApp(t)
	imageSourceFixture(t, a.Root)
	mustWrite(t, filepath.Join(a.Root, "flake.nix"), "{}")
	if err := a.Dispatch(t.Context(), []string{"shell"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"nix", "develop", a.Root, "--command", "bash"}
	if len(r.calls) != 1 || !reflect.DeepEqual(r.calls[0].Args, want) || r.calls[0].Dir != a.Root {
		t.Fatalf("shell: %v", r.calls)
	}
	r.calls = nil
	if err := a.validate(t.Context(), []string{"cli"}); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("CLI validation calls: %v", r.calls)
	}
	for _, call := range r.calls {
		if call.Dir != filepath.Join(a.Root, "cli") {
			t.Fatalf("validation outside standalone module: %v", call)
		}
	}
}

func TestKindFailureWritesReportAtStandaloneRoot(t *testing.T) {
	a, r := testApp(t)
	failure := errors.New("synthetic cluster failure")
	r.run = func(Invocation) ([]byte, error) { return nil, failure }
	k := &Kind{A: a}
	if err := k.test(t.Context()); !errors.Is(err, failure) {
		t.Fatalf("Kind failure: %v", err)
	}
	path := filepath.Join(a.Root, "acs-kind-test-report.md")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), failure.Error()) {
		t.Fatalf("missing failure report: %s, %v", data, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("report permissions: %v, %v", st, err)
	}
}

func TestPlatformReadsStandaloneSnapshots(t *testing.T) {
	a, _ := testApp(t)
	a.Exec = OSRunner{Out: io.Discard, Err: io.Discard}
	c := fixtureDeployment(t)
	a.Config.DeploymentFile = filepath.Join(a.Root, "acs-deployment.json")
	mustWrite(t, a.Config.DeploymentFile, string(encoded(t, c)))
	m, err := c.render()
	if err != nil {
		t.Fatal(err)
	}
	files, err := m.files()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range snapshots {
		mustWrite(t, filepath.Join(a.Root, name), string(files[name]))
	}
	if err := a.validatePlatform(t.Context()); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(a.Root, snapshots[0]), "kind: stale\n")
	if err := a.validatePlatform(t.Context()); err == nil || !strings.Contains(err.Error(), "stale snapshot") {
		t.Fatalf("stale root snapshot: %v", err)
	}
}
