package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var snapshots = []string{"acs-namespaces.yaml", "acs-forgejo-runners.yaml", "acs-external-secrets.yaml", "acs-keda-values.yaml"}

func selectPools(args []string) ([]string, error) {
	if len(args) == 0 {
		return pools, nil
	}
	seen := map[string]bool{}
	for _, p := range args {
		ok := false
		for _, known := range pools {
			if p == known {
				ok = true
			}
		}
		if !ok || seen[p] {
			return nil, fmt.Errorf("invalid or duplicate pool %q", p)
		}
		seen[p] = true
	}
	return args, nil
}
func (a *App) images(ctx context.Context, args []string) (result error) {
	if len(args) == 0 || (args[0] != "build" && args[0] != "publish") {
		return errors.New("usage: acs images build|publish [nix go node24 docker]")
	}
	selected, e := selectPools(args[1:])
	if e != nil {
		return e
	}
	if e = validSystem(a.Config.System); e != nil {
		return e
	}
	publish := args[0] == "publish"
	if publish {
		if e = a.registry(); e != nil {
			return e
		}
	}
	if e = os.MkdirAll(a.Config.OutputDir, 0o755); e != nil {
		return e
	}
	work, e := privateDir("acs-images-")
	if e != nil {
		return e
	}
	defer func() { result = errors.Join(result, removePrivate(work)) }()
	for _, p := range selected {
		path := filepath.Join(a.Config.OutputDir, "runner-"+p+"-image")
		if e = a.buildImage(ctx, p, path); e != nil {
			return e
		}
		if publish {
			if e = a.command(ctx, "skopeo", "--tmpdir", work, "copy", "docker-archive:"+path, "docker://"+a.Config.Registry+"/"+a.Config.RegistryNamespace+"/forgejo-runner-"+p+":"+tag); e != nil {
				return e
			}
		}
		a.message("%s: %s", p, path)
	}
	return nil
}
func (a *App) registryCommand(ctx context.Context, args []string) error {
	if e := exact(args, 1); e != nil {
		return e
	}
	if e := a.registry(); e != nil {
		return e
	}
	switch args[0] {
	case "login":
		return a.command(ctx, "skopeo", "login", a.Config.Registry)
	case "digests":
		for _, p := range pools {
			b, e := a.capture(ctx, "skopeo", "inspect", "docker://"+a.Config.Registry+"/"+a.Config.RegistryNamespace+"/forgejo-runner-"+p+":"+tag, "--format", "{{.Digest}}")
			if e != nil {
				return e
			}
			a.message("%s: %s", p, strings.TrimSpace(string(b)))
		}
		return nil
	default:
		return errors.New("usage: acs registry login|digests")
	}
}
func (a *App) requireBundle() error {
	if _, e := os.Stat(filepath.Join(a.Config.Bundle, "acs-forgejo-runners.yaml")); e != nil {
		return errors.New("bundle missing; run acs bundle build first")
	}
	return nil
}
func (a *App) bundleCommand(ctx context.Context, args []string) error {
	if e := exact(args, 1); e != nil {
		return e
	}
	if args[0] == "build" {
		c, err := a.deployment()
		if err != nil {
			return err
		}
		return a.buildBundle(ctx, c, a.Config.Bundle)
	}
	if e := a.requireBundle(); e != nil {
		return e
	}
	switch args[0] {
	case "inspect":
		return a.command(ctx, "less", filepath.Join(a.Config.Bundle, "acs-forgejo-runners.yaml"))
	case "snapshots":
		// Pre-read all files before changing any snapshot; replace read-only store copies.
		data := map[string][]byte{}
		for _, name := range snapshots {
			b, e := os.ReadFile(filepath.Join(a.Config.Bundle, name))
			if e != nil {
				return e
			}
			data[name] = b
		}
		for _, name := range snapshots {
			if e := atomicWrite(filepath.Join(a.Root, "acs-k8s", name), data[name], 0o644); e != nil {
				return e
			}
		}
		a.message("Refreshed four YAML snapshots.")
		return nil
	default:
		return errors.New("usage: acs bundle build|inspect|snapshots")
	}
}
func atomicWrite(path string, b []byte, mode os.FileMode) (result error) {
	f, e := os.CreateTemp(filepath.Dir(path), ".acs-write-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer func() {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}()
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(b)
	}
	closeErr := f.Close()
	if err := errors.Join(e, closeErr); err != nil {
		return err
	}
	return os.Rename(name, path)
}
func (a *App) clusterAction(ctx context.Context, s *Session, args []string) error {
	k := func(v ...string) error { _, e := a.kubectl(ctx, s, false, false, nil, v...); return e }
	switch args[0] {
	case "connect":
		if e := k("config", "get-contexts"); e != nil {
			return e
		}
		if e := k("cluster-info"); e != nil {
			return e
		}
		if e := k("version"); e != nil {
			return e
		}
		_, e := a.helm(ctx, s, false, "list", "--all-namespaces")
		return e
	case "namespaces":
		if e := a.requireBundle(); e != nil {
			return e
		}
		return k("apply", "-f", filepath.Join(a.Config.Bundle, "acs-namespaces.yaml"))
	case "deploy":
		return a.deploy(ctx, s)
	case "status":
		return a.clusterStatus(ctx, s)
	case "watch":
		return k("-n", a.Config.RunnerNamespace, "get", "jobs,pods", "-w")
	case "logs":
		for _, c := range []string{"register", "runner"} {
			if e := k("-n", a.Config.RunnerNamespace, "logs", args[1], "-c", c); e != nil {
				return e
			}
		}
		if e := k("-n", a.Config.RunnerNamespace, "describe", "pod", args[1]); e != nil {
			return e
		}
		return k("-n", a.Config.KEDANamespace, "logs", "deployment/keda-operator", "--tail=100")
	}
	return errors.New("unknown cluster action")
}

type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}
type DeployPlan struct {
	Version          int         `json:"version"`
	RunnerNamespace  string      `json:"runnerNamespace"`
	KEDANamespace    string      `json:"kedaNamespace"`
	RequiredSecrets  []SecretRef `json:"requiredSecrets"`
	ImagePullSecrets []string    `json:"imagePullSecrets"`
	ExternalSecrets  []string    `json:"externalSecrets"`
	ScaledJobs       []string    `json:"scaledJobs"`
}

func (a *App) deploy(ctx context.Context, s *Session) error {
	if e := a.requireBundle(); e != nil {
		return e
	}
	b, e := os.ReadFile(filepath.Join(a.Config.Bundle, "acs-deploy-plan.json"))
	if e != nil {
		return errors.New("bundle lacks CLI deployment plan; rebuild it with acs bundle build")
	}
	var p DeployPlan
	if json.Unmarshal(b, &p) != nil || p.Version != 1 || len(p.RequiredSecrets) == 0 || len(p.ScaledJobs) == 0 {
		return errors.New("invalid deployment plan")
	}
	if p.RunnerNamespace != a.Config.RunnerNamespace || p.KEDANamespace != a.Config.KEDANamespace {
		return errors.New("bundle namespaces do not match the CLI configuration")
	}
	k := func(v ...string) error { _, e := a.kubectl(ctx, s, false, false, nil, v...); return e }
	if e = k("cluster-info"); e != nil {
		return e
	}
	if e = k("apply", "-f", filepath.Join(a.Config.Bundle, "acs-namespaces.yaml")); e != nil {
		return e
	}
	if len(p.ExternalSecrets) > 0 {
		if e = k("get", "crd", "externalsecrets.external-secrets.io"); e != nil {
			return e
		}
		if e = k("apply", "-f", filepath.Join(a.Config.Bundle, "acs-external-secrets.yaml")); e != nil {
			return e
		}
		for _, name := range p.ExternalSecrets {
			if e = k("-n", p.RunnerNamespace, "wait", "--for=condition=Ready", "externalsecret/"+name, "--timeout=180s"); e != nil {
				return e
			}
		}
	}
	for _, ref := range p.RequiredSecrets {
		template := fmt.Sprintf("{{if index .data %q}}present{{end}}", ref.Key)
		value, e := a.kubectl(ctx, s, true, true, nil, "-n", p.RunnerNamespace, "get", "secret", ref.Name, "-o", "go-template="+template)
		if e != nil {
			return e
		}
		if string(value) != "present" {
			return fmt.Errorf("missing nonempty Secret key %s/%s", ref.Name, ref.Key)
		}
	}
	for _, name := range p.ImagePullSecrets {
		value, e := a.kubectl(ctx, s, true, true, nil, "-n", p.RunnerNamespace, "get", "secret", name, "-o", "jsonpath={.type}")
		if e != nil {
			return e
		}
		if string(value) != "kubernetes.io/dockerconfigjson" {
			return fmt.Errorf("image-pull Secret %s has incorrect type", name)
		}
	}
	releases, e := a.helm(ctx, s, true, "list", "--namespace", p.KEDANamespace, "--all", "--filter", "^keda$", "--output", "json")
	if e != nil {
		return e
	}
	var rows []any
	if json.Unmarshal(releases, &rows) != nil || rows == nil {
		return errors.New("invalid Helm release list")
	}
	if len(rows) > 0 {
		values, e := a.helm(ctx, s, true, "get", "values", "keda", "--namespace", p.KEDANamespace, "--output", "json")
		if e != nil {
			return e
		}
		var v struct {
			Managed   bool   `json:"acsForgejoManaged"`
			Namespace string `json:"watchNamespace"`
		}
		if json.Unmarshal(values, &v) != nil || !v.Managed || v.Namespace != p.RunnerNamespace {
			return errors.New("refusing to adopt an unrelated KEDA release")
		}
	}
	_, e = a.helm(ctx, s, false, "upgrade", "--install", "keda", filepath.Join(a.Config.Bundle, "acs-keda-2.20.2.tgz"), "--namespace", p.KEDANamespace, "--values", filepath.Join(a.Config.Bundle, "acs-keda-values.yaml"), "--wait", "--atomic", "--timeout", "5m")
	if e != nil {
		return e
	}
	if e = k("wait", "--for=condition=Established", "crd/scaledjobs.keda.sh", "crd/triggerauthentications.keda.sh", "--timeout=120s"); e != nil {
		return e
	}
	manifests := filepath.Join(a.Config.Bundle, "acs-forgejo-runners.yaml")
	if e = k("apply", "--dry-run=server", "-f", manifests); e != nil {
		return e
	}
	if e = k("apply", "-f", manifests); e != nil {
		return e
	}
	for _, name := range p.ScaledJobs {
		if e = k("-n", p.RunnerNamespace, "wait", "--for=condition=Ready", "scaledjob/"+name, "--timeout=120s"); e != nil {
			failure := fmt.Errorf("scaledjob %s/%s did not become Ready: %w", p.RunnerNamespace, name, e)
			if ctx.Err() != nil {
				return failure
			}
			a.message("Readiness check failed; collecting ScaledJob conditions, events and KEDA operator logs.")
			return errors.Join(failure, a.clusterStatus(ctx, s))
		}
	}
	return nil
}
