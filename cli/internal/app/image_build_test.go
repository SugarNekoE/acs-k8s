package app

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func imageSourceFixture(t *testing.T, root string) {
	t.Helper()
	for name, data := range map[string]string{
		"acs-k8s/cli/go.mod":                             "module example.invalid/cli\ngo 1.26.0\n",
		"acs-k8s/cli/go.sum":                             "",
		"acs-k8s/cli/cmd/acs/main.go":                    "package main\nfunc main(){}\n",
		"acs-k8s/cli/internal/app/assets/fixture.yaml":   "kind: List\nitems: []\n",
		"acs-k8s/cli/internal/app/secret_test.go":        "synthetic test-only secret",
		"acs-k8s/cli/internal/app/testdata/fixture.json": "synthetic fixture",
		"acs-k8s/cli/local-auth.json":                    "must not enter image",
		"acs-k8s/acs-config.json":                        "synthetic operator configuration",
		"modules/secrets/synthetic.yaml":                 "synthetic secret",
		"acs-k8s/images/Containerfile":                   "FROM scratch AS go\n",
		"acs-k8s/images/rootfs/test.conf":                "public image configuration",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, path, data)
	}
}

func fakeImageBuild(t *testing.T, in Invocation) ([]byte, error) {
	t.Helper()
	if in.Args[0] == "buildah" && in.Args[1] == "build" {
		i := slices.Index(in.Args, "--iidfile")
		if i < 0 {
			t.Fatal("missing image ID output")
		}
		mustWrite(t, in.Args[i+1], "sha256:"+strings.Repeat("a", 64))
	}
	if in.Args[0] == "buildah" && in.Args[1] == "push" {
		target := strings.TrimPrefix(in.Args[len(in.Args)-1], "docker-archive:")
		i := strings.LastIndex(target, ":forgejo-runner-")
		if i < 0 {
			t.Fatal("invalid archive target")
		}
		mustWrite(t, target[:i], "synthetic archive")
	}
	return nil, nil
}

func TestImagesSeparateBuildPublish(t *testing.T) {
	for _, action := range []string{"build", "publish"} {
		t.Run(action, func(t *testing.T) {
			a, r := testApp(t)
			imageSourceFixture(t, a.Root)
			r.run = func(in Invocation) ([]byte, error) { return fakeImageBuild(t, in) }
			if err := a.images(t.Context(), []string{action, "go"}); err != nil {
				t.Fatal(err)
			}
			if r.calls[0].Args[0] != "buildah" || !slices.Contains(r.calls[0].Args, "linux/amd64") {
				t.Fatal(r.calls)
			}
			want := 2
			if action == "publish" {
				want = 3
			}
			if len(r.calls) != want {
				t.Fatal(r.calls)
			}
			for _, call := range r.calls {
				if slices.Contains([]string{"nix", "sops", "kubectl", "helm"}, call.Args[0]) {
					t.Fatal("image build invoked Nix or cluster tools")
				}
			}
			if action == "publish" && r.calls[2].Args[0] != "skopeo" {
				t.Fatal("publishing did not use Skopeo")
			}
			data, err := os.ReadFile(filepath.Join(a.Config.OutputDir, "runner-go-image"))
			if err != nil || string(data) != "synthetic archive" {
				t.Fatal("archive not published locally")
			}
		})
	}
}

func TestImageBuildFailuresPreserveExistingArchive(t *testing.T) {
	for _, failure := range []string{"build", "push", "image-id"} {
		t.Run(failure, func(t *testing.T) {
			a, r := testApp(t)
			imageSourceFixture(t, a.Root)
			archive := filepath.Join(a.Root, "image.tar")
			mustWrite(t, archive, "previous archive")
			var work string
			r.run = func(in Invocation) ([]byte, error) {
				if in.Args[1] == "build" {
					work = filepath.Dir(in.Args[len(in.Args)-1])
				}
				if in.Args[1] == failure {
					return nil, errors.New("synthetic failure")
				}
				out, err := fakeImageBuild(t, in)
				if failure == "image-id" && in.Args[1] == "build" {
					i := slices.Index(in.Args, "--iidfile")
					mustWrite(t, in.Args[i+1], "not-an-image")
				}
				return out, err
			}
			if err := a.buildImage(t.Context(), "go", archive); err == nil {
				t.Fatal("failure ignored")
			}
			data, err := os.ReadFile(archive)
			if err != nil || string(data) != "previous archive" {
				t.Fatal("previous archive damaged")
			}
			if _, err = os.Stat(work); !os.IsNotExist(err) {
				t.Fatal("temporary build context remains")
			}
		})
	}
}

func TestImageContextExcludesSecretsAndTestData(t *testing.T) {
	a, _ := testApp(t)
	imageSourceFixture(t, a.Root)
	destination := t.TempDir()
	if err := prepareImageContext(a.Root, destination); err != nil {
		t.Fatal(err)
	}
	var paths []string
	if err := filepath.WalkDir(destination, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(destination, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"Containerfile", "cli/cmd/acs/main.go", "cli/go.mod", "cli/go.sum", "cli/internal/app/assets/fixture.yaml", "rootfs/test.conf"}
	slices.Sort(paths)
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Fatalf("unexpected build inputs: %v", paths)
	}
	if err := os.Symlink(a.Config.SOPSFile, filepath.Join(a.Root, "acs-k8s/cli/cmd/acs/leak.go")); err != nil {
		t.Fatal(err)
	}
	if err := prepareImageContext(a.Root, t.TempDir()); err == nil {
		t.Fatal("external symlink entered build context")
	}
}

func TestImageCheckUsesNativeRuntimeAndCleansUp(t *testing.T) {
	for _, failRun := range []bool{false, true} {
		a, r := testApp(t)
		var output bytes.Buffer
		a.Out = &output
		r.run = func(in Invocation) ([]byte, error) {
			if in.Args[0] != "buildah" {
				t.Fatal("non-Buildah image check")
			}
			if in.Args[1] == "inspect" {
				return encoded(t, map[string]any{"OCIv1": map[string]any{"architecture": "amd64", "config": map[string]any{"User": "1000:1000", "WorkingDir": "/data", "Cmd": []string{"forgejo-runner", "one-job"}}}}), nil
			}
			if in.Args[1] == "run" {
				if !slices.Contains(in.Args, "--network") || !slices.Contains(in.Args, "none") || !slices.Contains(in.Args, "--cap-drop") || !slices.Contains(in.Args, "ALL") {
					t.Fatal("image test did not isolate network/capabilities")
				}
				if failRun {
					return nil, errors.New("runtime failed")
				}
				if slices.Contains(in.Args, "id") || slices.Contains(in.Args, "stat") {
					return []byte("1000\n"), nil
				}
				return []byte("Run only one job --ephemeral"), nil
			}
			return nil, nil
		}
		err := a.imageCheck(t.Context(), "synthetic.tar", "go")
		if (err != nil) != failRun {
			t.Fatal(err)
		}
		if r.calls[len(r.calls)-1].Args[1] != "rm" {
			t.Fatal("temporary container was not cleaned")
		}
	}
}
