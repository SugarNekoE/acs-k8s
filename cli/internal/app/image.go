package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
)

// Image checks import the archive and run its actual filesystem and environment
// through Buildah, with networking disabled and all capabilities dropped.
func (a *App) imageChecks(ctx context.Context, archives []string) error {
	for i, pool := range []string{"go", "node24", "docker", "nix"} {
		if err := a.imageCheck(ctx, archives[i], pool); err != nil {
			return fmt.Errorf("%s image: %w", pool, err)
		}
	}
	return nil
}

func (a *App) imageCheck(ctx context.Context, archive, pool string) (result error) {
	work, err := privateDir("acs-image-check-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, removePrivate(work)) }()
	name := filepath.Base(work)
	if err = a.command(ctx, "buildah", "from", "--name", name, "docker-archive:"+archive); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		result = errors.Join(result, a.command(cleanup, "buildah", "rm", name))
	}()
	b, err := a.capture(ctx, "buildah", "inspect", "--type", "container", name)
	if err != nil {
		return err
	}
	var inspection struct {
		OCIv1 struct {
			Architecture string `json:"architecture"`
			Config       struct {
				User                 string
				WorkingDir           string
				Cmd, Entrypoint, Env []string
			} `json:"config"`
		}
	}
	if err = json.Unmarshal(b, &inspection); err != nil {
		return err
	}
	expectedArch := runtime.GOARCH
	if a.Config.System != "" {
		expectedArch, err = imageArchitecture(a.Config.System)
		if err != nil {
			return err
		}
	}
	if inspection.OCIv1.Architecture != expectedArch {
		return errors.New("image architecture does not match the selected system")
	}
	config := inspection.OCIv1.Config
	expectedUser := "1000:1000"
	if pool == "nix" || pool == "docker" {
		expectedUser = "0:0"
	}
	if config.User != expectedUser || config.WorkingDir != "/data" || !reflect.DeepEqual(config.Cmd, []string{"forgejo-runner", "one-job"}) || len(config.Entrypoint) != 0 {
		return errors.New("image user, working directory or runner entrypoint is incorrect")
	}
	run := func(capture bool, command ...string) ([]byte, error) {
		args := []string{"buildah", "run", "--network", "none", "--cap-drop", "ALL", "--user", expectedUser, name, "--"}
		return a.call(ctx, append(args, command...), nil, capture, false, nil)
	}
	for _, command := range [][]string{{"/usr/bin/env", "bash", "-c", "echo env-shebang-ok"}, {"forgejo-runner", "--version"}, {"acs", "--help"}, {"acs", "workflow", "image-test", pool}} {
		if _, err = run(false, command...); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		args []string
		want string
	}{
		{[]string{"forgejo-runner", "one-job", "--help"}, "Run only one job"},
		{[]string{"forgejo-runner", "register", "--help"}, "--ephemeral"},
	} {
		b, err = run(true, item.args...)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), item.want) {
			return errors.New("runner lacks required ephemeral one-job support")
		}
	}
	b, err = run(true, "id", "-u")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != strings.Split(expectedUser, ":")[0] {
		return errors.New("image runtime UID differs from configuration")
	}
	b, err = run(true, "stat", "-c", "%u", "/data/home")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != strings.Split(expectedUser, ":")[0] {
		return errors.New("image HOME is not owned by its configured user")
	}
	if pool != "nix" {
		if _, err = run(false, "test", "!", "-e", "/nix"); err != nil {
			return errors.New("non-Nix runner unexpectedly contains a Nix store")
		}
	}
	if pool == "nix" {
		for _, command := range [][]string{{"direnv", "version"}, {"cachix", "--version"}, {"attic", "--version"}} {
			if _, err = run(false, command...); err != nil {
				return err
			}
		}
	}
	if _, err = run(false, "test", "-s", "/etc/ssl/certs/ca-bundle.crt"); err != nil {
		return err
	}
	a.message("PASS: %s image and embedded CLI", pool)
	return nil
}

func (a *App) builderBinaries(ctx context.Context) error {
	for _, tool := range []string{"buildah", "skopeo", "crun"} {
		short, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := a.command(short, tool, "--version")
		cancel()
		if err != nil {
			return err
		}
	}
	for _, path := range []string{"/etc/containers/policy.json", "/etc/containers/storage.conf"} {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(b) == 0 {
			return fmt.Errorf("empty %s", path)
		}
		if strings.HasSuffix(path, "storage.conf") && !strings.Contains(string(b), "vfs") {
			return errors.New("builder must use vfs storage")
		}
	}
	if _, err := os.Stat("/var/run/docker.sock"); !os.IsNotExist(err) {
		return errors.New("unexpected Docker socket or inspection failure")
	}
	a.message("PASS: Buildah/Skopeo/crun and builder configuration (ACS build/push remains separate)")
	return nil
}
