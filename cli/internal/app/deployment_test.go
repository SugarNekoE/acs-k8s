package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func fixtureDeployment(t *testing.T) DeploymentConfig {
	t.Helper()
	c, err := loadDeployment("testdata/nix-reference/deployment.json")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDeploymentPullRegistry(t *testing.T) {
	a, r := testApp(t)
	a.Config.PullRegistry = "registry-vpc.invalid"
	a.Config.DeploymentFile = filepath.Join(a.Root, "acs-deployment.json")
	c := fixtureDeployment(t)
	images := map[string]string{
		"go":     a.Config.Registry + "/ci/runner-go@sha256:" + strings.Repeat("a", 64),
		"nix":    a.Config.Registry + "/ci/runner-nix:test",
		"docker": a.Config.Registry + ".external.invalid/ci/runner-docker:test",
		"node24": a.Config.PullRegistry + "/ci/runner-node24:test",
	}
	for name, image := range images {
		runner := c.Runners[name]
		runner.Image = image
		c.Runners[name] = runner
	}
	before := encoded(t, c)
	mustWrite(t, a.Config.DeploymentFile, string(before))
	resolved, err := a.deployment()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"go":     a.Config.PullRegistry + "/ci/runner-go@sha256:" + strings.Repeat("a", 64),
		"nix":    a.Config.PullRegistry + "/ci/runner-nix:test",
		"docker": images["docker"],
		"node24": images["node24"],
	}
	m, err := resolved.render()
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range m.Resources {
		if resource["kind"] != "ScaledJob" {
			continue
		}
		name := strings.TrimPrefix(str(at(resource, "metadata", "name")), "acs-runner-")
		pod := obj(at(resource, "spec", "jobTargetRef", "template", "spec"))
		for _, key := range []string{"containers", "initContainers"} {
			if got := at(obj(arr(pod[key])[0]), "image"); got != want[name] {
				t.Fatalf("%s %s image = %v, want %s", name, key, got, want[name])
			}
		}
	}
	for name, runner := range resolved.kindConfig().Runners {
		if runner.Image != "localhost:30050/forgejo-runner-"+name+":"+tag {
			t.Fatal("VPC override escaped into Kind registry")
		}
	}
	after, err := os.ReadFile(a.Config.DeploymentFile)
	if err != nil || !bytes.Equal(before, after) || len(r.calls) != 0 {
		t.Fatal("resolving pull registry changed source JSON or invoked external tools")
	}
	a.Config.PullRegistry = ""
	resolved, err = a.deployment()
	if err != nil {
		t.Fatal(err)
	}
	for name, runner := range resolved.Runners {
		if runner.Image != images[name] {
			t.Fatal("unset pull_registry changed an image")
		}
	}
}
func TestGoManifestsMatchPreviousNixBundle(t *testing.T) {
	c := fixtureDeployment(t)
	// The historical Nix snapshots retained finished Jobs for five minutes.
	// Keep that explicit input here; current cleanup defaults are tested below.
	for name, runner := range c.Runners {
		runner.TTLSecondsAfterFinished = 300
		c.Runners[name] = runner
	}
	m, err := c.render()
	if err != nil {
		t.Fatal(err)
	}
	files, err := m.files()
	if err != nil {
		t.Fatal(err)
	}
	for name, actual := range files {
		t.Run(name, func(t *testing.T) {
			expected, err := os.ReadFile(filepath.Join("testdata/nix-reference", name))
			if err != nil {
				t.Fatal(err)
			}
			var av, ev any
			if err = yaml.Unmarshal(actual, &av); err != nil {
				t.Fatal(err)
			}
			if err = yaml.Unmarshal(expected, &ev); err != nil {
				t.Fatal(err)
			}
			if name == "acs-forgejo-runners.yaml" {
				// Preserve the historical fixture, allowing only the intentional
				// scheduler and empty-poll behavior changes in today's manifests.
				for _, item := range arr(at(obj(ev), "items")) {
					resource := obj(item)
					if resource["kind"] != "ScaledJob" {
						continue
					}
					spec := obj(resource["spec"])
					spec["scalingStrategy"] = Obj{"strategy": "default"}
					pod := obj(at(spec, "jobTargetRef", "template", "spec"))
					main := obj(arr(pod["containers"])[0])
					main["args"] = append(arr(main["args"]), "--wait")
					init := obj(arr(pod["initContainers"])[0])
					init["args"] = []any{registrationScript}
					init["terminationMessagePolicy"] = "FallbackToLogsOnError"
				}
			}
			if !reflect.DeepEqual(av, ev) {
				t.Fatalf("Go output differs from previous Nix bundle: %s", name)
			}
		})
	}
}

func TestRunnerSchedulingPolicy(t *testing.T) {
	c := fixtureDeployment(t)
	for _, config := range []DeploymentConfig{c, c.kindConfig()} {
		m, err := config.render()
		if err != nil {
			t.Fatal(err)
		}
		objects, err := canonicalObjects(m.Resources)
		if err != nil {
			t.Fatal(err)
		}
		checked := 0
		for _, resource := range objects {
			if resource["kind"] != "ScaledJob" {
				continue
			}
			checked++
			spec := obj(resource["spec"])
			if at(spec, "scalingStrategy", "strategy") != "default" {
				t.Fatal("running tasks would create surplus Jobs with pending-only accounting")
			}
			if at(spec, "rollout", "strategy") != "gradual" || spec["minReplicaCount"] != float64(0) {
				t.Fatal("scheduler must preserve active Jobs and scale to zero")
			}
			name := strings.TrimPrefix(str(at(resource, "metadata", "name")), "acs-runner-")
			if spec["maxReplicaCount"] != float64(config.Runners[name].MaxReplicas) {
				t.Fatal("pool limit changed")
			}
			job := obj(spec["jobTargetRef"])
			pod := obj(at(job, "template", "spec"))
			main := obj(arr(pod["containers"])[0])
			if !reflect.DeepEqual(main["args"], []any{"--config", "/etc/acs-runner/config.yaml", "one-job", "--wait"}) {
				t.Fatal("empty task poll would exit instead of waiting")
			}
			if job["activeDeadlineSeconds"] != float64(config.Runners[name].ActiveDeadlineSeconds) || job["backoffLimit"] != float64(0) {
				t.Fatal("waiting must remain bounded without replaying the workflow")
			}
		}
		if checked != len(config.Runners) {
			t.Fatal("missing pool scheduling policy")
		}
	}
}

func TestRunnerCleanupPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		wantTTL        int
	}{
		{name: "default immediate", settings: `{}`, wantTTL: 0},
		{name: "explicit immediate", settings: `{"ttlSecondsAfterFinished":0}`, wantTTL: 0},
		{name: "retain for debugging", settings: `{"ttlSecondsAfterFinished":300}`, wantTTL: 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixtureDeployment(t)
			var policy RunnerConfig
			if err := json.Unmarshal([]byte(tc.settings), &policy); err != nil {
				t.Fatal(err)
			}
			for name, runner := range c.Runners {
				runner.TTLSecondsAfterFinished = policy.TTLSecondsAfterFinished
				c.Runners[name] = runner
			}
			m, err := c.render()
			if err != nil {
				t.Fatal(err)
			}
			checked := 0
			for _, resource := range m.Resources {
				if resource["kind"] != "ScaledJob" {
					continue
				}
				checked++
				job := obj(at(resource, "spec", "jobTargetRef"))
				if job["ttlSecondsAfterFinished"] != tc.wantTTL || job["backoffLimit"] != 0 {
					t.Fatalf("incorrect terminal Job cleanup: %+v", job)
				}
				pod := obj(at(job, "template", "spec"))
				if pod["restartPolicy"] != "Never" || pod["automountServiceAccountToken"] != false {
					t.Fatal("cleanup must not restart workflows or grant cluster credentials")
				}
				init := obj(arr(pod["initContainers"])[0])
				args, ok := init["args"].([]string)
				if !ok || len(args) != 1 || !strings.Contains(args[0], "--no-interactive --ephemeral") {
					t.Fatal("Forgejo cleanup requires ephemeral registration")
				}
			}
			if checked != len(c.Runners) {
				t.Fatal("missing runner cleanup policy")
			}
		})
	}
}

func TestDeploymentValidationParity(t *testing.T) {
	cases := map[string]func(*DeploymentConfig){
		"empty pools":     func(c *DeploymentConfig) { c.Runners = map[string]RunnerConfig{} },
		"same namespace":  func(c *DeploymentConfig) { c.KEDA.Namespace = c.Namespace },
		"http":            func(c *DeploymentConfig) { c.URL = "http://forgejo.invalid" },
		"url credentials": func(c *DeploymentConfig) { c.URL = "https://user:password@forgejo.invalid" },
		"empty labels":    func(c *DeploymentConfig) { r := c.Runners["go"]; r.Labels = nil; c.Runners["go"] = r },
		"duplicate labels": func(c *DeploymentConfig) {
			r := c.Runners["go"]
			r.Labels = []string{"same", "same"}
			c.Runners["go"] = r
		},
		"shared labels": func(c *DeploymentConfig) {
			r := c.Runners["go"]
			r.Labels = c.Runners["nix"].Labels
			c.Runners["go"] = r
		},
		"host backend label": func(c *DeploymentConfig) { r := c.Runners["go"]; r.Labels = []string{"go:host"}; c.Runners["go"] = r },
		"bad pool name":      func(c *DeploymentConfig) { c.Runners["invalid_pool"] = c.Runners["go"] },
		"bad scope": func(c *DeploymentConfig) {
			r := c.Runners["go"]
			r.Scope = &Scope{Type: "repository"}
			c.Runners["go"] = r
		},
		"shared secret": func(c *DeploymentConfig) {
			r := c.Runners["go"]
			r.ScalerSecret = c.Runners["nix"].RegistrationSecret
			c.Runners["go"] = r
		},
		"cross pool token": func(c *DeploymentConfig) {
			r := c.Runners["go"]
			r.RegistrationSecret = c.Runners["nix"].ScalerSecret
			c.Runners["go"] = r
		},
		"builder UID": func(c *DeploymentConfig) { r := c.Runners["docker"]; r.RunAsUser = 1000; c.Runners["docker"] = r },
		"timeout": func(c *DeploymentConfig) {
			r := c.Runners["go"]
			r.ActiveDeadlineSeconds = r.JobTimeoutSeconds + 120
			c.Runners["go"] = r
		},
		"zero replicas":  func(c *DeploymentConfig) { r := c.Runners["go"]; r.MaxReplicas = 0; c.Runners["go"] = r },
		"negative TTL":   func(c *DeploymentConfig) { r := c.Runners["go"]; r.TTLSecondsAfterFinished = -1; c.Runners["go"] = r },
		"long namespace": func(c *DeploymentConfig) { c.Namespace = strings.Repeat("a", 64) },
		"long secret": func(c *DeploymentConfig) {
			r := c.Runners["go"]
			r.ScalerSecret = &SecretRef{strings.Repeat("a", 254), "token"}
			c.Runners["go"] = r
		},
		"bad external name": func(c *DeploymentConfig) {
			c.ExternalSecrets = map[string]ExternalSecret{"invalid_name": {StoreRef: StoreRef{"store", "ClusterSecretStore"}, Type: "Opaque", Data: []ExternalData{}}}
		},
		"gpu QoS": func(c *DeploymentConfig) { c.Aliyun.ComputeClass = "gpu-hpn"; c.Aliyun.ComputeQos = "best-effort" },
		"bad quantity": func(c *DeploymentConfig) {
			r := c.Runners["go"]
			r.Resources.Limits["cpu"] = "invalid"
			c.Runners["go"] = r
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := fixtureDeployment(t)
			change(&c)
			if err := c.validate(); err == nil {
				t.Fatal("invalid deployment accepted")
			}
		})
	}
	c := fixtureDeployment(t)
	c.Enable = false
	c.Runners = nil
	m, err := c.render()
	if err != nil || len(m.Resources) != 0 || len(m.Namespaces) != 0 {
		t.Fatalf("disabled: %+v %v", m, err)
	}
	c = fixtureDeployment(t)
	r := c.Runners["go"]
	r.ScalerSecret = &SecretRef{"valid.dotted.secret", "token"}
	c.Runners["go"] = r
	if err = c.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestScopesExternalSecretsAndCustomPool(t *testing.T) {
	for _, scope := range []Scope{{Type: "global"}, {Type: "user"}, {Type: "organization", Owner: "ci-org"}, {Type: "repository", Owner: "ci-org", Repo: "ci-repo"}} {
		c := fixtureDeployment(t)
		for name, r := range c.Runners {
			r.Scope = &scope
			c.Runners[name] = r
		}
		m, err := c.render()
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range m.Resources {
			if o["kind"] != "ScaledJob" {
				continue
			}
			trigger := obj(arr(at(o, "spec", "triggers"))[0])
			metadata := obj(trigger["metadata"])
			if (metadata["global"] == "true") != (scope.Type == "global") {
				t.Fatal("wrong global scope")
			}
			if (metadata["org"] != nil) != (scope.Type == "organization") {
				t.Fatal("wrong org scope")
			}
			if (metadata["owner"] != nil) != (scope.Type == "repository") {
				t.Fatal("wrong repository scope")
			}
		}
	}
	c := fixtureDeployment(t)
	r := c.Runners["go"]
	r.Labels = []string{"acs-custom"}
	r.Architecture = "arm64"
	c.Runners["custom"] = r
	c.Aliyun.ComputeQos = "best-effort"
	c.ExternalSecrets = map[string]ExternalSecret{"acr.pull": {StoreRef: StoreRef{"store", "ClusterSecretStore"}, Type: "kubernetes.io/dockerconfigjson", Data: []ExternalData{{".dockerconfigjson", RemoteRef{Key: "ci/acr"}}}}}
	m, err := c.render()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Plan.ScaledJobs, "acs-runner-custom") {
		t.Fatal("custom pool missing")
	}
	b := encoded(t, m.ExternalSecrets)
	if bytes.Contains(b, []byte(`"property"`)) || !bytes.Contains(b, []byte(`"creationPolicy":"Owner"`)) {
		t.Fatal("incorrect ESO output")
	}
	for _, o := range m.Resources {
		if o["kind"] == "ScaledJob" && at(o, "metadata", "name") == "acs-runner-custom" {
			if at(o, "spec", "jobTargetRef", "template", "spec", "nodeSelector", "kubernetes.io/arch") != "arm64" {
				t.Fatal("architecture not rendered")
			}
		}
	}
}

func TestNativeBundleAndKindSchemaValidation(t *testing.T) {
	c := fixtureDeployment(t)
	a, _ := testApp(t)
	a.Exec = &fakeRunner{run: func(in Invocation) ([]byte, error) {
		if in.Args[0] != "helm" {
			t.Fatalf("bundle/check invoked an unexpected tool: %s", in.Args[0])
		}
		return (OSRunner{Out: io.Discard, Err: io.Discard}).Run(t.Context(), in)
	}}
	configs := []DeploymentConfig{c, c.kindConfig()}
	for _, scope := range []Scope{{Type: "organization", Owner: "ci"}, {Type: "repository", Owner: "ci", Repo: "test"}} {
		variant := fixtureDeployment(t)
		for name, runner := range variant.Runners {
			runner.Scope = &scope
			runner.Architecture = "arm64"
			variant.Runners[name] = runner
		}
		configs = append(configs, variant)
	}
	for _, config := range configs {
		dir := t.TempDir()
		if err := a.writeBundle(t.Context(), config, dir); err != nil {
			t.Fatal(err)
		}
		if err := validateBundle(config, dir); err != nil {
			t.Fatal(err)
		}
		objects, err := readObjects(filepath.Join(dir, "acs-forgejo-runners.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objects {
			if o["kind"] == "ScaledJob" {
				obj(o["spec"])["maxReplicaCount"] = "not-a-number"
				break
			}
		}
		rendered, err := readObjects(filepath.Join(dir, "acs-keda-rendered.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if err = validateSchemas(rendered, objects); err == nil {
			t.Fatal("KEDA CRD schema check did not reject wrong type")
		}
	}
	kind := c.kindConfig()
	if kind.URL == c.URL || len(kind.ImagePullSecrets) != 0 || kind.Scope.Type != "user" {
		t.Fatal("Kind uses production settings")
	}
	for _, r := range kind.Runners {
		if r.MaxReplicas != 1 || r.ScalerSecret.Name != "acs-e2e-scaler" || !strings.HasPrefix(r.Image, "localhost:30050/") {
			t.Fatal("Kind isolation lost")
		}
	}
	if c.Namespace != "forgejo-runners" || c.Runners["go"].MaxReplicas != 10 {
		t.Fatal("Kind override mutated production")
	}
}

func TestSecurityGuardsRejectModifiedResources(t *testing.T) {
	for _, change := range []string{"privileged", "hostNetwork", "token-mount", "persistent-storage", "capability", "pending-only-scaling", "empty-poll-exit", "skip-registration"} {
		t.Run(change, func(t *testing.T) {
			m, err := fixtureDeployment(t).render()
			if err != nil {
				t.Fatal(err)
			}
			objects, err := canonicalObjects(m.Resources)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range objects {
				if o["kind"] != "ScaledJob" {
					continue
				}
				pod := obj(at(o, "spec", "jobTargetRef", "template", "spec"))
				main := obj(arr(pod["containers"])[0])
				switch change {
				case "skip-registration":
					obj(arr(pod["initContainers"])[0])["args"] = []any{"true"}
				case "pending-only-scaling":
					obj(o["spec"])["scalingStrategy"] = Obj{"strategy": "accurate"}
				case "empty-poll-exit":
					main["args"] = []any{"--config", "/etc/acs-runner/config.yaml", "one-job"}
				case "privileged":
					obj(main["securityContext"])["privileged"] = true
				case "hostNetwork":
					pod["hostNetwork"] = true
				case "token-mount":
					main["volumeMounts"] = append(arr(main["volumeMounts"]), Obj{"name": "registration-token"})
				case "persistent-storage":
					pod["volumes"] = append(arr(pod["volumes"]), Obj{"persistentVolumeClaim": Obj{"claimName": "shared"}})
				case "capability":
					obj(at(main, "securityContext", "capabilities"))["add"] = []any{"SYS_ADMIN"}
				}
				break
			}
			if err = validateRunnerSecurity(objects); err == nil {
				t.Fatal("unsafe resource accepted")
			}
		})
	}
}

func TestKEDAOverridesCannotDisableDeploymentGuards(t *testing.T) {
	c := fixtureDeployment(t)
	c.KEDA.Values = Obj{"acsForgejoManaged": false, "watchNamespace": "other", "rbac": Obj{"create": false, "aggregateToDefaultRoles": true}, "permissions": Obj{"operator": Obj{"restrict": Obj{"allowAllServiceAccountTokenCreation": true}}}}
	m, err := c.render()
	if err != nil {
		t.Fatal(err)
	}
	if m.KEDAValues["acsForgejoManaged"] != true || m.KEDAValues["watchNamespace"] != c.Namespace || at(m.KEDAValues, "rbac", "create") != true || at(m.KEDAValues, "rbac", "aggregateToDefaultRoles") != false || at(m.KEDAValues, "permissions", "operator", "restrict", "allowAllServiceAccountTokenCreation") != false {
		t.Fatal("unsafe Helm overrides escaped enforcement")
	}
}

func TestBundleFailureKeepsPreviousOutput(t *testing.T) {
	a, r := testApp(t)
	c := fixtureDeployment(t)
	root := t.TempDir()
	previous := filepath.Join(root, "previous")
	if err := os.Mkdir(previous, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "bundle")
	if err := os.Symlink(previous, path); err != nil {
		t.Fatal(err)
	}
	r.run = func(in Invocation) ([]byte, error) {
		if in.Args[0] != "helm" {
			t.Fatalf("bundle invoked %s", in.Args[0])
		}
		return nil, errors.New("render failed")
	}
	if err := a.buildBundle(t.Context(), c, path); err == nil {
		t.Fatal("render failure ignored")
	}
	target, err := os.Readlink(path)
	if err != nil || target != previous {
		t.Fatal("previous bundle replaced after failure")
	}
	if err = a.buildBundle(t.Context(), c, previous); err == nil {
		t.Fatal("user directory overwritten")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatal("failed build left temporary artifacts")
	}
}

func TestRegistrationAdapter(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	mustWrite(t, token, "synthetic-registration-token")
	mock := filepath.Join(dir, "forgejo-runner")
	mustWrite(t, mock, "#!/bin/sh\nset -eu\n[ \"$1\" = --config ] && shift 2\n[ \"$1\" = register ] && shift\n[ \"$1\" = --no-interactive ] && shift\n[ \"$1\" = --ephemeral ] && shift\n[ \"$1\" = --instance ] && shift 2\n[ \"$1\" = --name ] && shift 2\n[ \"$1\" = --token ] && [ \"$2\" = synthetic-registration-token ]\nprintf '{\"token\":\"one-job-identity\"}' > \"$TEST_ROOT/.runner\"\n")
	if err := os.Chmod(mock, 0700); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(strings.ReplaceAll(registrationScript, "/run/registration/token", token), "/data", dir)
	var out, stderr bytes.Buffer
	_, err := (OSRunner{Out: &out, Err: &stderr}).Run(context.Background(), Invocation{Args: []string{"sh", "-ec", script}, Dir: dir, Env: map[string]string{"PATH": dir + ":" + os.Getenv("PATH"), "TEST_ROOT": dir, "FORGEJO_URL": "https://forgejo.invalid", "POD_NAME": "test"}})
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if strings.Contains(out.String()+stderr.String(), "synthetic-registration-token") {
		t.Fatal("registration token leaked")
	}
	st, err := os.Stat(filepath.Join(dir, ".runner"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("registration file permissions: %v %v", st, err)
	}
}

func TestRegistrationFailuresStopInitialization(t *testing.T) {
	for _, tc := range []struct {
		name, token, runnerScript, message string
		wantExit                           int
		wantInvocation                     bool
	}{
		{name: "missing token", runnerScript: "exit 0", message: "token file is missing", wantExit: 1},
		{name: "empty token", token: "empty", runnerScript: "exit 0", message: "token file is missing", wantExit: 1},
		{name: "rejected registration", token: "present", runnerScript: "exit 42", message: "registration failed (exit 42)", wantExit: 42, wantInvocation: true},
		{name: "timed out", token: "present", runnerScript: "exit 124", message: "registration timed out after 120s", wantExit: 124, wantInvocation: true},
		{name: "missing state file", token: "present", runnerScript: "exit 0", message: "registration returned success without", wantExit: 1, wantInvocation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			token := filepath.Join(dir, "token")
			if tc.token != "" {
				content := ""
				if tc.token == "present" {
					content = "synthetic-registration-token"
				}
				mustWrite(t, token, content)
			}
			mock := filepath.Join(dir, "forgejo-runner")
			mustWrite(t, mock, "#!/bin/sh\nset -eu\nprintf invoked > \"$TEST_ROOT/invoked\"\n"+tc.runnerScript+"\n")
			if err := os.Chmod(mock, 0700); err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(strings.ReplaceAll(registrationScript, "/run/registration/token", token), "/data", dir)
			script += "\nprintf started > \"$TEST_ROOT/workflow-started\"\n"
			var stdout, stderr bytes.Buffer
			_, err := (OSRunner{Out: &stdout, Err: &stderr}).Run(t.Context(), Invocation{Args: []string{"sh", "-ec", script}, Dir: dir, Env: map[string]string{"PATH": dir + ":" + os.Getenv("PATH"), "TEST_ROOT": dir, "FORGEJO_URL": "https://forgejo.invalid", "POD_NAME": "test"}})
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.wantExit {
				t.Fatalf("registration exit: %v, stderr: %s", err, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.message) || strings.Contains(stdout.String()+stderr.String(), "synthetic-registration-token") {
				t.Fatal("missing diagnostic or token exposed")
			}
			if _, err := os.Stat(filepath.Join(dir, "workflow-started")); !os.IsNotExist(err) {
				t.Fatal("initialization continued after failed registration")
			}
			_, err = os.Stat(filepath.Join(dir, "invoked"))
			if tc.wantInvocation {
				if err != nil {
					t.Fatal(err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("registration attempted without a token")
			}
		})
	}
}

func TestStrictDeploymentJSON(t *testing.T) {
	for _, data := range []string{`{"typo":1}`, `{} {}`} {
		var r RunnerConfig
		if err := json.Unmarshal([]byte(data), &r); err == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
	var r RunnerConfig
	if err := json.Unmarshal([]byte(`{"image":"registry.invalid/runner:test","labels":["test"],"maxReplicas":0}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.MaxReplicas != 0 || r.RunAsUser != 1000 || r.PollingInterval != 10 {
		t.Fatal("explicit zero was defaulted or omitted defaults were lost")
	}
	c := fixtureDeployment(t)
	c.Runners["go"] = r
	c.inheritDefaults()
	if err := c.validate(); err == nil {
		t.Fatal("zero replicas accepted")
	}
}
