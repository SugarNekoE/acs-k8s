package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

const chartName = "acs-keda-2.20.2.tgz"
const chartDigest = "22Cx+mDwVl/vpM4vh+hVtqiFQ2PugpYvviihKnDWTEw="

func jsonValue(value any) (any, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out any
	if err = json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}
func manifestYAML(value any) ([]byte, error) {
	normalized, err := jsonValue(value)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(normalized)
}
func (m Manifests) files() (map[string][]byte, error) {
	files := map[string][]byte{}
	for name, value := range map[string]any{"acs-namespaces.yaml": objectsList(m.Namespaces), "acs-forgejo-runners.yaml": objectsList(m.Resources), "acs-external-secrets.yaml": objectsList(m.ExternalSecrets), "acs-keda-values.yaml": m.KEDAValues} {
		b, err := manifestYAML(value)
		if err != nil {
			return nil, err
		}
		files[name] = b
	}
	plan, err := json.MarshalIndent(m.Plan, "", "  ")
	if err != nil {
		return nil, err
	}
	files["acs-deploy-plan.json"] = append(plan, '\n')
	return files, nil
}
func pinnedChart() ([]byte, error) {
	chart, err := assets.ReadFile("assets/" + chartName)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(chart)
	if base64.StdEncoding.EncodeToString(sum[:]) != chartDigest {
		return nil, errors.New("embedded KEDA chart checksum mismatch")
	}
	return chart, nil
}
func (a *App) deployment() (DeploymentConfig, error) { return loadDeployment(a.Config.DeploymentFile) }

// writeBundle creates only local artifacts. Helm rendering never contacts a cluster.
func (a *App) writeBundle(ctx context.Context, c DeploymentConfig, dir string) (result error) {
	if !c.Enable {
		return errors.New("cannot build a disabled deployment")
	}
	m, err := c.render()
	if err != nil {
		return err
	}
	files, err := m.files()
	if err != nil {
		return err
	}
	chart, err := pinnedChart()
	if err != nil {
		return err
	}
	files[chartName] = chart
	for _, name := range sortedKeys(files) {
		if err = os.WriteFile(filepath.Join(dir, name), files[name], 0600); err != nil {
			return err
		}
	}
	work, err := privateDir("acs-helm-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, removePrivate(work)) }()
	args := []string{"helm", "template", "keda", filepath.Join(dir, chartName), "--namespace", c.KEDA.Namespace, "--include-crds", "--values", filepath.Join(dir, "acs-keda-values.yaml")}
	if c.KubeVersion != "" {
		args = append(args, "--kube-version", c.KubeVersion)
	}
	rendered, err := a.call(ctx, args, nil, true, false, map[string]string{"HELM_CACHE_HOME": filepath.Join(work, "cache"), "HELM_CONFIG_HOME": filepath.Join(work, "config"), "HELM_DATA_HOME": filepath.Join(work, "data")})
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "acs-keda-rendered.yaml"), rendered, 0600); err != nil {
		return err
	}
	return validateBundle(c, dir)
}

// Publish a new directory through an atomic symlink switch. Old Nix-store links
// are replaced without writing through them; user-owned directories are refused.
func (a *App) buildBundle(ctx context.Context, c DeploymentConfig, path string) (result error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink == 0 {
			return errors.New("bundle output already exists and is not a symlink; choose another --bundle path")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(parent, ".acs-bundle-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			result = errors.Join(result, removePrivate(dir))
		}
	}()
	if err = a.writeBundle(ctx, c, dir); err != nil {
		return err
	}
	link := filepath.Join(parent, filepath.Base(dir)+"-link")
	if err = os.Symlink(filepath.Base(dir), link); err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}()
	if err = os.Rename(link, path); err != nil {
		return err
	}
	published = true
	a.message("Bundle: %s", path)
	return nil
}

func (a *App) validatePlatform(ctx context.Context) (result error) {
	c, err := a.deployment()
	if err != nil {
		return err
	}
	dir, err := privateDir("acs-platform-check-")
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, removePrivate(dir)) }()
	if err = a.writeBundle(ctx, c, dir); err != nil {
		return err
	}
	for _, name := range snapshots {
		actual, err := os.ReadFile(filepath.Join(a.Root, "acs-k8s", name))
		if err != nil {
			return err
		}
		expected, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		var av, ev any
		if err = yaml.Unmarshal(actual, &av); err != nil {
			return err
		}
		if err = yaml.Unmarshal(expected, &ev); err != nil {
			return err
		}
		if !reflect.DeepEqual(av, ev) {
			return fmt.Errorf("stale snapshot %s; run acs bundle build and acs bundle snapshots", name)
		}
	}
	a.message("PASS: deployment configuration, KEDA CRD schemas, security/RBAC guards and YAML snapshots")
	return nil
}
func (a *App) validate(ctx context.Context, args []string) error {
	if err := exact(args, 1); err != nil {
		return err
	}
	switch args[0] {
	case "platform":
		return a.validatePlatform(ctx)
	case "images":
		if err := a.images(ctx, []string{"build"}); err != nil {
			return err
		}
		paths := []string{}
		for _, pool := range []string{"go", "node24", "docker", "nix"} {
			paths = append(paths, filepath.Join(a.Config.OutputDir, "runner-"+pool+"-image"))
		}
		return a.imageChecks(ctx, paths)
	case "cli":
		local := *a
		local.Root = filepath.Join(a.Root, "acs-k8s/cli")
		for _, args := range [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}, {"errcheck", "-blank", "-excludeonly", "./..."}} {
			if err := local.command(ctx, args...); err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.New("usage: acs validate platform|images|cli")
	}
}

func publicCABundle() ([]byte, error) {
	paths := []string{os.Getenv("SSL_CERT_FILE"), "/etc/ssl/certs/ca-certificates.crt", "/etc/ssl/certs/ca-bundle.crt"}
	for _, path := range paths {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err == nil {
			if !strings.Contains(string(data), "BEGIN CERTIFICATE") {
				return nil, errors.New("invalid public CA bundle")
			}
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, errors.New("public CA bundle missing; set SSL_CERT_FILE or run acs shell")
}
