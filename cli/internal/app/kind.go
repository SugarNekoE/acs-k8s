package app

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed assets/*
var assets embed.FS

const kindContext = "kind-acs-test"
const fixtureNS = "acs-e2e-fixtures"
const runnerNS = "acs-e2e-runners"
const kedaNS = "acs-e2e-keda"
const owner = "acs-kind-e2e"
const forgejoHost = "forgejo.acs-e2e-fixtures.svc.cluster.local"

type Obj = map[string]any

func obj(v any) Obj {
	if o, ok := v.(map[string]any); ok {
		return o
	}
	return nil
}
func arr(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}
func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func at(o Obj, keys ...string) any {
	var v any = o
	for _, k := range keys {
		v = obj(v)[k]
	}
	return v
}
func metadata(o Obj) Obj {
	m := obj(o["metadata"])
	if m == nil {
		m = Obj{}
		o["metadata"] = m
	}
	return m
}
func child(o Obj, key string) Obj {
	v := obj(o[key])
	if v == nil {
		v = Obj{}
		o[key] = v
	}
	return v
}
func decodeObjects(b []byte) ([]Obj, error) {
	d := yaml.NewDecoder(bytes.NewReader(b))
	var out []Obj
	for {
		var o Obj
		e := d.Decode(&o)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		if len(o) == 0 {
			continue
		}
		if o["kind"] == "List" {
			if arr(o["items"]) == nil {
				return nil, errors.New("invalid object list items")
			}
			for _, v := range arr(o["items"]) {
				if obj(v) == nil {
					return nil, errors.New("invalid object list")
				}
				out = append(out, obj(v))
			}
		} else {
			out = append(out, o)
		}
	}
	return out, nil
}

type Kind struct{ A *App }

func (k *Kind) command(ctx context.Context, capture, sensitive bool, input []byte, args ...string) ([]byte, error) {
	return k.A.call(ctx, append([]string{"kubectl", "--context", kindContext}, args...), input, capture, sensitive, nil)
}
func (k *Kind) helm(ctx context.Context, capture bool, args ...string) ([]byte, error) {
	return k.A.call(ctx, append([]string{"helm", "--kube-context", kindContext}, args...), nil, capture, false, nil)
}
func (k *Kind) get(ctx context.Context, resource, ns, name string) (Obj, error) {
	args := []string{"get", resource}
	if name != "" {
		args = append(args, name)
	}
	if ns == "*" {
		args = append(args, "-A")
	} else if ns != "" {
		args = append(args, "-n", ns)
	}
	args = append(args, "--ignore-not-found", "-o", "json")
	b, e := k.command(ctx, true, false, nil, args...)
	if e != nil {
		return nil, e
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var o Obj
	if e = json.Unmarshal(b, &o); e != nil {
		return nil, errors.New("invalid Kubernetes JSON response")
	}
	return o, nil
}
func (k *Kind) list(ctx context.Context, resource, ns string) ([]Obj, error) {
	o, e := k.get(ctx, resource, ns, "")
	if e != nil {
		return nil, e
	}
	v, ok := o["items"]
	if !ok {
		return nil, fmt.Errorf("missing items in %s response", resource)
	}
	var out []Obj
	for _, item := range arr(v) {
		if obj(item) == nil {
			return nil, errors.New("invalid Kubernetes list item")
		}
		out = append(out, obj(item))
	}
	if arr(v) == nil {
		return nil, errors.New("invalid Kubernetes list")
	}
	return out, nil
}
func (k *Kind) guard(ctx context.Context) error {
	node, e := k.get(ctx, "node", "", "acs-test-control-plane")
	if e != nil {
		return e
	}
	if str(at(node, "status", "nodeInfo", "architecture")) != "amd64" {
		return errors.New("expected existing amd64 acs-test-control-plane node")
	}
	for _, c := range arr(at(node, "status", "conditions")) {
		if at(obj(c), "type") == "Ready" && at(obj(c), "status") == "True" {
			return nil
		}
	}
	return errors.New("acs-test node is not Ready")
}
func (k *Kind) ownedNamespace(ctx context.Context, name string, create bool) error {
	n, e := k.get(ctx, "namespace", "", name)
	if e != nil {
		return e
	}
	if n != nil {
		if str(at(n, "metadata", "labels", "app.kubernetes.io/part-of")) != owner {
			return fmt.Errorf("refusing unowned namespace %s", name)
		}
		return nil
	}
	if !create {
		return fmt.Errorf("fixture namespace %s does not exist", name)
	}
	return k.apply(ctx, []Obj{{"apiVersion": "v1", "kind": "Namespace", "metadata": Obj{"name": name}}})
}
func (k *Kind) apply(ctx context.Context, objects []Obj) error {
	sensitive := false
	for _, o := range objects {
		child(metadata(o), "labels")["app.kubernetes.io/part-of"] = owner
		if o["kind"] == "Secret" {
			sensitive = true
		}
	}
	b, e := json.Marshal(Obj{"apiVersion": "v1", "kind": "List", "items": objects})
	if e != nil {
		return e
	}
	_, e = k.command(ctx, false, sensitive, b, "apply", "--server-side", "--field-manager=acs-kind-e2e", "-f", "-")
	return e
}
func wait(ctx context.Context, limit time.Duration, fn func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		ok, e := fn(ctx)
		if e != nil {
			return e
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// forwardingOutput signals readiness without a scanner goroutine or discarded I/O errors.
type forwardingOutput struct {
	pending string
	ready   chan struct{}
}

func (w *forwardingOutput) Write(p []byte) (int, error) {
	w.pending += string(p)
	for {
		line, rest, ok := strings.Cut(w.pending, "\n")
		if !ok {
			break
		}
		w.pending = rest
		if strings.HasPrefix(line, "Forwarding from 127.0.0.1:") {
			select {
			case w.ready <- struct{}{}:
			default:
			}
		}
	}
	return len(p), nil
}
func forward(ctx context.Context, ns, service, port string) (func() error, error) {
	childCtx, cancel := context.WithCancel(ctx)
	c := exec.CommandContext(childCtx, "kubectl", "--context", kindContext, "-n", ns, "port-forward", "--address", "127.0.0.1", "service/"+service, port)
	output := &forwardingOutput{ready: make(chan struct{}, 1)}
	var stderr bytes.Buffer
	c.Stdout = output
	c.Stderr = &stderr
	c.WaitDelay = 2 * time.Second
	if err := c.Start(); err != nil {
		cancel()
		return nil, err
	}
	done := make(chan struct{})
	var processErr error
	go func() { processErr = c.Wait(); close(done) }()
	failure := func() error {
		if processErr == nil {
			return errors.New("port-forward exited unexpectedly")
		}
		return fmt.Errorf("port-forward: %w: %s", processErr, strings.TrimSpace(stderr.String()))
	}
	stop := func() error {
		select {
		case <-done:
			cancel()
			return failure()
		default:
		}
		cancel()
		select {
		case <-done:
			// SIGKILL is expected only when we explicitly stop the port-forward.
			var exitErr *exec.ExitError
			if errors.As(processErr, &exitErr) && exitErr.ExitCode() == -1 {
				return nil
			}
			if processErr != nil {
				return failure()
			}
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("port-forward did not stop after cancellation")
		}
	}
	select {
	case <-output.ready:
		return stop, nil
	case <-done:
		cancel()
		return nil, failure()
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), stop())
	case <-time.After(20 * time.Second):
		return nil, errors.Join(errors.New("port-forward startup timed out"), stop())
	}
}
func (k *Kind) bundle(ctx context.Context) (string, error) {
	c, err := k.A.deployment()
	if err != nil {
		return "", err
	}
	path := filepath.Join(k.A.Config.OutputDir, "kind-bundle")
	err = k.A.buildBundle(ctx, c.kindConfig(), path)
	return path, err
}
func (k *Kind) registry(ctx context.Context) error {
	if e := k.ownedNamespace(ctx, fixtureNS, true); e != nil {
		return e
	}
	b, e := assets.ReadFile("assets/acs-kind-registry.yaml")
	if e != nil {
		return e
	}
	objects, e := decodeObjects(b)
	if e != nil {
		return e
	}
	if e = k.apply(ctx, objects); e != nil {
		return e
	}
	bounded, cancel := context.WithTimeout(ctx, 200*time.Second)
	defer cancel()
	_, e = k.command(bounded, false, false, nil, "-n", fixtureNS, "rollout", "status", "deployment/registry", "--timeout=180s")
	return e
}
func (k *Kind) upload(ctx context.Context, selected []string) (result error) {
	stop, e := forward(ctx, fixtureNS, "registry", "35002:5000")
	if e != nil {
		return e
	}
	defer func() { result = errors.Join(result, stop()) }()
	work, e := privateDir("acs-kind-upload-")
	if e != nil {
		return e
	}
	defer func() { result = errors.Join(result, removePrivate(work)) }()
	auth := filepath.Join(work, "auth.json")
	if e = os.WriteFile(auth, []byte(`{"auths":{}}`), 0o600); e != nil {
		return e
	}
	builder := *k.A
	builder.Config.System = "x86_64-linux"
	for _, p := range selected {
		archive := filepath.Join(k.A.Config.OutputDir, "kind-images", "runner-"+p+"-image")
		if e = builder.buildImage(ctx, p, archive); e != nil {
			return e
		}
		if e = k.A.command(ctx, "skopeo", "--tmpdir", work, "copy", "--authfile", auth, "--dest-tls-verify=false", "docker-archive:"+archive, "docker://127.0.0.1:35002/forgejo-runner-"+p+":"+tag); e != nil {
			return e
		}
	}

	return nil
}
func (k *Kind) probePod(ctx context.Context, name string, command []any) error {
	old, e := k.get(ctx, "pod", fixtureNS, name)
	if e != nil {
		return e
	}
	if old != nil {
		if str(at(old, "metadata", "labels", "app.kubernetes.io/part-of")) != owner {
			return errors.New("refusing to replace unowned probe pod")
		}
		if _, e = k.command(ctx, false, false, nil, "-n", fixtureNS, "delete", "pod", name, "--wait", "--timeout=60s"); e != nil {
			return e
		}
	}
	pod := Obj{"apiVersion": "v1", "kind": "Pod", "metadata": Obj{"name": name, "namespace": fixtureNS}, "spec": Obj{
		"automountServiceAccountToken": false, "restartPolicy": "Never", "activeDeadlineSeconds": 180,
		"securityContext": Obj{"runAsUser": 1000, "runAsGroup": 1000},
		"containers":      []any{Obj{"name": "probe", "image": "localhost:30050/forgejo-runner-node24:" + tag, "imagePullPolicy": "Always", "command": command, "securityContext": Obj{"allowPrivilegeEscalation": false, "capabilities": Obj{"drop": []any{"ALL"}}}}}}}
	if e = k.apply(ctx, []Obj{pod}); e != nil {
		return e
	}
	e = wait(ctx, 200*time.Second, func(ctx context.Context) (bool, error) {
		p, e := k.get(ctx, "pod", fixtureNS, name)
		if e != nil {
			return false, e
		}
		phase := str(at(p, "status", "phase"))
		if phase == "Failed" {
			return false, fmt.Errorf("probe %s failed", name)
		}
		return phase == "Succeeded", nil
	})
	_, logErr := k.command(ctx, false, false, nil, "-n", fixtureNS, "logs", name)
	return errors.Join(e, logErr)
}
func (k *Kind) probe(ctx context.Context) error {
	if e := k.registry(ctx); e != nil {
		return e
	}
	if e := k.upload(ctx, []string{"node24"}); e != nil {
		return e
	}
	return k.probePod(ctx, "image-probe", []any{"acs", "workflow", "test", "node24"})
}
func ownedCRD(o Obj) bool {
	group := str(at(o, "spec", "group"))
	if group != "keda.sh" && !strings.HasSuffix(group, ".keda.sh") {
		return false
	}
	return str(at(o, "metadata", "labels", "acs-kind-e2e/owner")) == kindContext ||
		(str(at(o, "metadata", "labels", "app.kubernetes.io/managed-by")) == "Helm" && str(at(o, "metadata", "annotations", "meta.helm.sh/release-name")) == "keda" && str(at(o, "metadata", "annotations", "meta.helm.sh/release-namespace")) == kedaNS)
}
func (k *Kind) checkRelease(ctx context.Context) (bool, error) {
	b, e := k.helm(ctx, true, "list", "-A", "--all", "-o", "json")
	if e != nil {
		return false, e
	}
	var releases []Obj
	if json.Unmarshal(b, &releases) != nil || releases == nil {
		return false, errors.New("invalid Helm release list")
	}
	exists := false
	for _, r := range releases {
		if strings.HasPrefix(str(r["chart"]), "keda-") && str(r["namespace"]) != kedaNS {
			return false, errors.New("another KEDA release exists; refusing shared-resource changes")
		}
		if r["name"] == "keda" && r["namespace"] == kedaNS {
			exists = true
		}
	}
	if exists {
		b, e = k.helm(ctx, true, "get", "values", "keda", "-n", kedaNS, "-o", "json")
		if e != nil {
			return false, e
		}
		var v Obj
		if json.Unmarshal(b, &v) != nil || v["acsForgejoManaged"] != true || v["watchNamespace"] != runnerNS {
			return false, errors.New("refusing unrelated Helm release")
		}
	}
	return exists, nil
}
func (k *Kind) cleanup(ctx context.Context) error {
	for _, ns := range []string{runnerNS, kedaNS, fixtureNS} {
		o, e := k.get(ctx, "namespace", "", ns)
		if e != nil {
			return e
		}
		if o != nil && str(at(o, "metadata", "labels", "app.kubernetes.io/part-of")) != owner {
			return fmt.Errorf("refusing unowned namespace %s", ns)
		}
	}
	release, e := k.checkRelease(ctx)
	if e != nil {
		return e
	}
	crds, e := k.list(ctx, "crds", "")
	if e != nil {
		return e
	}
	var names []string
	for _, crd := range crds {
		if !ownedCRD(crd) {
			continue
		}
		name := str(at(crd, "metadata", "name"))
		instances, e := k.list(ctx, name, "*")
		if e != nil {
			return e
		}
		for _, instance := range instances {
			ns := str(at(instance, "metadata", "namespace"))
			if ns != runnerNS && ns != kedaNS && ns != fixtureNS {
				return fmt.Errorf("foreign %s instance; refusing cleanup", name)
			}
		}
		names = append(names, name)
	}
	if release {
		if _, e = k.helm(ctx, false, "uninstall", "keda", "-n", kedaNS, "--wait", "--timeout", "120s"); e != nil {
			return e
		}
	}
	if _, e = k.command(ctx, false, false, nil, "delete", "namespaces", runnerNS, kedaNS, fixtureNS, "--ignore-not-found", "--wait", "--timeout=180s"); e != nil {
		return e
	}
	for _, name := range names {
		if _, e = k.command(ctx, false, false, nil, "delete", "crd", name, "--ignore-not-found", "--wait", "--timeout=120s"); e != nil {
			return e
		}
	}
	return nil
}
func (k *Kind) network(ctx context.Context) error {
	if e := k.ownedNamespace(ctx, fixtureNS, false); e != nil {
		return e
	}
	svc, e := k.get(ctx, "service", "default", "kubernetes")
	if e != nil {
		return e
	}
	vip := str(at(svc, "spec", "clusterIP"))
	node, e := k.get(ctx, "node", "", "acs-test-control-plane")
	if e != nil {
		return e
	}
	ip := ""
	for _, v := range arr(at(node, "status", "addresses")) {
		if at(obj(v), "type") == "InternalIP" {
			ip = str(at(obj(v), "address"))
		}
	}
	if vip == "" || ip == "" {
		return errors.New("API routing metadata missing")
	}
	var failures []error
	for _, deployment := range []string{"registry", "forgejo"} {
		for _, target := range []struct{ host, port string }{{vip, "443"}, {ip, "6443"}} {
			_, e = k.command(ctx, true, false, []byte("GET /version HTTP/1.0\r\n\r\n"), "-n", fixtureNS, "exec", "-i", "deployment/"+deployment, "--", "nc", "-w", "3", target.host, target.port)
			state := "reachable"
			if e != nil {
				state = "failed"
				failures = append(failures, fmt.Errorf("%s -> %s:%s: %w", deployment, target.host, target.port, e))
			}
			k.A.message("%s -> %s:%s TCP %s", deployment, target.host, target.port, state)
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}
func (k *Kind) diagnose(ctx context.Context) error {
	for _, ns := range []string{runnerNS, kedaNS} {
		if e := k.ownedNamespace(ctx, ns, false); e != nil {
			return e
		}
	}
	if _, e := k.checkRelease(ctx); e != nil {
		return e
	}
	bundle, e := k.bundle(ctx)
	if e != nil {
		return e
	}
	if _, e = k.helm(ctx, false, "upgrade", "--install", "keda", filepath.Join(bundle, "acs-keda-2.20.2.tgz"), "-n", kedaNS, "--values", filepath.Join(bundle, "acs-keda-values.yaml")); e != nil {
		return e
	}
	_, e = k.command(ctx, false, false, nil, "-n", kedaNS, "logs", "deployment/keda-operator", "--tail=100", "--pod-running-timeout=90s")
	return e
}
func (a *App) kind(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: acs kind probe|test|network|diagnose|cleanup --yes")
	}
	if args[0] == "cleanup" {
		if len(args) != 2 || args[1] != "--yes" {
			return errors.New("cleanup requires explicit --yes")
		}
	} else if e := exact(args, 1); e != nil {
		return e
	}
	switch args[0] {
	case "probe", "test", "network", "diagnose", "cleanup":
	default:
		return errors.New("unknown Kind command")
	}
	k := &Kind{A: a}
	if e := k.guard(ctx); e != nil {
		return e
	}
	switch args[0] {
	case "probe":
		return k.probe(ctx)
	case "test":
		return k.test(ctx)
	case "network":
		return k.network(ctx)
	case "diagnose":
		return k.diagnose(ctx)
	case "cleanup":
		return k.cleanup(ctx)
	}
	return nil
}
