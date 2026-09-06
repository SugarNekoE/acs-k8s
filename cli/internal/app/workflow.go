package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *App) workflow(ctx context.Context, args []string) (result error) {
	if len(args) == 1 && args[0] == "network" {
		host := os.Getenv("KUBERNETES_SERVICE_HOST")
		port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
		if port == "" {
			port = "443"
		}
		if host == "" {
			return errors.New("kubernetes API Service environment is missing")
		}
		d := net.Dialer{Timeout: 5 * time.Second}
		c, e := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if e != nil {
			return fmt.Errorf("kubernetes API Service transport is blocked: %w", e)
		}
		if e = c.Close(); e != nil {
			return e
		}
		a.message("API Service TCP preflight passed (not a TLS/authentication check).")
		return nil
	}
	if len(args) != 2 || (args[0] != "test" && args[0] != "kind-test" && args[0] != "image-test") {
		return errors.New("usage: acs workflow test|kind-test nix|go|node24|docker")
	}
	if _, e := selectPools(args[1:]); e != nil {
		return e
	}
	kind := args[0] == "kind-test"
	pool := args[1]
	node, e := a.capture(ctx, "node", "--version")
	if e != nil {
		return e
	}
	if !strings.HasPrefix(string(node), "v24.") {
		return errors.New("node.js major version must be 24")
	}
	a.message("Node %s", strings.TrimSpace(string(node)))
	work, e := privateDir("acs-workflow-")
	if e != nil {
		return e
	}
	defer func() { result = errors.Join(result, removePrivate(work)) }()
	switch pool {
	case "nix":
		if e = a.command(ctx, "devenv", "version"); e != nil {
			return e
		}
		if e = a.command(ctx, "nix", "--version"); e != nil {
			return e
		}
		expr := `derivation { name = "acs-cli-smoke"; system = builtins.currentSystem; builder = "/bin/sh"; args = [ "-c" "echo acs-ok > $out" ]; }`
		file := filepath.Join(work, "smoke.nix")
		if e = os.WriteFile(file, []byte(expr), 0o600); e != nil {
			return e
		}
		b, e := a.capture(ctx, "nix-build", "--option", "substituters", "", "--option", "builders", "", "--no-out-link", file)
		if e != nil {
			return e
		}
		value, e := os.ReadFile(strings.TrimSpace(string(b)))
		if e != nil {
			return e
		}
		if strings.TrimSpace(string(value)) != "acs-ok" {
			return errors.New("local Nix build produced an unexpected result")
		}
	case "go":
		if e = a.command(ctx, "go", "version"); e != nil {
			return e
		}
		file := filepath.Join(work, "main.go")
		if e = os.WriteFile(file, []byte("package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"acs-ok\")}\n"), 0o600); e != nil {
			return e
		}
		binary := filepath.Join(work, "hello")
		if _, e = a.call(ctx, []string{"go", "build", "-o", binary, file}, nil, false, false, map[string]string{"GOTOOLCHAIN": "local"}); e != nil {
			return e
		}
		b, e := a.capture(ctx, binary)
		if e != nil {
			return e
		}
		if strings.TrimSpace(string(b)) != "acs-ok" {
			return errors.New("go executable produced an unexpected result")
		}
	case "node24":
		if e = a.command(ctx, "node", "-e", "if (2 + 2 !== 4) process.exit(1); console.log('acs-ok')"); e != nil {
			return e
		}
	case "docker":
		if args[0] == "image-test" {
			return a.builderBinaries(ctx)
		}
		if e = a.buildahTest(ctx, work, kind); e != nil {
			return e
		}
	}
	a.message("PASS: %s toolchain", pool)
	return nil
}
func (a *App) buildahTest(ctx context.Context, work string, kind bool) error {
	for _, tool := range []string{"buildah", "skopeo"} {
		if e := a.command(ctx, tool, "--version"); e != nil {
			return e
		}
	}
	short, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if e := a.command(short, "crun", "--version"); e != nil {
		return e
	}
	registry := os.Getenv("ACR")
	target := os.Getenv("TARGET_IMAGE")
	base := "docker.io/library/alpine:3.22"
	if kind {
		registry = "registry.acs-e2e-fixtures.svc.cluster.local:5000"
		target = registry + "/acs-kind-proof:tested"
		base = registry + "/forgejo-runner-node24:" + tag
	}
	if registry == "" || target == "" || !strings.HasPrefix(target, registry+"/") {
		return errors.New("ACR and a TARGET_IMAGE under that registry are required")
	}
	auth := filepath.Join(work, "auth.json")
	if e := os.WriteFile(auth, []byte(`{"auths":{}}`), 0o600); e != nil {
		return e
	}
	if !kind {
		username, password := os.Getenv("ACR_USERNAME"), os.Getenv("ACR_PASSWORD")
		if username == "" || password == "" {
			return errors.New("ACR_USERNAME and ACR_PASSWORD workflow secrets are required")
		}
		if _, e := a.call(ctx, []string{"buildah", "login", "--authfile", auth, "--username", username, "--password-stdin", registry}, []byte(password), false, true, nil); e != nil {
			return e
		}
	}
	dir := filepath.Join(work, "context")
	if e := os.Mkdir(dir, 0o700); e != nil {
		return e
	}
	dockerfile := "FROM " + base + "\nUSER 0:0\nRUN echo acs-ok > /proof.txt\nCMD [\"cat\", \"/proof.txt\"]\n"
	if e := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); e != nil {
		return e
	}
	args := []string{"buildah", "--storage-driver", "vfs", "build", "--isolation", "chroot", "--network", "host", "--authfile", auth}
	if kind {
		args = append(args, "--tls-verify=false")
	}
	args = append(args, "-t", "acs-build-smoke:manual", dir)
	if e := a.command(ctx, args...); e != nil {
		return e
	}
	args = []string{"buildah", "--storage-driver", "vfs", "push", "--authfile", auth}
	if kind {
		args = append(args, "--tls-verify=false")
	}
	return a.command(ctx, append(args, "acs-build-smoke:manual", "docker://"+target)...)
}
