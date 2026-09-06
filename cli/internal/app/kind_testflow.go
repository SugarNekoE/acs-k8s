package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type Certificates struct{ CA, Cert, Key []byte }

func certificates() (Certificates, error) {
	var out Certificates
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		return out, e
	}
	leafKey, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		return out, e
	}
	serial := func() (*big.Int, error) {
		n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return nil, err
		}
		return n.Add(n, big.NewInt(1)), nil
	}
	caSerial, e := serial()
	if e != nil {
		return out, e
	}
	leafSerial, e := serial()
	if e != nil {
		return out, e
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: caSerial, Subject: pkix.Name{CommonName: "acs-kind-e2e-ca"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		return out, e
	}
	ca, e = x509.ParseCertificate(caDER)
	if e != nil {
		return out, e
	}
	leaf := &x509.Certificate{SerialNumber: leafSerial, Subject: pkix.Name{CommonName: forgejoHost}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{forgejoHost, "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, e := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if e != nil {
		return out, e
	}
	out.CA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	out.Cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	out.Key = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})
	return out, nil
}
func randomString() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type ForgejoAPI struct {
	URL, Auth string
	Client    *http.Client
}

func (f ForgejoAPI) request(ctx context.Context, method, path string, data, out any) (result error) {
	var reader io.Reader
	if data != nil {
		b, e := json.Marshal(data)
		if e != nil {
			return e
		}
		reader = strings.NewReader(string(b))
	}
	req, e := http.NewRequestWithContext(ctx, method, f.URL+"/api/v1"+path, reader)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", f.Auth)
	req.Header.Set("Content-Type", "application/json")
	response, e := f.Client.Do(req)
	if e != nil {
		return errors.New("forgejo request failed (transport/TLS)")
	}
	defer func() { result = errors.Join(result, response.Body.Close()) }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("forgejo %s %s returned HTTP %d", method, path, response.StatusCode)
	}
	if out != nil {
		if e = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(out); e != nil {
			return errors.New("forgejo returned invalid JSON")
		}
	}
	return nil
}
func (f ForgejoAPI) token(value string) ForgejoAPI { f.Auth = "token " + value; return f }

type WorkflowRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}
type WorkflowJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	RunnerID   int64  `json:"runner_id"`
	RunnerName string `json:"runner_name"`
}

func success(status, conclusion string) bool {
	if conclusion != "" {
		return conclusion == "success"
	}
	return status == "success"
}

type KindReport struct {
	Context     string                 `json:"context"`
	Started     time.Time              `json:"started_at"`
	Finished    time.Time              `json:"finished_at"`
	Passed      bool                   `json:"passed"`
	Error       string                 `json:"error,omitempty"`
	Stages      []string               `json:"stages"`
	InitialZero bool                   `json:"initial_scale_zero"`
	FinalZero   bool                   `json:"final_scale_zero"`
	Run         WorkflowRun            `json:"run"`
	Pools       map[string]WorkflowJob `json:"pools"`
	Pods        map[string]Obj         `json:"pods"`
}

func (r *KindReport) complete() bool {
	if !r.InitialZero || !r.FinalZero || !success(r.Run.Status, r.Run.Conclusion) || len(r.Pools) != 4 || len(r.Pods) < 4 {
		return false
	}
	for _, p := range pools {
		job, ok := r.Pools[p]
		if !ok || !success(job.Status, job.Conclusion) {
			return false
		}
	}
	return true
}
func (k *Kind) capturePods(ctx context.Context, r *KindReport) error {
	pods, e := k.list(ctx, "pods", runnerNS)
	if e != nil {
		return e
	}
	for _, p := range pods {
		spec := obj(p["spec"])
		containers := arr(spec["containers"])
		if len(containers) != 1 {
			return errors.New("unexpected runner container count")
		}
		main := obj(containers[0])
		security := obj(main["securityContext"])
		if spec["automountServiceAccountToken"] != false || security["allowPrivilegeEscalation"] != false || security["privileged"] == true {
			return errors.New("runner API-token/privilege boundary violated")
		}
		caps := obj(security["capabilities"])
		if !reflect.DeepEqual(arr(caps["drop"]), []any{"ALL"}) {
			return errors.New("runner must drop ALL capabilities")
		}
		for _, cap := range arr(caps["add"]) {
			if cap == "SYS_ADMIN" {
				return errors.New("runner gained SYS_ADMIN")
			}
		}
		for _, mount := range arr(main["volumeMounts"]) {
			if obj(mount)["name"] == "registration-token" {
				return errors.New("reusable token mounted in workflow container")
			}
		}
		if !reflect.DeepEqual(arr(main["args"]), []any{"--config", "/etc/acs-runner/config.yaml", "one-job"}) {
			return errors.New("runner is not using one-job")
		}
		name := str(at(p, "metadata", "name"))
		r.Pods[name] = Obj{"uid": at(p, "metadata", "uid"), "phase": at(p, "status", "phase"), "resources": main["resources"], "security": security, "containers": at(p, "status", "containerStatuses")}
	}
	return nil
}
func (k *Kind) createSecret(ctx context.Context, ns, name, typ string, values map[string]string) error {
	return k.apply(ctx, []Obj{{"apiVersion": "v1", "kind": "Secret", "metadata": Obj{"name": name, "namespace": ns}, "type": typ, "stringData": values}})
}
func (k *Kind) startForgejo(ctx context.Context, c Certificates) error {
	if e := k.createSecret(ctx, fixtureNS, "forgejo-tls", "kubernetes.io/tls", map[string]string{"tls.crt": string(c.Cert), "tls.key": string(c.Key)}); e != nil {
		return e
	}
	env := map[string]string{"FORGEJO__security__INSTALL_LOCK": "true", "FORGEJO__database__DB_TYPE": "sqlite3", "FORGEJO__server__PROTOCOL": "https", "FORGEJO__server__HTTP_PORT": "3000", "FORGEJO__server__ROOT_URL": "https://" + forgejoHost + ":3000/", "FORGEJO__server__CERT_FILE": "/tls/tls.crt", "FORGEJO__server__KEY_FILE": "/tls/tls.key", "FORGEJO__server__DISABLE_SSH": "true", "FORGEJO__service__DISABLE_REGISTRATION": "true", "FORGEJO__actions__ENABLED": "true"}
	var environment []any
	for key, value := range env {
		environment = append(environment, Obj{"name": key, "value": value})
	}
	objects := []Obj{{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": Obj{"name": "forgejo", "namespace": fixtureNS}, "spec": Obj{"replicas": 1, "selector": Obj{"matchLabels": Obj{"app": "acs-e2e-forgejo"}}, "template": Obj{"metadata": Obj{"labels": Obj{"app": "acs-e2e-forgejo"}}, "spec": Obj{
		"automountServiceAccountToken": false, "securityContext": Obj{"runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000},
		"containers": []any{Obj{"name": "forgejo", "image": "codeberg.org/forgejo/forgejo:16.0.3-rootless", "env": environment, "ports": []any{Obj{"containerPort": 3000}},
			"readinessProbe": Obj{"httpGet": Obj{"scheme": "HTTPS", "path": "/api/v1/version", "port": 3000}},
			"resources":      Obj{"requests": Obj{"cpu": "500m", "memory": "512Mi"}, "limits": Obj{"cpu": "2", "memory": "2Gi"}},
			"volumeMounts":   []any{Obj{"name": "data", "mountPath": "/var/lib/gitea"}, Obj{"name": "tls", "mountPath": "/tls", "readOnly": true}}}},
		"volumes": []any{Obj{"name": "data", "emptyDir": Obj{}}, Obj{"name": "tls", "secret": Obj{"secretName": "forgejo-tls"}}},
	}}}}, {"apiVersion": "v1", "kind": "Service", "metadata": Obj{"name": "forgejo", "namespace": fixtureNS}, "spec": Obj{"selector": Obj{"app": "acs-e2e-forgejo"}, "ports": []any{Obj{"port": 3000, "targetPort": 3000}}}}}
	if e := k.apply(ctx, objects); e != nil {
		return e
	}
	bounded, cancel := context.WithTimeout(ctx, 330*time.Second)
	defer cancel()
	_, e := k.command(bounded, false, false, nil, "-n", fixtureNS, "rollout", "status", "deployment/forgejo", "--timeout=300s")
	return e
}
func readObjects(path string) ([]Obj, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	return decodeObjects(b)
}
func (k *Kind) test(ctx context.Context) (result error) {
	r := &KindReport{Context: kindContext, Started: time.Now().UTC(), Pools: map[string]WorkflowJob{}, Pods: map[string]Obj{}}
	reportPath := filepath.Join(k.A.Root, "acs-k8s/acs-kind-test-report.md")
	writeReport := func() error {
		b, e := json.MarshalIndent(r, "", "  ")
		if e != nil {
			return e
		}
		return atomicWrite(reportPath, []byte("# Kind ACS end-to-end report\n\nTest-only resources remain for inspection. No real Forgejo/ACR credentials were used.\n\n```json\n"+string(b)+"\n```\n"), 0o600)
	}
	if e := writeReport(); e != nil {
		return e
	}
	defer func() {
		r.Finished = time.Now().UTC()
		r.Passed = result == nil && r.complete()
		if !r.Passed && result == nil {
			result = errors.New("incomplete end-to-end evidence")
		}
		if result != nil {
			r.Error = result.Error()
		}
		result = errors.Join(result, writeReport())
		k.A.message("Report: %s", reportPath)
	}()
	stage := func(message string) { r.Stages = append(r.Stages, message); k.A.message("%s", message) }
	old, e := k.get(ctx, "deployment", fixtureNS, "forgejo")
	if e != nil {
		return e
	}
	if old != nil {
		return errors.New("existing Forgejo fixture: inspect it, then explicitly run acs kind cleanup --yes")
	}
	crds, e := k.list(ctx, "crds", "")
	if e != nil {
		return e
	}
	for _, crd := range crds {
		g := str(at(crd, "spec", "group"))
		if g == "keda.sh" || strings.HasSuffix(g, ".keda.sh") {
			return errors.New("KEDA CRDs already exist; refusing to adopt an existing controller")
		}
	}
	stage("Importing Node 24 image and running native CLI/image probe")
	if e = k.probe(ctx); e != nil {
		return e
	}
	stage("Checking pod-to-API Service routing before creating credentials or KEDA")
	if e = k.probePod(ctx, "network-preflight", []any{"acs", "workflow", "network"}); e != nil {
		return fmt.Errorf("Kind Service routing preflight failed; repair networking separately: %w", e)
	}
	if e = k.upload(ctx, pools); e != nil {
		return e
	}
	bundle, e := k.bundle(ctx)
	if e != nil {
		return e
	}
	for _, ns := range []string{runnerNS, kedaNS} {
		if e = k.ownedNamespace(ctx, ns, true); e != nil {
			return e
		}
	}
	namespaces, e := readObjects(filepath.Join(bundle, "acs-namespaces.yaml"))
	if e != nil {
		return e
	}
	if e = k.apply(ctx, namespaces); e != nil {
		return e
	}
	stage("Creating short-lived HTTPS certificates and isolated Forgejo")
	certs, e := certificates()
	if e != nil {
		return e
	}
	publicCA, e := publicCABundle()
	if e != nil {
		return e
	}
	trust := string(publicCA) + "\n" + string(certs.CA)
	for _, ns := range []string{runnerNS, kedaNS} {
		if e = k.apply(ctx, []Obj{{"apiVersion": "v1", "kind": "ConfigMap", "metadata": Obj{"name": "acs-e2e-ca", "namespace": ns}, "data": Obj{"ca-bundle.crt": trust}}}); e != nil {
			return e
		}
	}
	if e = k.startForgejo(ctx, certs); e != nil {
		return e
	}
	stop, e := forward(ctx, fixtureNS, "forgejo", "35001:3000")
	if e != nil {
		return e
	}
	defer func() { result = errors.Join(result, stop()) }()
	password, e := randomString()
	if e != nil {
		return e
	}
	// Minimal remote stdin adapter: the orchestration lives here, and no password
	// enters the Kubernetes exec URL/audit arguments or a repository script.
	adapter := `set -eu; IFS= read -r password; exec forgejo --config /var/lib/gitea/custom/conf/app.ini admin user create --username acse2e --email acs-e2e@example.invalid --password "$password" --admin --must-change-password=false`
	if _, e = k.command(ctx, true, true, []byte(password+"\n"), "-n", fixtureNS, "exec", "-i", "deployment/forgejo", "--", "sh", "-c", adapter); e != nil {
		return e
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certs.CA) {
		return errors.New("invalid fixture CA")
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	defer client.CloseIdleConnections()
	api := ForgejoAPI{URL: "https://127.0.0.1:35001", Auth: "Basic " + base64.StdEncoding.EncodeToString([]byte("acse2e:"+password)), Client: client}
	var admin, scaler struct {
		SHA1 string `json:"sha1"`
	}
	if e = api.request(ctx, "POST", "/users/acse2e/tokens", Obj{"name": "fixture-admin", "scopes": []string{"all"}}, &admin); e != nil {
		return e
	}
	if e = api.request(ctx, "POST", "/users/acse2e/tokens", Obj{"name": "fixture-queue", "scopes": []string{"read:user"}}, &scaler); e != nil {
		return e
	}
	if admin.SHA1 == "" || scaler.SHA1 == "" {
		return errors.New("forgejo returned an empty token")
	}
	api = api.token(admin.SHA1)
	queueAPI := api.token(scaler.SHA1)
	var registration struct {
		Token string `json:"token"`
	}
	if e = api.request(ctx, "GET", "/user/actions/runners/registration-token", nil, &registration); e != nil {
		return e
	}
	if registration.Token == "" {
		return errors.New("empty registration token")
	}
	if e = k.createSecret(ctx, runnerNS, "acs-e2e-registration", "Opaque", map[string]string{"token": registration.Token}); e != nil {
		return e
	}
	if e = k.createSecret(ctx, runnerNS, "acs-e2e-scaler", "Opaque", map[string]string{"token": scaler.SHA1}); e != nil {
		return e
	}
	var queue []Obj
	if e = queueAPI.request(ctx, "GET", "/user/actions/runners/jobs?labels=acs-kind-nix", nil, &queue); e != nil {
		return e
	}
	if len(queue) != 0 {
		return errors.New("new fixture queue is not empty")
	}
	stage("Installing isolated KEDA; failures retain controller pods for diagnosis")
	rendered, e := readObjects(filepath.Join(bundle, "acs-keda-rendered.yaml"))
	if e != nil {
		return e
	}
	for _, o := range rendered {
		if o["kind"] != "CustomResourceDefinition" {
			continue
		}
		labels := child(metadata(o), "labels")
		labels["acs-kind-e2e/owner"] = kindContext
		labels["app.kubernetes.io/managed-by"] = "Helm"
		annotations := child(metadata(o), "annotations")
		annotations["meta.helm.sh/release-name"] = "keda"
		annotations["meta.helm.sh/release-namespace"] = kedaNS
		if e = k.apply(ctx, []Obj{o}); e != nil {
			return e
		}
	}
	helmCtx, cancel := context.WithTimeout(ctx, 330*time.Second)
	_, e = k.helm(helmCtx, false, "upgrade", "--install", "keda", filepath.Join(bundle, "acs-keda-2.20.2.tgz"), "-n", kedaNS, "--values", filepath.Join(bundle, "acs-keda-values.yaml"), "--wait", "--timeout", "5m")
	cancel()
	if e != nil {
		return e
	}
	resources, e := readObjects(filepath.Join(bundle, "acs-forgejo-runners.yaml"))
	if e != nil {
		return e
	}
	for _, o := range resources {
		if o["kind"] != "ScaledJob" {
			continue
		}
		pod := obj(at(o, "spec", "jobTargetRef", "template", "spec"))
		pod["volumes"] = append(arr(pod["volumes"]), Obj{"name": "acs-e2e-ca", "configMap": Obj{"name": "acs-e2e-ca"}})
		containers := append(append([]any{}, arr(pod["containers"])...), arr(pod["initContainers"])...)
		for _, v := range containers {
			c := obj(v)
			c["volumeMounts"] = append(arr(c["volumeMounts"]), Obj{"name": "acs-e2e-ca", "mountPath": "/etc/acs-kind-ca", "readOnly": true})
			for _, key := range []string{"SSL_CERT_FILE", "NIX_SSL_CERT_FILE", "GIT_SSL_CAINFO", "NODE_EXTRA_CA_CERTS"} {
				c["env"] = append(arr(c["env"]), Obj{"name": key, "value": "/etc/acs-kind-ca/ca-bundle.crt"})
			}
		}
	}
	payload, e := json.Marshal(Obj{"apiVersion": "v1", "kind": "List", "items": resources})
	if e != nil {
		return e
	}
	if _, e = k.command(ctx, false, false, payload, "apply", "--dry-run=server", "-f", "-"); e != nil {
		return e
	}
	if e = k.apply(ctx, resources); e != nil {
		return e
	}
	for _, p := range pools {
		if _, e = k.command(ctx, false, false, nil, "-n", runnerNS, "wait", "--for=condition=Ready", "scaledjob/acs-runner-"+p, "--timeout=120s"); e != nil {
			return e
		}
	}
	jobs, e := k.list(ctx, "jobs", runnerNS)
	if e != nil {
		return e
	}
	if len(jobs) != 0 {
		return errors.New("expected initial scale-to-zero")
	}
	r.InitialZero = true
	stage("Dispatching four real workflow jobs using the CLI baked into each image")
	if e = api.request(ctx, "POST", "/user/repos", Obj{"name": "runner-smoke", "auto_init": true, "private": false, "default_branch": "main"}, nil); e != nil {
		return e
	}
	repo := "/repos/acse2e/runner-smoke"
	if e = api.request(ctx, "PATCH", repo, Obj{"has_actions": true}, nil); e != nil {
		return e
	}
	workflowJobs := Obj{}
	for _, p := range pools {
		workflowJobs[p] = Obj{"name": p, "runs-on": "acs-kind-" + p, "steps": []any{Obj{"run": "acs workflow kind-test " + p}}}
	}
	workflow, e := json.Marshal(Obj{"name": "ACS Kind end-to-end", "on": Obj{"workflow_dispatch": Obj{}}, "jobs": workflowJobs})
	if e != nil {
		return e
	}
	if e = api.request(ctx, "POST", repo+"/contents/.forgejo/workflows/acs-kind-e2e.yaml", Obj{"branch": "main", "message": "add isolated CLI test", "content": base64.StdEncoding.EncodeToString(workflow)}, nil); e != nil {
		return e
	}
	if e = api.request(ctx, "POST", repo+"/actions/workflows/acs-kind-e2e.yaml/dispatches", Obj{"ref": "main"}, nil); e != nil {
		return e
	}
	e = wait(ctx, 20*time.Minute, func(ctx context.Context) (bool, error) {
		if e := k.capturePods(ctx, r); e != nil {
			return false, e
		}
		var runs struct {
			Runs []WorkflowRun `json:"workflow_runs"`
		}
		if e := api.request(ctx, "GET", repo+"/actions/runs", nil, &runs); e != nil {
			return false, e
		}
		if len(runs.Runs) == 0 {
			return false, nil
		}
		r.Run = runs.Runs[0]
		var response struct {
			Jobs []WorkflowJob `json:"jobs"`
		}
		if e := api.request(ctx, "GET", fmt.Sprintf("%s/actions/runs/%d/jobs", repo, r.Run.ID), nil, &response); e != nil {
			return false, e
		}
		for _, job := range response.Jobs {
			r.Pools[job.Name] = job
		}
		switch r.Run.Status {
		case "completed", "success", "failure", "cancelled", "skipped":
			return true, nil
		}
		return false, nil
	})
	if e != nil {
		return e
	}
	if e = k.capturePods(ctx, r); e != nil {
		return e
	}
	stage("Checking completion, queue drain and TTL cleanup")
	if e = wait(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		jobs, e := k.list(ctx, "jobs", runnerNS)
		return len(jobs) == 0, e
	}); e != nil {
		return e
	}
	r.FinalZero = true
	for _, p := range pools {
		queue = nil
		if e = queueAPI.request(ctx, "GET", "/user/actions/runners/jobs?labels=acs-kind-"+p, nil, &queue); e != nil {
			return e
		}
		if len(queue) != 0 {
			return errors.New("queue did not drain")
		}
	}
	if !r.complete() {
		return errors.New("four successful pools, observed pods and scale-to-zero are required; do not weaken security to force a pass")
	}
	stage("PASS: four pools, native builds/push, one-job execution and return to zero")
	return nil
}
