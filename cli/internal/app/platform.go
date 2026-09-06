package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

type offlineSchemaLoader struct{}

func (offlineSchemaLoader) Load(string) (any, error) {
	return nil, errors.New("external JSON Schema references are not allowed")
}

func canonicalObjects(objects []Obj) ([]Obj, error) {
	b, err := json.Marshal(objects)
	if err != nil {
		return nil, err
	}
	var out []Obj
	err = json.Unmarshal(b, &out)
	return out, err
}
func validateSchemas(rendered, resources []Obj) error {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft7)
	compiler.UseLoader(offlineSchemaLoader{})
	schemas := map[string]*jsonschema.Schema{}
	for _, crd := range rendered {
		if crd["kind"] != "CustomResourceDefinition" {
			continue
		}
		kind := str(at(crd, "spec", "names", "kind"))
		group := str(at(crd, "spec", "group"))
		for _, v := range arr(at(crd, "spec", "versions")) {
			version := obj(v)
			if version["served"] != true {
				continue
			}
			key := group + "/" + str(version["name"]) + "/" + kind
			uri := "https://acs.invalid/schemas/" + key
			schema, err := jsonValue(at(version, "schema", "openAPIV3Schema"))
			if err != nil {
				return err
			}
			if schema == nil {
				return fmt.Errorf("missing schema for %s", key)
			}
			if err = compiler.AddResource(uri, schema); err != nil {
				return err
			}
			compiled, err := compiler.Compile(uri)
			if err != nil {
				return err
			}
			schemas[key] = compiled
		}
	}
	for _, resource := range resources {
		if !strings.HasPrefix(str(resource["apiVersion"]), "keda.sh/") {
			continue
		}
		key := str(resource["apiVersion"]) + "/" + str(resource["kind"])
		schema := schemas[key]
		if schema == nil {
			return fmt.Errorf("KEDA chart is missing schema %s", key)
		}
		value, err := jsonValue(resource)
		if err != nil {
			return err
		}
		if err = schema.Validate(value); err != nil {
			return fmt.Errorf("%s/%s: %w", resource["kind"], at(resource, "metadata", "name"), err)
		}
	}
	return nil
}

func validateRunnerSecurity(resources []Obj) error {
	for _, o := range resources {
		switch o["kind"] {
		case "Secret", "RoleBinding", "ClusterRoleBinding", "Deployment":
			return fmt.Errorf("unexpected runner resource %s", o["kind"])
		case "ServiceAccount":
			if o["automountServiceAccountToken"] != false {
				return errors.New("runner ServiceAccount must not automount API tokens")
			}
		case "ScaledJob":
			spec := obj(o["spec"])
			pod := obj(at(spec, "jobTargetRef", "template", "spec"))
			if pod["automountServiceAccountToken"] != false || pod["hostNetwork"] == true || pod["hostPID"] == true {
				return errors.New("runner pod violates API-token or host isolation")
			}
			if len(arr(pod["containers"])) != 1 || len(arr(pod["initContainers"])) != 1 {
				return errors.New("runner requires one workflow and one registration container")
			}
			main, init := obj(arr(pod["containers"])[0]), obj(arr(pod["initContainers"])[0])
			if !reflect.DeepEqual(main["command"], []any{"forgejo-runner"}) || !reflect.DeepEqual(main["args"], []any{"--config", "/etc/acs-runner/config.yaml", "one-job"}) {
				return errors.New("runner must execute one-job")
			}
			if main["image"] != init["image"] {
				return errors.New("registration/workflow images differ")
			}
			for _, container := range []Obj{main, init} {
				security := obj(container["securityContext"])
				if security["privileged"] != false || security["allowPrivilegeEscalation"] != false || !reflect.DeepEqual(at(security, "capabilities", "drop"), []any{"ALL"}) {
					return errors.New("runner capabilities or privilege boundary violated")
				}
				for _, cap := range arr(at(security, "capabilities", "add")) {
					if !slices.Contains([]string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "SETGID", "SETUID", "SETFCAP", "SYS_CHROOT"}, str(cap)) {
						return errors.New("unsupported runner capability")
					}
				}
			}
			if len(arr(at(init, "securityContext", "capabilities", "add"))) != 0 {
				return errors.New("registration init must not gain capabilities")
			}
			for _, mount := range arr(main["volumeMounts"]) {
				if at(obj(mount), "name") == "registration-token" {
					return errors.New("reusable token mounted into workflow container")
				}
			}
			for _, env := range arr(main["env"]) {
				if obj(env)["valueFrom"] != nil {
					return errors.New("unexpected credential source in workflow environment")
				}
			}
			for _, v := range arr(pod["volumes"]) {
				if obj(v)["hostPath"] != nil || obj(v)["persistentVolumeClaim"] != nil {
					return errors.New("runner storage must remain ephemeral")
				}
			}
		}
	}
	return nil
}

func validateBundle(c DeploymentConfig, dir string) error {
	expected, err := c.render()
	if err != nil {
		return err
	}
	files, err := expected.files()
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(files) {
		actual, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		var av, ev any
		if err = yaml.Unmarshal(actual, &av); err != nil {
			return err
		}
		if err = yaml.Unmarshal(files[name], &ev); err != nil {
			return err
		}
		if !reflect.DeepEqual(av, ev) {
			return fmt.Errorf("bundle %s does not match deployment configuration", name)
		}
	}
	resources, err := readObjects(filepath.Join(dir, "acs-forgejo-runners.yaml"))
	if err != nil {
		return err
	}
	resources, err = canonicalObjects(resources)
	if err != nil {
		return err
	}
	if err = validateRunnerSecurity(resources); err != nil {
		return err
	}
	rendered, err := readObjects(filepath.Join(dir, "acs-keda-rendered.yaml"))
	if err != nil {
		return err
	}
	if err = validateSchemas(rendered, resources); err != nil {
		return err
	}
	hasOperator, canCreateJobs := false, false
	for _, o := range rendered {
		switch o["kind"] {
		case "Deployment":
			if at(o, "metadata", "name") == "keda-operator" {
				hasOperator = true
			}
		case "Role", "ClusterRole":
			for _, r := range arr(o["rules"]) {
				rule := obj(r)
				hasJobs, hasCreate := false, false
				for _, v := range arr(rule["resources"]) {
					hasJobs = hasJobs || v == "jobs"
				}
				for _, v := range arr(rule["verbs"]) {
					hasCreate = hasCreate || v == "create"
				}
				canCreateJobs = canCreateJobs || (hasJobs && hasCreate)
			}
		case "RoleBinding", "ClusterRoleBinding":
			for _, s := range arr(o["subjects"]) {
				if at(obj(s), "name") == "acs-forgejo-runner" {
					return errors.New("KEDA grants RBAC to the untrusted runner ServiceAccount")
				}
			}
		}
	}
	if !hasOperator || !canCreateJobs {
		return errors.New("KEDA chart lacks its operator or Job-creation RBAC")
	}
	return nil
}
