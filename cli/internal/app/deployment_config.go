package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

type Scope struct {
	Type  string `json:"type"`
	Owner string `json:"owner,omitempty"`
	Repo  string `json:"repo,omitempty"`
}
type Resources struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}
type RunnerConfig struct {
	Image                   string            `json:"image"`
	Labels                  []string          `json:"labels"`
	Scope                   *Scope            `json:"scope,omitempty"`
	RegistrationSecret      *SecretRef        `json:"registrationSecret,omitempty"`
	ScalerSecret            *SecretRef        `json:"scalerSecret,omitempty"`
	MaxReplicas             int               `json:"maxReplicas"`
	PollingInterval         int               `json:"pollingInterval"`
	ActiveDeadlineSeconds   int               `json:"activeDeadlineSeconds"`
	JobTimeoutSeconds       int               `json:"jobTimeoutSeconds"`
	TTLSecondsAfterFinished int               `json:"ttlSecondsAfterFinished"`
	Resources               Resources         `json:"resources"`
	Architecture            string            `json:"architecture"`
	Buildah                 bool              `json:"buildah"`
	RunAsUser               int               `json:"runAsUser"`
	PodLabels               map[string]string `json:"podLabels,omitempty"`
	PodAnnotations          map[string]string `json:"podAnnotations,omitempty"`
}
type StoreRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}
type RemoteRef struct {
	Key      string  `json:"key"`
	Property *string `json:"property,omitempty"`
}
type ExternalData struct {
	SecretKey string    `json:"secretKey"`
	RemoteRef RemoteRef `json:"remoteRef"`
}
type ExternalSecret struct {
	StoreRef StoreRef       `json:"storeRef"`
	Type     string         `json:"type"`
	Data     []ExternalData `json:"data"`
}
type KEDAConfig struct {
	Namespace string `json:"namespace"`
	Values    Obj    `json:"values,omitempty"`
}
type AliyunConfig struct {
	ComputeClass   string            `json:"computeClass"`
	ComputeQos     string            `json:"computeQos"`
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
}
type DeploymentConfig struct {
	Version            int                       `json:"version"`
	Enable             bool                      `json:"enable"`
	URL                string                    `json:"url"`
	Namespace          string                    `json:"namespace"`
	Scope              Scope                     `json:"scope"`
	RegistrationSecret SecretRef                 `json:"registrationSecret"`
	ScalerSecret       SecretRef                 `json:"scalerSecret"`
	ImagePullSecrets   []string                  `json:"imagePullSecrets"`
	Runners            map[string]RunnerConfig   `json:"runners"`
	ExternalSecrets    map[string]ExternalSecret `json:"externalSecrets,omitempty"`
	KEDA               KEDAConfig                `json:"keda"`
	Aliyun             AliyunConfig              `json:"aliyun"`
	KubeVersion        string                    `json:"kubeVersion,omitempty"`
}

func strictJSON(b []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("expected one JSON document")
	}
	return nil
}
func runnerDefaults() RunnerConfig {
	return RunnerConfig{MaxReplicas: 20, PollingInterval: 10, ActiveDeadlineSeconds: 14400, JobTimeoutSeconds: 10800, TTLSecondsAfterFinished: 300, Architecture: "amd64", RunAsUser: 1000,
		Resources: Resources{Requests: map[string]string{"cpu": "2", "memory": "4Gi", "ephemeral-storage": "30Gi"}, Limits: map[string]string{"cpu": "2", "memory": "4Gi", "ephemeral-storage": "30Gi"}}}
}
func (r *RunnerConfig) UnmarshalJSON(b []byte) error {
	type plain RunnerConfig
	v := plain(runnerDefaults())
	if err := strictJSON(b, &v); err != nil {
		return err
	}
	*r = RunnerConfig(v)
	return nil
}
func (s *Scope) UnmarshalJSON(b []byte) error {
	type plain Scope
	v := plain{Type: "user"}
	if err := strictJSON(b, &v); err != nil {
		return err
	}
	*s = Scope(v)
	return nil
}
func (s *SecretRef) UnmarshalJSON(b []byte) error {
	type plain SecretRef
	v := plain{Key: "token"}
	if err := strictJSON(b, &v); err != nil {
		return err
	}
	*s = SecretRef(v)
	return nil
}
func (s *ExternalSecret) UnmarshalJSON(b []byte) error {
	type plain ExternalSecret
	v := plain{Type: "Opaque", StoreRef: StoreRef{Kind: "ClusterSecretStore"}}
	if err := strictJSON(b, &v); err != nil {
		return err
	}
	*s = ExternalSecret(v)
	return nil
}
func deploymentDefaults() DeploymentConfig {
	return DeploymentConfig{Version: 1, Enable: true, Namespace: "forgejo-runners", Scope: Scope{Type: "user"}, RegistrationSecret: SecretRef{"acs-forgejo-registration", "token"}, ScalerSecret: SecretRef{"acs-forgejo-scaler", "token"}, ImagePullSecrets: []string{}, Runners: map[string]RunnerConfig{}, KEDA: KEDAConfig{Namespace: "keda", Values: Obj{}}, Aliyun: AliyunConfig{ComputeClass: "general-purpose", ComputeQos: "default"}}
}
func loadDeployment(path string) (DeploymentConfig, error) {
	c := deploymentDefaults()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read deployment configuration: %w", err)
	}
	if err = strictJSON(b, &c); err != nil {
		return c, fmt.Errorf("invalid deployment JSON: %w", err)
	}
	c.inheritDefaults()
	return c, c.validate()
}
func (c *DeploymentConfig) inheritDefaults() {
	for name, r := range c.Runners {
		if r.Scope == nil {
			v := c.Scope
			r.Scope = &v
		}
		if r.RegistrationSecret == nil {
			v := c.RegistrationSecret
			r.RegistrationSecret = &v
		}
		if r.ScalerSecret == nil {
			v := c.ScalerSecret
			r.ScalerSecret = &v
		}
		c.Runners[name] = r
	}
}

var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var workflowLabelRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$`)
var secretKeyRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
var scopeNameRE = regexp.MustCompile(`^[a-zA-Z0-9_.-]*$`)
var quantityRE = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)(?:[eE][+-]?[0-9]+|[numkKMGTPE]|[KMGTPE]i)?$`)

func dnsLabel(s string) bool { return len(s) <= 63 && dnsLabelRE.MatchString(s) }
func resourceName(s string) bool {
	if len(s) > 253 || s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !dnsLabel(part) {
			return false
		}
	}
	return true
}
func (s Scope) valid() bool {
	if !scopeNameRE.MatchString(s.Owner) || !scopeNameRE.MatchString(s.Repo) {
		return false
	}
	switch s.Type {
	case "repository":
		return s.Owner != "" && s.Repo != ""
	case "organization":
		return s.Owner != "" && s.Repo == ""
	case "user", "global":
		return s.Owner == "" && s.Repo == ""
	}
	return false
}
func validSecret(s SecretRef) bool { return resourceName(s.Name) && secretKeyRE.MatchString(s.Key) }
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func (c DeploymentConfig) validate() error {
	var failures []error
	check := func(ok bool, message string) {
		if !ok {
			failures = append(failures, errors.New(message))
		}
	}
	check(c.Version == 1, "unsupported deployment configuration version")
	u, err := url.Parse(c.URL)
	check(err == nil && u != nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !strings.ContainsAny(c.URL, " \t\r\n"), "Forgejo URL must use HTTPS without credentials, query or fragment")
	check(dnsLabel(c.Namespace) && dnsLabel(c.KEDA.Namespace), "runner and KEDA namespaces must be DNS labels")
	check(c.Scope.valid(), "invalid default Forgejo scope")
	check(validSecret(c.RegistrationSecret) && validSecret(c.ScalerSecret), "invalid default Secret reference")
	check(slices.Contains([]string{"general-purpose", "performance", "gpu", "gpu-hpn"}, c.Aliyun.ComputeClass), "invalid ACS compute class")
	check(slices.Contains([]string{"default", "best-effort"}, c.Aliyun.ComputeQos), "invalid ACS QoS")
	if c.Enable {
		check(len(c.Runners) > 0, "at least one runner pool is required")
		check(c.Namespace != c.KEDA.Namespace, "KEDA and runners need separate namespaces")
		check(c.Aliyun.ComputeClass != "gpu-hpn" || c.Aliyun.ComputeQos == "default", "ACS gpu-hpn requires default QoS")
	}
	for _, name := range c.ImagePullSecrets {
		check(resourceName(name), "invalid image-pull Secret name")
	}
	allLabels := map[string]bool{}
	registrationNames := map[string]bool{}
	scalerNames := map[string]bool{}
	for _, name := range sortedKeys(c.Runners) {
		r := c.Runners[name]
		prefix := "pool " + name + ": "
		check(dnsLabel(name) && len(name) <= 30, prefix+"name must be a DNS label of at most 30 characters")
		check(strings.TrimSpace(r.Image) != "" && !strings.ContainsAny(r.Image, " \t\r\n"), prefix+"image is required and cannot contain whitespace")
		check(len(r.Labels) > 0, prefix+"labels must not be empty")
		for _, label := range r.Labels {
			check(workflowLabelRE.MatchString(label), prefix+"invalid bare workflow label")
			if c.Enable {
				check(!allLabels[label], prefix+"labels must be unique across pools")
			}
			allLabels[label] = true
		}
		check(r.Scope != nil && r.Scope.valid(), prefix+"inconsistent scope fields")
		check(r.RegistrationSecret != nil && validSecret(*r.RegistrationSecret), prefix+"invalid registration Secret")
		check(r.ScalerSecret != nil && validSecret(*r.ScalerSecret), prefix+"invalid scaler Secret")
		if r.RegistrationSecret != nil {
			registrationNames[r.RegistrationSecret.Name] = true
		}
		if r.ScalerSecret != nil {
			scalerNames[r.ScalerSecret.Name] = true
		}
		check(r.MaxReplicas > 0 && r.PollingInterval > 0 && r.JobTimeoutSeconds > 0 && r.ActiveDeadlineSeconds > 0 && r.TTLSecondsAfterFinished >= 0, prefix+"replicas and timeouts must be positive; TTL must be nonnegative")
		check(r.ActiveDeadlineSeconds > r.JobTimeoutSeconds && r.ActiveDeadlineSeconds-r.JobTimeoutSeconds > 120, prefix+"deadline must allow registration plus workflow timeout")
		check(slices.Contains([]string{"amd64", "arm64"}, r.Architecture), prefix+"unsupported architecture")
		check(r.RunAsUser >= 0 && (!r.Buildah || r.RunAsUser == 0), prefix+"Buildah requires UID 0; UID must be nonnegative")
		for _, quantities := range []map[string]string{r.Resources.Requests, r.Resources.Limits} {
			for _, key := range []string{"cpu", "memory"} {
				check(quantityRE.MatchString(quantities[key]), prefix+"invalid or missing "+key+" quantity")
			}
			for _, value := range quantities {
				check(quantityRE.MatchString(value), prefix+"invalid resource quantity")
			}
		}
	}
	if c.Enable {
		for name := range registrationNames {
			check(!scalerNames[name], "registration and scaler Secret names must be disjoint across all pools")
		}
	}
	for _, name := range sortedKeys(c.ExternalSecrets) {
		s := c.ExternalSecrets[name]
		check(resourceName(name), "invalid ExternalSecret name")
		check(dnsLabel(s.StoreRef.Name) && slices.Contains([]string{"SecretStore", "ClusterSecretStore"}, s.StoreRef.Kind), "invalid ExternalSecret store reference")
		check(slices.Contains([]string{"Opaque", "kubernetes.io/dockerconfigjson"}, s.Type), "invalid ExternalSecret target type")
		check(s.Data != nil, "ExternalSecret data list is required")
		for _, item := range s.Data {
			check(secretKeyRE.MatchString(item.SecretKey) && item.RemoteRef.Key != "", "invalid ExternalSecret data mapping")
		}
	}
	return errors.Join(failures...)
}
