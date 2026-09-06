package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
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
func TestGoManifestsMatchPreviousNixBundle(t *testing.T) {
	c := fixtureDeployment(t)
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
			if !reflect.DeepEqual(av, ev) {
				t.Fatalf("Go output differs from previous Nix bundle: %s", name)
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
	for _, change := range []string{"privileged", "hostNetwork", "token-mount", "persistent-storage", "capability"} {
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
