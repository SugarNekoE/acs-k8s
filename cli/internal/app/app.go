package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var pools = []string{"nix", "go", "node24", "docker"}

const tag = "runner-13.1.0"
const help = `acs — ACS Forgejo runner operations

Usage: acs [global options] COMMAND

  config show                    Show non-secret effective settings
  shell                          Open the optional workstation tooling shell
  connect                        Check the SOPS-managed cluster connection
  registry login|digests         Interactive publishing login / image digests
  images build|publish [POOLS]    Build only / build and push (default: all pools)
  bundle build|inspect|snapshots  Build, inspect or refresh generated YAML
  namespaces                     Apply bundle namespaces
  secrets apply                  Apply SOPS-managed registration/scaler/pull Secrets
  deploy                         Explicitly install KEDA and runner resources
  status | watch | logs POD      Observe the configured cluster
  validate platform|images|cli    Validate configuration, images or CLI
  check images ARCHIVES           Check four image archives with Buildah
  check image POOL ARCHIVE        Check one image archive with Buildah
  workflow test POOL              Toolchain smoke / real Buildah build and push
  workflow kind-test POOL         Native builds against the isolated Kind registry
  workflow network               Credential-free API Service TCP preflight
  kind probe|test|network         Isolated Kind image / end-to-end / network tests
  kind diagnose                  Retain a diagnostic KEDA installation
  kind cleanup --yes             Delete only owned Kind test fixtures

Global options (may appear before or after commands):
  --repo PATH, --config FILE, --context NAME, --system SYSTEM,
  --output-dir PATH, --bundle PATH, --sops-file FILE, --deployment FILE,
  --pull-registry HOST

Defaults: acs-config.json, then environment overrides, then flags.
Paths use --repo, the --config directory, or the nearest ACS directory/current directory.
Image builds, validate cli and shell require their source files in --repo.
Cluster commands require an explicit context and use only the SOPS kubeconfig.
Kind commands use only kind-acs-test and never decrypt ACS credentials.
Builds never deploy. No sudo. Secrets are never printed.
`

type Config struct {
	Registry          string `json:"registry"`
	PullRegistry      string `json:"pull_registry,omitempty"`
	RegistryNamespace string `json:"registry_namespace"`
	System            string `json:"image_system"`
	Context           string `json:"context"`
	SOPSFile          string `json:"sops_file"`
	RunnerNamespace   string `json:"runner_namespace"`
	KEDANamespace     string `json:"keda_namespace"`
	OutputDir         string `json:"output_dir,omitempty"`
	Bundle            string `json:"bundle,omitempty"`
	DeploymentFile    string `json:"deployment_file,omitempty"`
}

type Invocation struct {
	Args               []string
	Dir                string
	Env                map[string]string
	Input              []byte
	Capture, Sensitive bool
}
type Runner interface {
	Run(context.Context, Invocation) ([]byte, error)
}
type OSRunner struct {
	In       io.Reader
	Out, Err io.Writer
}

func (r OSRunner) Run(ctx context.Context, in Invocation) ([]byte, error) {
	c := exec.CommandContext(ctx, in.Args[0], in.Args[1:]...)
	c.Dir = in.Dir
	c.Env = os.Environ()
	for k, v := range in.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.WaitDelay = 2 * time.Second
	c.Stdin = r.In
	if in.Input != nil {
		c.Stdin = bytes.NewReader(in.Input)
	}
	var out, stderr bytes.Buffer
	if in.Capture || in.Sensitive {
		c.Stdout = &out
	} else {
		c.Stdout = r.Out
	}
	if in.Sensitive {
		c.Stderr = &stderr
	} else {
		c.Stderr = r.Err
	}
	err := c.Run()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", filepath.Base(in.Args[0]), err)
	}
	return out.Bytes(), nil
}

type App struct {
	Root      string
	Config    Config
	Exec      Runner
	Out       io.Writer
	outputErr error
}

func (a *App) call(ctx context.Context, args []string, input []byte, capture, sensitive bool, env map[string]string) ([]byte, error) {
	b, err := a.Exec.Run(ctx, Invocation{Args: args, Dir: a.Root, Input: input, Capture: capture, Sensitive: sensitive, Env: env})
	if err != nil && sensitive {
		return nil, fmt.Errorf("%s failed; credential-bearing output suppressed", filepath.Base(args[0]))
	}
	return b, err
}
func (a *App) command(ctx context.Context, args ...string) error {
	_, e := a.call(ctx, args, nil, false, false, nil)
	return e
}
func (a *App) capture(ctx context.Context, args ...string) ([]byte, error) {
	return a.call(ctx, args, nil, true, false, nil)
}
func (a *App) message(f string, args ...any) {
	_, err := fmt.Fprintf(a.Out, f+"\n", args...)
	a.outputErr = errors.Join(a.outputErr, err)
}
func exact(args []string, n int) error {
	if len(args) != n {
		return errors.New("unexpected or missing arguments; use acs --help")
	}
	return nil
}

func Run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) error {
	flags, command, err := parseArgs(args)
	if err != nil {
		return err
	}
	if len(command) == 0 || command[0] == "help" {
		_, err = io.WriteString(out, help)
		return err
	}
	if command[0] == "workflow" || command[0] == "check" {
		a := &App{Config: Config{System: flags["system"]}, Exec: OSRunner{in, out, stderr}, Out: out}
		err := a.Dispatch(ctx, command)
		return errors.Join(err, a.outputErr)
	}
	base := flags["repo"]
	if base == "" && flags["config"] != "" {
		configPath, err := filepath.Abs(flags["config"])
		if err != nil {
			return err
		}
		base = filepath.Dir(configPath)
	}
	root, err := findRoot(base)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(root, flags)
	if err != nil {
		return err
	}
	a := &App{Root: root, Config: cfg, Exec: OSRunner{in, out, stderr}, Out: out}
	err = a.Dispatch(ctx, command)
	return errors.Join(err, a.outputErr)
}
func parseArgs(args []string) (map[string]string, []string, error) {
	flags := map[string]string{}
	var command []string
	allowed := map[string]bool{"repo": true, "config": true, "context": true, "system": true, "output-dir": true, "bundle": true, "sops-file": true, "deployment": true, "pull-registry": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			return flags, []string{"help"}, nil
		}
		if strings.HasPrefix(arg, "--") && arg != "--yes" {
			key, value, has := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			if !allowed[key] {
				return nil, nil, fmt.Errorf("unknown option --%s", key)
			}
			if !has {
				i++
				if i >= len(args) || strings.HasPrefix(args[i], "--") {
					return nil, nil, fmt.Errorf("missing value for --%s", key)
				}
				value = args[i]
			}
			if value == "" {
				return nil, nil, fmt.Errorf("empty value for --%s", key)
			}
			flags[key] = value
		} else {
			command = append(command, arg)
		}
	}
	return flags, command, nil
}
func findRoot(explicit string) (string, error) {
	p := explicit
	if p == "" {
		var err error
		p, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("ACS working directory is not a directory: %s", p)
	}
	if explicit != "" {
		return p, nil
	}
	start := p
	for {
		for _, marker := range []string{"acs-config.json", "acs-deployment.json", "cli/go.mod"} {
			if info, err := os.Stat(filepath.Join(p, marker)); err == nil {
				if info.Mode().IsRegular() {
					return p, nil
				}
			} else if !os.IsNotExist(err) {
				return "", err
			}
		}
		if filepath.Dir(p) == p {
			break
		}
		p = filepath.Dir(p)
	}
	return start, nil
}

func (a *App) requireSourceFiles(command string, paths ...string) error {
	for _, path := range paths {
		info, err := os.Stat(filepath.Join(a.Root, path))
		if err != nil {
			return fmt.Errorf("%s requires %s in the ACS checkout; pass --repo PATH: %w", command, path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s requires a regular %s file in the ACS checkout; pass --repo PATH", command, path)
		}
	}
	return nil
}
func loadConfig(root string, flags map[string]string) (Config, error) {
	c := Config{System: "x86_64-linux", RunnerNamespace: "forgejo-runners", KEDANamespace: "keda"}
	path := flags["config"]
	explicit := path != ""
	if path == "" && root != "" {
		path = filepath.Join(root, "acs-config.json")
	}
	if path != "" {
		b, e := os.ReadFile(path)
		if e != nil && (explicit || !os.IsNotExist(e)) {
			return c, e
		}
		if e == nil {
			d := json.NewDecoder(bytes.NewReader(b))
			d.DisallowUnknownFields()
			if e = d.Decode(&c); e != nil {
				return c, fmt.Errorf("invalid non-secret config: %w", e)
			}
		}
	}
	overrides := map[string]*string{"ACR": &c.Registry, "ACS_PULL_REGISTRY": &c.PullRegistry, "ACR_NAMESPACE": &c.RegistryNamespace, "IMAGE_SYSTEM": &c.System, "CTX": &c.Context, "ACS_SOPS_FILE": &c.SOPSFile, "ACS_DEPLOYMENT_FILE": &c.DeploymentFile, "ACS_OUTPUT_DIR": &c.OutputDir, "ACS_BUNDLE": &c.Bundle, "ACS_RUNNER_NAMESPACE": &c.RunnerNamespace, "ACS_KEDA_NAMESPACE": &c.KEDANamespace}
	for key, p := range overrides {
		if v := os.Getenv(key); v != "" {
			*p = v
		}
	}
	for key, p := range map[string]*string{"context": &c.Context, "system": &c.System, "output-dir": &c.OutputDir, "bundle": &c.Bundle, "sops-file": &c.SOPSFile, "deployment": &c.DeploymentFile, "pull-registry": &c.PullRegistry} {
		if v := flags[key]; v != "" {
			*p = v
		}
	}
	if c.PullRegistry != "" && !validRegistryHost(c.PullRegistry) {
		return c, errors.New("pull_registry must be a registry hostname with optional port, without a scheme or path")
	}
	if c.OutputDir == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			h, e := os.UserHomeDir()
			if e != nil {
				return c, e
			}
			base = filepath.Join(h, ".local/state")
		}
		c.OutputDir = filepath.Join(base, "acs-forgejo")
	}
	if !filepath.IsAbs(c.OutputDir) {
		c.OutputDir = filepath.Join(root, c.OutputDir)
	}
	if c.Bundle == "" {
		c.Bundle = filepath.Join(c.OutputDir, "bundle")
	}
	if !filepath.IsAbs(c.Bundle) {
		c.Bundle = filepath.Join(root, c.Bundle)
	}
	if c.SOPSFile != "" && !filepath.IsAbs(c.SOPSFile) {
		c.SOPSFile = filepath.Join(root, c.SOPSFile)
	}
	if c.DeploymentFile == "" {
		c.DeploymentFile = "acs-deployment.json"
	}
	if !filepath.IsAbs(c.DeploymentFile) {
		c.DeploymentFile = filepath.Join(root, c.DeploymentFile)
	}
	return c, nil
}
func validSystem(s string) error {
	if s != "x86_64-linux" && s != "aarch64-linux" {
		return fmt.Errorf("unsupported image system %q", s)
	}
	return nil
}
func (a *App) registry() error {
	if a.Config.Registry == "" || a.Config.RegistryNamespace == "" {
		return errors.New("configure registry and registry_namespace")
	}
	if !validRegistryHost(a.Config.Registry) || strings.ContainsAny(a.Config.RegistryNamespace, "/ \t\r\n") {
		return errors.New("invalid registry hostname or namespace")
	}
	if !validRegistryHost(a.Config.pullRegistry()) {
		return errors.New("invalid pull_registry hostname")
	}
	return nil
}

func (c Config) pullRegistry() string {
	if c.PullRegistry != "" {
		return c.PullRegistry
	}
	return c.Registry
}

func validRegistryHost(host string) bool {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, "/\\ @\t\r\n?#") {
		return false
	}
	u, err := url.Parse("https://" + host)
	return err == nil && u.Host == host && u.Hostname() != ""
}

func (a *App) Dispatch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("command required")
	}
	switch args[0] {
	case "config":
		if len(args) != 2 || args[1] != "show" {
			return errors.New("usage: acs config show")
		}
		return json.NewEncoder(a.Out).Encode(a.Config)
	case "shell":
		if e := exact(args, 1); e != nil {
			return e
		}
		if err := a.requireSourceFiles("shell", "flake.nix", "cli/go.mod"); err != nil {
			return err
		}
		return a.command(ctx, "nix", "develop", a.Root, "--command", "bash")
	case "images":
		return a.images(ctx, args[1:])
	case "registry":
		return a.registryCommand(ctx, args[1:])
	case "bundle":
		return a.bundleCommand(ctx, args[1:])
	case "validate":
		return a.validate(ctx, args[1:])
	case "check":
		if len(args) == 4 && args[1] == "image" {
			if _, err := selectPools(args[2:3]); err != nil {
				return err
			}
			return a.imageCheck(ctx, args[3], args[2])
		}
		if len(args) != 6 || args[1] != "images" {
			return errors.New("usage: acs check images GO NODE24 DOCKER NIX (archive paths)")
		}
		return a.imageChecks(ctx, args[2:])
	case "workflow":
		return a.workflow(ctx, args[1:])
	case "kind":
		return a.kind(ctx, args[1:])
	case "connect", "namespaces", "deploy", "status", "watch":
		if e := exact(args, 1); e != nil {
			return e
		}
		return a.withCluster(ctx, func(s *Session) error { return a.clusterAction(ctx, s, args) })
	case "logs":
		if e := exact(args, 2); e != nil {
			return e
		}
		if strings.HasPrefix(args[1], "-") {
			return errors.New("invalid pod name")
		}
		return a.withCluster(ctx, func(s *Session) error { return a.clusterAction(ctx, s, args) })
	case "secrets":
		if len(args) != 2 || args[1] != "apply" {
			return errors.New("usage: acs secrets apply")
		}
		return a.withCluster(ctx, func(s *Session) error { return a.applySecrets(ctx, s) })
	default:
		return fmt.Errorf("unknown command %q; use acs --help", args[0])
	}
}
