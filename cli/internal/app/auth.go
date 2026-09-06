package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Credentials struct {
	Kubeconfig string `json:"kubeconfig"`
	Forgejo    struct {
		Registration string `json:"registration-token"`
		Scaler       string `json:"scaler-token"`
	} `json:"forgejo"`
	ACR struct {
		Address   string `json:"address"`
		Namespace string `json:"namespace"`
		Username  string `json:"username"`
		Password  string `json:"password"`
	} `json:"acr"`
}
type Session struct {
	Credentials Credentials
	Env         map[string]string
}

func privateDir(prefix string) (string, error) {
	candidates := []string{os.Getenv("XDG_RUNTIME_DIR"), os.TempDir()}
	var last error
	for _, base := range candidates {
		if base == "" {
			continue
		}
		path, e := os.MkdirTemp(base, prefix)
		if e == nil {
			return path, nil
		}
		last = e
	}
	return "", fmt.Errorf("cannot create private temporary directory: %w", last)
}
func (a *App) withCluster(ctx context.Context, fn func(*Session) error) (result error) {
	if strings.TrimSpace(a.Config.Context) == "" {
		return errors.New("an explicit --context or configured context is required")
	}
	// Never fall back to the ambient KUBECONFIG for ACS operations.
	if st, e := os.Stat(a.Config.SOPSFile); e != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return errors.New("encrypted SOPS credential file is missing or unreadable")
	}
	b, e := a.call(ctx, []string{"sops", "decrypt", "--output-type", "json", a.Config.SOPSFile}, nil, true, true, nil)
	if e != nil {
		return e
	}
	var credentials Credentials
	if json.Unmarshal(b, &credentials) != nil {
		return errors.New("SOPS output is not a valid credential document")
	}
	if strings.TrimSpace(credentials.Kubeconfig) == "" {
		return errors.New("SOPS kubeconfig must be a nonempty string")
	}
	dir, e := privateDir("acs-kubeconfig-")
	if e != nil {
		return e
	}
	defer func() { result = errors.Join(result, removePrivate(dir)) }()
	path := filepath.Join(dir, "config.yaml")
	content := credentials.Kubeconfig
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if e = os.WriteFile(path, []byte(content), 0o600); e != nil {
		return errors.New("could not materialize private kubeconfig")
	}
	return fn(&Session{Credentials: credentials, Env: map[string]string{"KUBECONFIG": path}})
}
func (a *App) kubectl(ctx context.Context, s *Session, capture, sensitive bool, input []byte, args ...string) ([]byte, error) {
	return a.call(ctx, append([]string{"kubectl", "--context", a.Config.Context}, args...), input, capture, sensitive, s.Env)
}
func (a *App) helm(ctx context.Context, s *Session, capture bool, args ...string) ([]byte, error) {
	return a.call(ctx, append([]string{"helm", "--kube-context", a.Config.Context}, args...), nil, capture, false, s.Env)
}
func secretObjects(c Credentials, cfg Config) ([]byte, error) {
	fields := map[string]string{"forgejo.registration-token": c.Forgejo.Registration, "forgejo.scaler-token": c.Forgejo.Scaler, "acr.address": c.ACR.Address, "acr.namespace": c.ACR.Namespace, "acr.username": c.ACR.Username, "acr.password": c.ACR.Password}
	for field, value := range fields {
		if value == "" {
			return nil, fmt.Errorf("%s must be a nonempty string", field)
		}
	}
	if c.ACR.Address != cfg.Registry || c.ACR.Namespace != cfg.RegistryNamespace {
		return nil, errors.New("SOPS ACR address/namespace does not match the public configuration")
	}
	if strings.ContainsAny(c.ACR.Address, "/ @\t\n") {
		return nil, errors.New("acr.address must be a registry hostname")
	}
	auth := base64.StdEncoding.EncodeToString([]byte(c.ACR.Username + ":" + c.ACR.Password))
	docker, err := json.Marshal(map[string]any{"auths": map[string]any{c.ACR.Address: map[string]string{"username": c.ACR.Username, "password": c.ACR.Password, "auth": auth}}})
	if err != nil {
		return nil, err
	}
	makeSecret := func(name, typ, key, value string) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": name, "namespace": cfg.RunnerNamespace}, "type": typ, "stringData": map[string]string{key: value}}
	}
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{
		makeSecret("acs-forgejo-registration", "Opaque", "token", c.Forgejo.Registration),
		makeSecret("acs-forgejo-scaler", "Opaque", "token", c.Forgejo.Scaler),
		makeSecret("acs-acr-pull", "kubernetes.io/dockerconfigjson", ".dockerconfigjson", string(docker)),
	}})
}
func (a *App) applySecrets(ctx context.Context, s *Session) error {
	if e := a.registry(); e != nil {
		return e
	}
	b, e := secretObjects(s.Credentials, a.Config)
	if e != nil {
		return e
	}
	_, e = a.kubectl(ctx, s, false, true, b, "apply", "-f", "-")
	if e == nil {
		a.message("Applied registration, scaler and image-pull Secrets (values suppressed).")
	}
	return e
}

// Cleanup failures must make the action fail; temporary credentials may remain.
func removePrivate(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove private temporary directory %s: %w", path, err)
	}
	return nil
}
