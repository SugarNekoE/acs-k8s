package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var imageIDPattern = regexp.MustCompile(`^(sha256:)?[a-f0-9]{64}$`)

func imageArchitecture(system string) (string, error) {
	if err := validSystem(system); err != nil {
		return "", err
	}
	if system == "aarch64-linux" {
		return "arm64", nil
	}
	return "amd64", nil
}

// Build contexts contain only image recipes and Go build inputs. In particular,
// operator JSON, deployment JSON, SOPS documents, Git state and test fixtures
// never enter a build context.
func prepareImageContext(root, destination string) error {
	copyTree := func(source, target string, include func(string) bool) error {
		return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			if entry.IsDir() {
				if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "testdata" || entry.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			if !include(filepath.ToSlash(rel)) {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlinks are not allowed in image build inputs: %s", rel)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("non-regular image build input: %s", rel)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out := filepath.Join(target, rel)
			if err = os.MkdirAll(filepath.Dir(out), 0700); err != nil {
				return err
			}
			return os.WriteFile(out, data, 0600)
		})
	}
	if err := copyTree(filepath.Join(root, "cli"), filepath.Join(destination, "cli"), func(rel string) bool {
		return rel == "go.mod" || rel == "go.sum" || (strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go")) || strings.HasPrefix(rel, "internal/app/assets/")
	}); err != nil {
		return err
	}
	if err := copyTree(filepath.Join(root, "images"), destination, func(rel string) bool { return rel == "Containerfile" || strings.HasPrefix(rel, "rootfs/") }); err != nil {
		return err
	}
	for _, required := range []string{"Containerfile", "cli/go.mod", "cli/cmd/acs/main.go"} {
		if _, err := os.Stat(filepath.Join(destination, required)); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) buildImage(ctx context.Context, pool, archive string) (result error) {
	arch, err := imageArchitecture(a.Config.System)
	if err != nil {
		return err
	}
	if _, err = selectPools([]string{pool}); err != nil {
		return err
	}
	if err = a.requireSourceFiles("images build", "cli/go.mod", "images/Containerfile"); err != nil {
		return err
	}
	work, err := privateDir("acs-image-build-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, removePrivate(work)) }()
	buildContext := filepath.Join(work, "context")
	if err = os.Mkdir(buildContext, 0700); err != nil {
		return err
	}
	if err = prepareImageContext(a.Root, buildContext); err != nil {
		return err
	}
	iid := filepath.Join(work, "image-id")
	if err = a.command(ctx, "buildah", "build", "--platform", "linux/"+arch, "--target", pool, "--layers", "--force-rm", "--http-proxy=false", "--iidfile", iid, "--build-arg", "BUILDPLATFORM=linux/"+runtime.GOARCH, "--build-arg", "TARGETARCH="+arch, "--file", filepath.Join(buildContext, "Containerfile"), buildContext); err != nil {
		return err
	}
	idBytes, err := os.ReadFile(iid)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(string(idBytes))
	if !imageIDPattern.MatchString(id) {
		return errors.New("buildah returned an invalid image ID")
	}
	if err = os.MkdirAll(filepath.Dir(archive), 0755); err != nil {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(archive), ".acs-image-")
	if err != nil {
		return err
	}
	temporary := output.Name()
	defer func() {
		if err := os.Remove(temporary); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}()
	if err = output.Close(); err != nil {
		return err
	}
	ref := "forgejo-runner-" + pool + ":" + tag
	if err = a.command(ctx, "buildah", "push", id, "docker-archive:"+temporary+":"+ref); err != nil {
		return err
	}
	info, err := os.Stat(temporary)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("buildah produced an empty image archive")
	}
	// Renaming replaces a previous file or Nix out-link without following it.
	return os.Rename(temporary, archive)
}
