package app

import (
	"bytes"
	"encoding/base64"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStatusDiagnosesAllPoolsAndRedactsCredentials(t *testing.T) {
	a, r := testApp(t)
	var output bytes.Buffer
	a.Out = &output
	c := credentials()
	r.run = func(in Invocation) ([]byte, error) {
		if !in.Sensitive || !in.Capture {
			t.Fatal("diagnostics were not captured privately")
		}
		if in.Env["KUBECONFIG"] != "private-config" || in.Args[2] != "test-acs" {
			t.Fatal("lost cluster session")
		}
		return []byte("acs-runner-docker Ready=False ScaledJobCheckFailed: " + c.Forgejo.Scaler + " " + base64.StdEncoding.EncodeToString([]byte(c.ACR.Password))), nil
	}
	if err := a.clusterAction(t.Context(), &Session{Credentials: c, Env: map[string]string{"KUBECONFIG": "private-config"}}, []string{"status"}); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 4 {
		t.Fatalf("diagnostic calls: %d", len(r.calls))
	}
	if !slices.Contains(r.calls[2].Args, "describe") || r.calls[2].Args[len(r.calls[2].Args)-1] != "scaledjobs" {
		t.Fatal("status does not describe all pools")
	}
	if !slices.Contains(r.calls[3].Args, "deployment/keda-operator") {
		t.Fatal("missing operator logs")
	}
	text := output.String()
	if !strings.Contains(text, "ScaledJobCheckFailed") || !strings.Contains(text, "[REDACTED]") {
		t.Fatal("lost diagnostic evidence")
	}
	if strings.Contains(text, c.Forgejo.Scaler) || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(c.ACR.Password))) {
		t.Fatal("diagnostics leaked credentials")
	}
}

func TestStatusContinuesAfterDiagnosticFailure(t *testing.T) {
	a, r := testApp(t)
	r.run = func(in Invocation) ([]byte, error) {
		if len(r.calls) == 1 {
			return nil, errors.New("synthetic-private-error")
		}
		return []byte("remaining evidence"), nil
	}
	err := a.clusterStatus(t.Context(), &Session{})
	if err == nil || !strings.Contains(err.Error(), "KEDA pods") || strings.Contains(err.Error(), "synthetic-private-error") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.calls) != 4 {
		t.Fatal("stopped collecting evidence after one failure")
	}
}

func TestDeployReadinessFailureReportsDiagnosticsAndStillFails(t *testing.T) {
	a, r := testApp(t)
	var output bytes.Buffer
	a.Out = &output
	p := DeployPlan{Version: 1, RunnerNamespace: a.Config.RunnerNamespace, KEDANamespace: a.Config.KEDANamespace, RequiredSecrets: []SecretRef{{"scaler", "token"}}, ScaledJobs: []string{"acs-runner-docker", "acs-runner-go"}}
	mustWrite(t, filepath.Join(a.Config.Bundle, "acs-forgejo-runners.yaml"), "{}")
	mustWrite(t, filepath.Join(a.Config.Bundle, "acs-deploy-plan.json"), string(encoded(t, p)))
	readinessErr := errors.New("readiness timeout")
	r.run = func(in Invocation) ([]byte, error) {
		if slices.Contains(in.Args, "secret") {
			return []byte("present"), nil
		}
		if in.Args[0] == "helm" && slices.Contains(in.Args, "list") {
			return []byte("[]"), nil
		}
		if slices.Contains(in.Args, "scaledjob/acs-runner-docker") {
			return nil, readinessErr
		}
		return []byte("synthetic scaler failure detail"), nil
	}
	err := a.deploy(t.Context(), &Session{})
	if !errors.Is(err, readinessErr) || !strings.Contains(err.Error(), "acs-runner-docker") {
		t.Fatalf("readiness failure lost: %v", err)
	}
	if !strings.Contains(output.String(), "synthetic scaler failure detail") {
		t.Fatal("missing diagnostics")
	}
	for _, call := range r.calls {
		if slices.Contains(call.Args, "scaledjob/acs-runner-go") {
			t.Fatal("continued deployment after readiness failure")
		}
	}
	if !slices.Contains(r.calls[len(r.calls)-1].Args, "deployment/keda-operator") {
		t.Fatal("did not gather operator logs")
	}
}
