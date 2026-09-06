package app

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls []Invocation
	run   func(Invocation) ([]byte, error)
}

func (r *fakeRunner) Run(_ context.Context, in Invocation) ([]byte, error) {
	r.calls = append(r.calls, in)
	if r.run != nil {
		return r.run(in)
	}
	return nil, nil
}
func mustWrite(t *testing.T, path, stringValue string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(stringValue), 0600); err != nil {
		t.Fatal(err)
	}
}
func encoded(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func testApp(t *testing.T) (*App, *fakeRunner) {
	t.Helper()
	root := t.TempDir()
	r := &fakeRunner{}
	a := &App{Root: root, Config: Config{Context: "test-acs", System: "x86_64-linux", Registry: "registry.invalid", RegistryNamespace: "ci", RunnerNamespace: "forgejo-runners", KEDANamespace: "keda", SOPSFile: filepath.Join(root, "encrypted.yaml"), Bundle: root, OutputDir: filepath.Join(root, "output")}, Exec: r, Out: io.Discard}
	mustWrite(t, a.Config.SOPSFile, "synthetic encrypted document; no real credentials")
	return a, r
}
func credentials() Credentials {
	var c Credentials
	c.Kubeconfig = "synthetic-kubeconfig"
	c.Forgejo.Registration = "synthetic-registration"
	c.Forgejo.Scaler = "synthetic-scaler"
	c.ACR.Address = "registry.invalid"
	c.ACR.Namespace = "ci"
	c.ACR.Username = "synthetic-user"
	c.ACR.Password = "synthetic-password"
	return c
}

func TestArguments(t *testing.T) {
	for _, args := range [][]string{{"--bad"}, {"--context"}, {"--context", "--system", "x86_64-linux"}, {"--context="}} {
		if _, _, err := parseArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	flags, args, err := parseArgs([]string{"--repo", "/repo", "images", "build", "go", "--system=aarch64-linux"})
	if err != nil || flags["repo"] != "/repo" || flags["system"] != "aarch64-linux" || !reflect.DeepEqual(args, []string{"images", "build", "go"}) {
		t.Fatalf("%v %v %v", flags, args, err)
	}
	for _, args := range [][]string{{"deploy", "extra"}, {"secrets"}, {"kind", "cleanup"}, {"images", "build", "bad"}, {"images", "publish", "go", "go"}, {"logs", "--all"}} {
		a, r := testApp(t)
		if err := a.Dispatch(t.Context(), args); err == nil || len(r.calls) != 0 {
			t.Fatalf("unsafe arguments %v: %v %v", args, err, r.calls)
		}
	}
}
func TestConfigPrecedence(t *testing.T) {
	for _, key := range []string{"ACR", "ACR_NAMESPACE", "IMAGE_SYSTEM", "CTX", "ACS_SOPS_FILE", "ACS_OUTPUT_DIR", "ACS_BUNDLE", "ACS_RUNNER_NAMESPACE", "ACS_KEDA_NAMESPACE"} {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	path := filepath.Join(root, "settings.json")
	mustWrite(t, path, `{"registry":"file.invalid","context":"file","image_system":"aarch64-linux"}`)
	t.Setenv("CTX", "environment")
	t.Setenv("ACR", "environment.invalid")
	t.Setenv("XDG_STATE_HOME", root)
	c, err := loadConfig(root, map[string]string{"config": path, "context": "flag"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Context != "flag" || c.Registry != "environment.invalid" || c.System != "aarch64-linux" || c.Bundle != filepath.Join(root, "acs-forgejo/bundle") {
		t.Fatalf("%+v", c)
	}
	mustWrite(t, path, `{"password":"not-allowed"}`)
	if _, err = loadConfig(root, map[string]string{"config": path}); err == nil {
		t.Fatal("accepted secret/unknown config field")
	}
}

func TestPrivateSOPSSession(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			a, r := testApp(t)
			t.Setenv("KUBECONFIG", "ambient-must-not-be-used")
			var private string
			r.run = func(in Invocation) ([]byte, error) {
				if in.Args[0] == "sops" {
					if !in.Sensitive || !in.Capture {
						t.Fatal("SOPS output exposed")
					}
					return encoded(t, credentials()), nil
				}
				private = in.Env["KUBECONFIG"]
				if private == "" || private == "ambient-must-not-be-used" {
					t.Fatal("ambient kubeconfig used")
				}
				for path, mode := range map[string]os.FileMode{private: 0600, filepath.Dir(private): 0700} {
					st, err := os.Stat(path)
					if err != nil || st.Mode().Perm() != mode {
						t.Fatalf("private permissions: %v %v", st, err)
					}
				}
				b, err := os.ReadFile(private)
				if err != nil || string(b) != "synthetic-kubeconfig\n" {
					t.Fatalf("kubeconfig: %q %v", b, err)
				}
				if in.Args[1] != "--context" || in.Args[2] != "test-acs" {
					t.Fatal("context missing")
				}
				if fail {
					return nil, errors.New("client failed")
				}
				return nil, nil
			}
			err := a.withCluster(t.Context(), func(s *Session) error {
				_, err := a.kubectl(t.Context(), s, false, false, nil, "cluster-info")
				return err
			})
			if (err != nil) != fail {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Dir(private)); !os.IsNotExist(err) {
				t.Fatalf("kubeconfig remains: %v", err)
			}
		})
	}
	a, r := testApp(t)
	a.Config.Context = ""
	if err := a.Dispatch(t.Context(), []string{"connect"}); err == nil || len(r.calls) != 0 {
		t.Fatal("missing context reached SOPS")
	}
}

func TestSecretsAndRedaction(t *testing.T) {
	a, r := testApp(t)
	c := credentials()
	b, err := secretObjects(c, a.Config)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []struct {
			Metadata   map[string]string
			Type       string
			StringData map[string]string
		}
	}
	if err = json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 3 {
		t.Fatal("missing secrets")
	}
	if list.Items[0].StringData["token"] != c.Forgejo.Registration || list.Items[1].StringData["token"] != c.Forgejo.Scaler || list.Items[2].Type != "kubernetes.io/dockerconfigjson" {
		t.Fatal("wrong secret mapping")
	}
	var docker map[string]any
	if err = json.Unmarshal([]byte(list.Items[2].StringData[".dockerconfigjson"]), &docker); err != nil {
		t.Fatal(err)
	}
	if at(docker, "auths", c.ACR.Address, "password") != c.ACR.Password {
		t.Fatal("wrong registry password")
	}
	c.ACR.Namespace = "other"
	if _, err = secretObjects(c, a.Config); err == nil {
		t.Fatal("registry mismatch accepted")
	}
	c = credentials()
	c.Forgejo.Scaler = ""
	if _, err = secretObjects(c, a.Config); err == nil {
		t.Fatal("empty scaler accepted")
	}
	r.run = func(in Invocation) ([]byte, error) {
		if !in.Sensitive || !bytes.Contains(in.Input, []byte("synthetic-password")) {
			t.Fatal("secret apply not protected")
		}
		if strings.Contains(strings.Join(in.Args, " "), "synthetic-password") {
			t.Fatal("password in argv")
		}
		return nil, errors.New("synthetic-password")
	}
	err = a.applySecrets(t.Context(), &Session{Credentials: credentials()})
	if err == nil || strings.Contains(err.Error(), "synthetic-password") {
		t.Fatalf("leaked error: %v", err)
	}
	var stdout, stderr bytes.Buffer
	_, err = (OSRunner{Out: &stdout, Err: &stderr}).Run(t.Context(), Invocation{Args: []string{"sh", "-c", "echo synthetic-secret; echo synthetic-secret >&2; exit 1"}, Sensitive: true})
	if err == nil || stdout.Len() != 0 || stderr.Len() != 0 || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("sensitive child output leaked")
	}
}

func TestDeploymentGuardsAndOrdering(t *testing.T) {
	for _, scenario := range []string{"fresh", "owned", "missing", "empty", "wrong-pull-type", "foreign", "wrong-namespace", "list-error", "malformed-list", "null-list", "dry-run-error"} {
		t.Run(scenario, func(t *testing.T) {
			a, r := testApp(t)
			p := DeployPlan{Version: 1, RunnerNamespace: a.Config.RunnerNamespace, KEDANamespace: a.Config.KEDANamespace, RequiredSecrets: []SecretRef{{"registration", "token"}}, ImagePullSecrets: []string{"pull"}, ScaledJobs: []string{"acs-runner-go"}}
			mustWrite(t, filepath.Join(a.Config.Bundle, "acs-forgejo-runners.yaml"), "{}")
			mustWrite(t, filepath.Join(a.Config.Bundle, "acs-deploy-plan.json"), string(encoded(t, p)))
			r.run = func(in Invocation) ([]byte, error) {
				if in.Args[2] != "test-acs" {
					t.Fatal("wrong context")
				}
				args := strings.Join(in.Args, " ")
				if strings.Contains(args, "get secret") {
					if !in.Sensitive {
						t.Fatal("secret output not suppressed")
					}
					if scenario == "missing" {
						return nil, errors.New("missing")
					}
					if scenario == "empty" {
						return nil, nil
					}
					if strings.Contains(args, "jsonpath=") {
						if scenario == "wrong-pull-type" {
							return []byte("Opaque"), nil
						}
						return []byte("kubernetes.io/dockerconfigjson"), nil
					}
					return []byte("present"), nil
				}
				if in.Args[0] == "helm" && slices.Contains(in.Args, "list") {
					switch scenario {
					case "list-error":
						return nil, errors.New("API failed")
					case "malformed-list":
						return []byte("{}"), nil
					case "null-list":
						return []byte("null"), nil
					case "fresh":
						return []byte("[]"), nil
					}
					return []byte(`[{"name":"keda"}]`), nil
				}
				if in.Args[0] == "helm" && slices.Contains(in.Args, "get") {
					ns := "forgejo-runners"
					if scenario == "wrong-namespace" {
						ns = "foreign"
					}
					return encoded(t, Obj{"acsForgejoManaged": scenario != "foreign", "watchNamespace": ns}), nil
				}
				if slices.Contains(in.Args, "--dry-run=server") && scenario == "dry-run-error" {
					return nil, errors.New("invalid manifest")
				}
				return nil, nil
			}
			err := a.deploy(t.Context(), &Session{})
			wantSuccess := scenario == "fresh" || scenario == "owned"
			if (err == nil) != wantSuccess {
				t.Fatalf("%s: %v", scenario, err)
			}
			upgrade, dry, apply := -1, -1, -1
			for i, in := range r.calls {
				if slices.Contains(in.Args, "upgrade") {
					upgrade = i
				}
				if slices.Contains(in.Args, "--dry-run=server") {
					dry = i
				}
				if slices.Contains(in.Args, filepath.Join(a.Config.Bundle, "acs-forgejo-runners.yaml")) && !slices.Contains(in.Args, "--dry-run=server") {
					apply = i
				}
			}
			if wantSuccess && !(upgrade >= 0 && dry > upgrade && apply > dry) {
				t.Fatal("unsafe deployment order")
			}
			if !wantSuccess && scenario != "dry-run-error" && upgrade >= 0 {
				t.Fatal("guard allowed Helm mutation")
			}
			if scenario == "dry-run-error" && apply >= 0 {
				t.Fatal("applied after dry-run failure")
			}
		})
	}
}

func TestKindOwnershipFailsBeforeDeletion(t *testing.T) {
	for _, scenario := range []string{"foreign-namespace", "foreign-release", "foreign-instance", "list-error", "owned"} {
		t.Run(scenario, func(t *testing.T) {
			a, r := testApp(t)
			k := &Kind{A: a}
			crd := Obj{"metadata": Obj{"name": "scaledjobs.keda.sh", "labels": Obj{"acs-kind-e2e/owner": kindContext}}, "spec": Obj{"group": "keda.sh"}}
			r.run = func(in Invocation) ([]byte, error) {
				if in.Args[2] != kindContext {
					t.Fatal("Kind used ACS context")
				}
				args := strings.Join(in.Args, " ")
				if strings.Contains(args, "get namespace") {
					o := Obj{"metadata": Obj{"labels": Obj{"app.kubernetes.io/part-of": owner}}}
					if scenario == "foreign-namespace" {
						return []byte(`{"metadata":{}}`), nil
					}
					return encoded(t, o), nil
				}
				if in.Args[0] == "helm" && slices.Contains(in.Args, "list") {
					if scenario == "foreign-release" {
						return []byte(`[{"chart":"keda-2.20.2","namespace":"other"}]`), nil
					}
					return []byte(`[{"name":"keda","chart":"keda-2.20.2","namespace":"acs-e2e-keda"}]`), nil
				}
				if in.Args[0] == "helm" && slices.Contains(in.Args, "get") {
					return encoded(t, Obj{"acsForgejoManaged": true, "watchNamespace": runnerNS}), nil
				}
				if strings.Contains(args, "get crds") {
					return encoded(t, Obj{"items": []Obj{crd}}), nil
				}
				if strings.Contains(args, "get scaledjobs.keda.sh") {
					if scenario == "list-error" {
						return nil, errors.New("list denied")
					}
					if scenario == "foreign-instance" {
						return []byte(`{"items":[{"metadata":{"namespace":"production"}}]}`), nil
					}
					return []byte(`{"items":[]}`), nil
				}
				return nil, nil
			}
			err := k.cleanup(t.Context())
			if (err == nil) != (scenario == "owned") {
				t.Fatal(err)
			}
			for _, in := range r.calls {
				if scenario != "owned" && (slices.Contains(in.Args, "delete") || slices.Contains(in.Args, "uninstall")) {
					t.Fatal("deletion before all guards passed")
				}
			}
		})
	}
	if ownedCRD(Obj{"spec": Obj{"group": "keda.sh"}}) {
		t.Fatal("unmarked CRD adopted")
	}
}

func TestKindReportAndCertificates(t *testing.T) {
	r := KindReport{InitialZero: true, FinalZero: true, Run: WorkflowRun{Status: "completed", Conclusion: "success"}, Pools: map[string]WorkflowJob{}, Pods: map[string]Obj{}}
	for _, p := range pools {
		r.Pools[p] = WorkflowJob{Status: "completed", Conclusion: "success"}
		r.Pods[p] = Obj{}
	}
	if !r.complete() {
		t.Fatal("complete evidence rejected")
	}
	r.FinalZero = false
	if r.complete() {
		t.Fatal("missing cleanup evidence accepted")
	}
	r.FinalZero = true
	r.Pools["docker"] = WorkflowJob{Status: "completed", Conclusion: "failure"}
	if r.complete() {
		t.Fatal("failed Docker pool passed")
	}
	c, err := certificates()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(c.Cert)
	if block == nil {
		t.Fatal("invalid certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(c.CA) {
		t.Fatal("invalid CA")
	}
	for _, host := range []string{forgejoHost, "localhost", "127.0.0.1"} {
		if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "production.invalid"}); err == nil {
		t.Fatal("wrong hostname trusted")
	}
}

func TestForgejoQueueAndErrors(t *testing.T) {
	for _, body := range []string{"null", "[]", `[{"id":1}]`, "{}", "false", "0", `""`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "token synthetic" {
					t.Error("missing token")
				}
				if _, err := io.WriteString(w, body); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			api := ForgejoAPI{URL: server.URL, Auth: "token synthetic", Client: server.Client()}
			var queue []Obj
			err := api.request(t.Context(), "GET", "/user/actions/runners/jobs", nil, &queue)
			valid := body == "null" || strings.HasPrefix(body, "[")
			if (err == nil) != valid {
				t.Fatalf("queue %s: %v", body, err)
			}
		})
	}
}

func TestBuildahCredentialsContextAndCleanup(t *testing.T) {
	for _, failure := range []string{"", "login", "build", "push"} {
		t.Run("failure-"+failure, func(t *testing.T) {
			a, r := testApp(t)
			t.Setenv("ACR", "registry.invalid")
			t.Setenv("TARGET_IMAGE", "registry.invalid/ci/smoke:test")
			t.Setenv("ACR_USERNAME", "synthetic-user")
			t.Setenv("ACR_PASSWORD", "synthetic-password")
			var authFile string
			r.run = func(in Invocation) ([]byte, error) {
				if in.Args[0] == "node" {
					return []byte("v24.19.0\n"), nil
				}
				if strings.Contains(strings.Join(in.Args, " "), "synthetic-password") {
					t.Fatal("password in arguments")
				}
				if i := slices.Index(in.Args, "--authfile"); i >= 0 {
					authFile = in.Args[i+1]
					st, err := os.Stat(authFile)
					if err != nil || st.Mode().Perm() != 0600 {
						t.Fatalf("auth permissions: %v %v", st, err)
					}
				}
				if slices.Contains(in.Args, "login") && (!in.Sensitive || string(in.Input) != "synthetic-password") {
					t.Fatal("login was not protected")
				}
				if slices.Contains(in.Args, "build") {
					buildDir := in.Args[len(in.Args)-1]
					if filepath.Dir(authFile) == buildDir {
						t.Fatal("auth file in build context")
					}
					b, err := os.ReadFile(filepath.Join(buildDir, "Dockerfile"))
					if err != nil || !bytes.Contains(b, []byte("RUN echo acs-ok")) {
						t.Fatalf("Dockerfile: %q %v", b, err)
					}
				}
				if failure != "" && slices.Contains(in.Args, failure) {
					return nil, errors.New("synthetic child failure")
				}
				return nil, nil
			}
			err := a.workflow(t.Context(), []string{"test", "docker"})
			if (err != nil) != (failure != "") {
				t.Fatalf("%s: %v", failure, err)
			}
			if _, err = os.Stat(filepath.Dir(authFile)); !os.IsNotExist(err) {
				t.Fatalf("temporary auth remains: %v", err)
			}
			if failure != "" && !slices.Contains(r.calls[len(r.calls)-1].Args, failure) {
				t.Fatal("continued after child failure")
			}
		})
	}
}

func TestSnapshotPreReadAndReplacement(t *testing.T) {
	a, _ := testApp(t)
	target := a.Root
	a.Config.Bundle = filepath.Join(a.Root, "bundle")
	if err := os.Mkdir(a.Config.Bundle, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range snapshots {
		mustWrite(t, filepath.Join(target, name), "old")
		mustWrite(t, filepath.Join(a.Config.Bundle, name), "new")
	}
	if err := os.Remove(filepath.Join(a.Config.Bundle, snapshots[3])); err != nil {
		t.Fatal(err)
	}
	if err := a.bundleCommand(t.Context(), []string{"snapshots"}); err == nil {
		t.Fatal("missing snapshot accepted")
	}
	for _, name := range snapshots {
		b, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(b) != "old" {
			t.Fatal("changed snapshots before reading all sources")
		}
	}
	mustWrite(t, filepath.Join(a.Config.Bundle, snapshots[3]), "new")
	if err := os.Chmod(filepath.Join(target, snapshots[0]), 0444); err != nil {
		t.Fatal(err)
	}
	if err := a.bundleCommand(t.Context(), []string{"snapshots"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range snapshots {
		b, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || string(b) != "new" {
			t.Fatal("snapshot not replaced")
		}
	}
}

func TestSOPSFailuresStopBeforeCluster(t *testing.T) {
	for _, response := range []string{"invalid", `{}`, `{"kubeconfig":123}`, `{"kubeconfig":"  "}`} {
		a, r := testApp(t)
		r.run = func(Invocation) ([]byte, error) { return []byte(response), nil }
		if err := a.Dispatch(t.Context(), []string{"connect"}); err == nil || len(r.calls) != 1 {
			t.Fatalf("invalid SOPS response accepted: %s", response)
		}
	}
	a, r := testApp(t)
	if err := os.Remove(a.Config.SOPSFile); err != nil {
		t.Fatal(err)
	}
	if err := a.Dispatch(t.Context(), []string{"secrets", "apply"}); err == nil || len(r.calls) != 0 {
		t.Fatal("missing SOPS document reached external tool")
	}
}
