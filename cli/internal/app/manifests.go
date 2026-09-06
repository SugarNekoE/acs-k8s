package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This is the pod's small registration adapter, not operator orchestration.
// It preserves the established runner invocation and private .runner permissions.
const registrationScript = `umask 077
mkdir -p /data/home /data/workspace
# Never enable shell tracing: the token must not reach logs.
# The reusable token volume is NOT mounted by the workflow container.
timeout 120 forgejo-runner --config /etc/acs-runner/config.yaml register \
  --no-interactive --ephemeral \
  --instance "$FORGEJO_URL" --name "$POD_NAME" \
  --token "$(cat /run/registration/token)"
test -s /data/.runner
`

type Manifests struct {
	Namespaces, Resources, ExternalSecrets []Obj
	KEDAValues                             Obj
	Plan                                   DeployPlan
}

func object(api, kind, name, namespace string, body Obj) Obj {
	metadata := Obj{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	out := Obj{"apiVersion": api, "kind": kind, "metadata": metadata}
	for k, v := range body {
		out[k] = v
	}
	return out
}
func objectsList(objects []Obj) Obj { return Obj{"apiVersion": "v1", "kind": "List", "items": objects} }
func restrictedSecurity() Obj {
	return Obj{"allowPrivilegeEscalation": false, "privileged": false, "capabilities": Obj{"drop": []string{"ALL"}}}
}
func baseMounts() []any {
	return []any{Obj{"name": "runner-data", "mountPath": "/data"}, Obj{"name": "runner-config", "mountPath": "/etc/acs-runner", "readOnly": true}}
}
func mergeObjects(base, override Obj) Obj {
	out := Obj{}
	for k, v := range base {
		if nested, ok := v.(map[string]any); ok {
			out[k] = mergeObjects(nested, nil)
		} else {
			out[k] = v
		}
	}
	for k, v := range override {
		if nested, ok := v.(map[string]any); ok {
			out[k] = mergeObjects(obj(out[k]), nested)
		} else {
			out[k] = v
		}
	}
	return out
}
func (c DeploymentConfig) render() (Manifests, error) {
	c.inheritDefaults()
	if err := c.validate(); err != nil {
		return Manifests{}, err
	}
	m := Manifests{Namespaces: []Obj{}, Resources: []Obj{}, ExternalSecrets: []Obj{}}
	m.Plan = DeployPlan{Version: 1, RunnerNamespace: c.Namespace, KEDANamespace: c.KEDA.Namespace, RequiredSecrets: []SecretRef{}, ImagePullSecrets: append([]string{}, c.ImagePullSecrets...), ExternalSecrets: []string{}, ScaledJobs: []string{}}
	if !c.Enable {
		return m, nil
	}
	for _, ns := range []string{c.Namespace, c.KEDA.Namespace} {
		o := object("v1", "Namespace", ns, "", nil)
		if ns == c.Namespace {
			metadata(o)["labels"] = Obj{"pod-security.kubernetes.io/enforce": "baseline"}
		}
		m.Namespaces = append(m.Namespaces, o)
	}
	m.Resources = append(m.Resources, object("v1", "ServiceAccount", "acs-forgejo-runner", c.Namespace, Obj{"automountServiceAccountToken": false}))
	seen := map[SecretRef]bool{}
	scalers := map[string]bool{}
	require := func(ref SecretRef) {
		if !seen[ref] {
			m.Plan.RequiredSecrets = append(m.Plan.RequiredSecrets, ref)
			seen[ref] = true
		}
	}
	for _, name := range sortedKeys(c.Runners) {
		r := c.Runners[name]
		resourceName := "acs-runner-" + name
		configName := resourceName + "-config"
		hostLabels := []string{}
		for _, label := range r.Labels {
			hostLabels = append(hostLabels, label+":host")
		}
		runnerConfig := Obj{"log": Obj{"level": "info"}, "runner": Obj{"file": "/data/.runner", "capacity": 1, "labels": hostLabels, "timeout": fmt.Sprintf("%ds", r.JobTimeoutSeconds), "shutdown_timeout": "25s", "insecure": false}, "cache": Obj{"enabled": false}, "host": Obj{"workdir_parent": "/data/workspace"}, "container": Obj{"docker_host": "-"}}
		configJSON, err := json.Marshal(runnerConfig)
		if err != nil {
			return m, err
		}
		m.Resources = append(m.Resources, object("v1", "ConfigMap", configName, c.Namespace, Obj{"data": Obj{"config.yaml": string(configJSON)}}),
			object("keda.sh/v1alpha1", "TriggerAuthentication", resourceName, c.Namespace, Obj{"spec": Obj{"secretTargetRef": []any{Obj{"parameter": "token", "name": r.ScalerSecret.Name, "key": r.ScalerSecret.Key}}}}))
		init := Obj{"name": "register", "image": r.Image, "imagePullPolicy": "IfNotPresent", "securityContext": restrictedSecurity(), "resources": Obj{"requests": Obj{"cpu": "100m", "memory": "128Mi"}, "limits": Obj{"cpu": "1", "memory": "256Mi"}}, "workingDir": "/data", "command": []string{"/bin/sh", "-ec"}, "args": []string{registrationScript},
			"env":          []any{Obj{"name": "FORGEJO_URL", "value": c.URL}, Obj{"name": "POD_NAME", "valueFrom": Obj{"fieldRef": Obj{"fieldPath": "metadata.name"}}}, Obj{"name": "HOME", "value": "/data/home"}},
			"volumeMounts": append(baseMounts(), Obj{"name": "registration-token", "mountPath": "/run/registration", "readOnly": true})}
		main := Obj{"name": "runner", "image": r.Image, "resources": r.Resources, "imagePullPolicy": "IfNotPresent", "securityContext": restrictedSecurity(), "workingDir": "/data", "command": []string{"forgejo-runner"}, "args": []string{"--config", "/etc/acs-runner/config.yaml", "one-job"}, "env": []any{Obj{"name": "HOME", "value": "/data/home"}}, "volumeMounts": baseMounts()}
		volumes := []any{Obj{"name": "runner-data", "emptyDir": Obj{}}, Obj{"name": "runner-config", "configMap": Obj{"name": configName}}, Obj{"name": "registration-token", "secret": Obj{"secretName": r.RegistrationSecret.Name, "defaultMode": 288, "items": []any{Obj{"key": r.RegistrationSecret.Key, "path": "token"}}}}}
		if r.Buildah {
			obj(main["securityContext"])["capabilities"] = Obj{"drop": []string{"ALL"}, "add": []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETGID", "SETUID", "SETFCAP", "SYS_CHROOT"}}
			main["volumeMounts"] = append(baseMounts(), Obj{"name": "buildah-storage", "mountPath": "/var/lib/containers"}, Obj{"name": "buildah-run", "mountPath": "/run/containers"})
			volumes = append(volumes, Obj{"name": "buildah-storage", "emptyDir": Obj{}}, Obj{"name": "buildah-run", "emptyDir": Obj{}})
		}
		pulls := []any{}
		for _, secret := range c.ImagePullSecrets {
			pulls = append(pulls, Obj{"name": secret})
		}
		pod := Obj{"serviceAccountName": "acs-forgejo-runner", "automountServiceAccountToken": false, "restartPolicy": "Never", "terminationGracePeriodSeconds": 30, "imagePullSecrets": pulls, "nodeSelector": Obj{"kubernetes.io/arch": r.Architecture}, "securityContext": Obj{"runAsUser": r.RunAsUser, "runAsGroup": r.RunAsUser, "fsGroup": r.RunAsUser, "seccompProfile": Obj{"type": "RuntimeDefault"}}, "initContainers": []any{init}, "containers": []any{main}, "volumes": volumes}
		labels := Obj{"alibabacloud.com/compute-class": c.Aliyun.ComputeClass, "alibabacloud.com/compute-qos": c.Aliyun.ComputeQos}
		for k, v := range r.PodLabels {
			labels[k] = v
		}
		labels["app.kubernetes.io/name"] = resourceName
		annotations := map[string]string{}
		for k, v := range c.Aliyun.PodAnnotations {
			annotations[k] = v
		}
		for k, v := range r.PodAnnotations {
			annotations[k] = v
		}
		trigger := Obj{"address": strings.TrimSuffix(c.URL, "/"), "labels": strings.Join(r.Labels, ","), "global": "false"}
		switch r.Scope.Type {
		case "global":
			trigger["global"] = "true"
		case "organization":
			trigger["org"] = r.Scope.Owner
		case "repository":
			trigger["owner"] = r.Scope.Owner
			trigger["repo"] = r.Scope.Repo
		}
		spec := Obj{"minReplicaCount": 0, "maxReplicaCount": r.MaxReplicas, "pollingInterval": r.PollingInterval, "successfulJobsHistoryLimit": 1, "failedJobsHistoryLimit": 3, "rollout": Obj{"strategy": "gradual"}, "scalingStrategy": Obj{"strategy": "accurate"},
			"jobTargetRef": Obj{"parallelism": 1, "completions": 1, "backoffLimit": 0, "activeDeadlineSeconds": r.ActiveDeadlineSeconds, "ttlSecondsAfterFinished": r.TTLSecondsAfterFinished, "template": Obj{"metadata": Obj{"labels": labels, "annotations": annotations}, "spec": pod}},
			"triggers":     []any{Obj{"type": "forgejo-runner", "metadata": trigger, "authenticationRef": Obj{"name": resourceName}}}}
		m.Resources = append(m.Resources, object("keda.sh/v1alpha1", "ScaledJob", resourceName, c.Namespace, Obj{"spec": spec}))
		require(*r.RegistrationSecret)
		require(*r.ScalerSecret)
		scalers[r.ScalerSecret.Name] = true
		m.Plan.ScaledJobs = append(m.Plan.ScaledJobs, resourceName)
	}
	for _, secret := range c.ImagePullSecrets {
		require(SecretRef{secret, ".dockerconfigjson"})
	}
	for _, name := range sortedKeys(c.ExternalSecrets) {
		s := c.ExternalSecrets[name]
		data := append([]ExternalData{}, s.Data...)
		m.ExternalSecrets = append(m.ExternalSecrets, object("external-secrets.io/v1", "ExternalSecret", name, c.Namespace, Obj{"spec": Obj{"refreshInterval": "1h", "secretStoreRef": s.StoreRef, "target": Obj{"name": name, "creationPolicy": "Owner", "template": Obj{"type": s.Type}}, "data": data}}))
		m.Plan.ExternalSecrets = append(m.Plan.ExternalSecrets, name)
	}
	defaultLabels := Obj{}
	for _, component := range []string{"keda", "metricsAdapter", "webhooks"} {
		defaultLabels[component] = Obj{"alibabacloud.com/compute-class": "general-purpose", "alibabacloud.com/compute-qos": "default"}
	}
	m.KEDAValues = mergeObjects(mergeObjects(Obj{"podLabels": defaultLabels}, c.KEDA.Values), Obj{
		"acsForgejoManaged": true, "watchNamespace": c.Namespace, "operator": Obj{"name": "keda-operator"}, "serviceAccount": Obj{"operator": Obj{"create": true, "name": "keda-operator"}},
		"rbac":        Obj{"create": true, "aggregateToDefaultRoles": false, "enabledCustomScaledRefKinds": false},
		"permissions": Obj{"operator": Obj{"restrict": Obj{"secret": false, "namesAllowList": sortedKeys(scalers), "allowAllServiceAccountTokenCreation": false, "serviceAccountTokenCreationRoles": []any{}}}}})
	return m, nil
}

func (c DeploymentConfig) kindConfig() DeploymentConfig {
	c.URL = "https://" + forgejoHost + ":3000"
	c.Namespace = runnerNS
	c.Scope = Scope{Type: "user"}
	c.ImagePullSecrets = []string{}
	c.ExternalSecrets = nil
	c.RegistrationSecret = SecretRef{"acs-e2e-registration", "token"}
	c.ScalerSecret = SecretRef{"acs-e2e-scaler", "token"}
	c.KEDA.Namespace = kedaNS
	c.KubeVersion = "1.35.0"
	volume := Obj{"name": "acs-e2e-ca", "configMap": Obj{"name": "acs-e2e-ca"}}
	mount := Obj{"name": "acs-e2e-ca", "mountPath": "/etc/acs-kind-ca", "readOnly": true}
	volumes := Obj{}
	for _, component := range []string{"keda", "metricsApiServer", "webhooks"} {
		volumes[component] = Obj{"extraVolumes": []any{volume}, "extraVolumeMounts": []any{mount}}
	}
	c.KEDA.Values = mergeObjects(c.KEDA.Values, Obj{"env": []any{Obj{"name": "SSL_CERT_FILE", "value": "/etc/acs-kind-ca/ca-bundle.crt"}}, "volumes": volumes})
	copied := map[string]RunnerConfig{}
	for name, r := range c.Runners {
		r.Image = "localhost:30050/forgejo-runner-" + name + ":" + tag
		r.Labels = []string{"acs-kind-" + name}
		r.Scope = &c.Scope
		r.RegistrationSecret = &c.RegistrationSecret
		r.ScalerSecret = &c.ScalerSecret
		r.MaxReplicas = 1
		r.Architecture = "amd64"
		r.PollingInterval = 5
		r.JobTimeoutSeconds = 600
		r.ActiveDeadlineSeconds = 900
		r.TTLSecondsAfterFinished = 120
		copied[name] = r
	}
	c.Runners = copied
	return c
}
