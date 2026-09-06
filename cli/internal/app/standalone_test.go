package app

import (
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
	t.Chdir(t.TempDir())
	if got, err := findRoot(root); err != nil || got != root {
		t.Fatalf("explicit root = %q, %v", got, err)
	}
	if _, err := findRoot(""); err == nil {
		t.Fatal("accepted unrelated directory")
	}
	if _, err := findRoot(filepath.Join(root, "cli")); err == nil {
		t.Fatal("explicit --repo silently searched parent")
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

func TestUnsetSOPSPathStopsBeforeExternalTools(t *testing.T) {
	a, r := testApp(t)
	a.Config.SOPSFile = ""
	if err := a.Dispatch(t.Context(), []string{"connect"}); err == nil || !strings.Contains(err.Error(), "sops_file") || len(r.calls) != 0 {
		t.Fatalf("unset SOPS path: %v, calls: %v", err, r.calls)
	}
}

func TestStandaloneToolWorkingDirectories(t *testing.T) {
	a, r := testApp(t)
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
