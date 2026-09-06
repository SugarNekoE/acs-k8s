package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Diagnostics can contain remote API errors. Capture them before printing so
// credentials echoed by a controller or upstream endpoint can be redacted.
func diagnosticRedactor(c Credentials) (*strings.Replacer, error) {
	values := []string{c.Kubeconfig, c.Forgejo.Registration, c.Forgejo.Scaler, c.ACR.Username, c.ACR.Password}
	if c.ACR.Username != "" && c.ACR.Password != "" {
		values = append(values, c.ACR.Username+":"+c.ACR.Password)
	}
	var patterns []string
	for _, value := range values {
		if value == "" {
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, value, string(encoded[1:len(encoded)-1]), base64.StdEncoding.EncodeToString([]byte(value)))
	}
	// Redact combined credentials before their individual parts.
	sort.Slice(patterns, func(i, j int) bool { return len(patterns[i]) > len(patterns[j]) })
	var pairs []string
	for _, pattern := range patterns {
		pairs = append(pairs, pattern, "[REDACTED]")
	}
	return strings.NewReplacer(pairs...), nil
}

func (a *App) clusterStatus(ctx context.Context, s *Session) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	redactor, err := diagnosticRedactor(s.Credentials)
	if err != nil {
		return err
	}
	checks := []struct {
		title string
		args  []string
	}{
		{"KEDA pods", []string{"-n", a.Config.KEDANamespace, "get", "pods", "-o", "wide"}},
		{"Runner ScaledJobs", []string{"-n", a.Config.RunnerNamespace, "get", "scaledjobs"}},
		{"ScaledJob conditions and events", []string{"-n", a.Config.RunnerNamespace, "describe", "scaledjobs"}},
		{"KEDA operator logs (last 10 minutes, up to 100 lines)", []string{"-n", a.Config.KEDANamespace, "logs", "deployment/keda-operator", "--since=10m", "--tail=100", "--pod-running-timeout=10s"}},
	}
	var failures []error
	for _, check := range checks {
		a.message("%s:", check.title)
		output, err := a.kubectl(ctx, s, true, true, nil, append([]string{"--request-timeout=10s"}, check.args...)...)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", check.title, err))
			continue
		}
		if len(output) > 0 {
			a.message("%s", redactor.Replace(strings.TrimRight(string(output), "\n")))
		}
	}
	return errors.Join(failures...)
}
