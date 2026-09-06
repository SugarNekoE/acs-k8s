# ACS operations reference

Start with the [deployment quick guide](../acs-deployment-guide.md).

## Configuration and credentials

The CLI uses `--repo`, the `--config` file's directory, or the nearest parent with
ACS config/deployment or sources, falling back to the current directory. Relative
settings resolve there. Image builds, CLI validation, the tooling shell and Kind
commands that build images require the source checkout. Use `--deployment FILE`
for another deployment profile.

The SOPS document contains `kubeconfig` (complete YAML as a string),
`forgejo.registration-token`, `forgejo.scaler-token`, and
`acr.address`, `acr.namespace`, `acr.username`, `acr.password`. Use a global
registration token and a separate administrator queue-reading PAT. The CLI
requires an explicit context and creates a temporary private kubeconfig.
SOPS ACR credentials are pull-only; publishing uses a separate interactive login.

## VPC image pulls

Merge these fields into local `acs-config.json`:

```json
{
  "registry": "crpi-1u9nmrh29ouue3ym.cn-hongkong.personal.cr.aliyuncs.com",
  "pull_registry": "crpi-1u9nmrh29ouue3ym-vpc.cn-hongkong.personal.cr.aliyuncs.com",
  "registry_namespace": "asnk-forge"
}
```

Publishing, login and digest queries use `registry`. Bundle generation replaces
that exact host in both registration and workflow images with `pull_registry`;
other registries, paths, tags, digests and source JSON remain unchanged. The pull
Secret authenticates the VPC hostname using the existing SOPS credentials.
Keep SOPS `acr.address` matching `registry` and `acr.namespace` matching
`registry_namespace`. An empty `pull_registry` defaults to `registry`; override
it with `ACS_PULL_REGISTRY` or `--pull-registry HOST` (no scheme/path).

Use the reachable VPC endpoint of the same ACR instance. Rebuild the bundle and
snapshots, refresh the pull Secret, then deploy; no image rebuild is needed.
If ESO owns that Secret, update its Docker auth hostname through ESO. Existing
Jobs retain their image references. This option does not configure VPC/DNS or
change Buildah's public base-image sources.

## Change pools

To reuse an image under another label, add an entry under `runners` in local
deployment JSON:

```json
"extra-go": {
  "image": "COPY_THE_EXISTING_GO_POOL_IMAGE_REFERENCE",
  "labels": ["acs-extra-go"],
  "maxReplicas": 10
}
```

Use unique lowercase pool names and bare labels. Omitted resources default to
2 CPU / 4 GiB; override `resources.requests` and `resources.limits` as needed.
Rebuild snapshots, validate and deploy; select `runs-on: acs-extra-go` in workflows.
For a new image family or additional tools, follow the [image guide](../images/README.md).

To remove a pool, stop its workflows, annotate its ScaledJob
`autoscaling.keda.sh/paused: "true"`, and drain active Jobs. Remove its JSON entry
and rebuild. Explicitly delete only its ScaledJob, TriggerAuthentication
`acs-runner-NAME`, and ConfigMap `acs-runner-NAME-config`, then validate/deploy.
Deployment does not prune resources. Keep shared Secrets, ServiceAccount, KEDA
and at least one pool; remove images only after all references and Jobs are gone.

## Scaling and cleanup

Forgejo's queue includes waiting and running tasks. KEDA's `default` strategy
subtracts all unfinished Kubernetes Jobs; the former `accurate` strategy could
launch extra runners for an already-running task. Pods use `one-job --wait`,
bounded by the active Job deadline. These changes need a new bundle/deployment,
but existing v13.1.0 images support the flag. Gradual rollout preserves active Jobs.

Forgejo removes ephemeral registrations after a reported final success/failure.
Kubernetes deletes terminal Jobs and their Pods according to
`ttlSecondsAfterFinished` (default `0`). Explicit old values such as `300` remain
in effect; existing Jobs retain their original TTL. A Pod still retrying reports
is not terminal. Crashes before a final report can leave orphaned Forgejo runners;
Kubernetes TTL does not delete those records.

## Checks and troubleshooting

```sh
acs status                   # Pool conditions, Jobs, Pod/init states and KEDA logs
acs watch
acs logs POD-NAME             # Continues if a container has no logs
acs validate cli              # Go tests, vet and strict errcheck
acs validate images           # Rootless builds and native checks
acs check image go ARCHIVE    # Check an existing runner-go-image archive
nix flake check              # Package checks, race tests and static analysis
```

A Job exists before registration; its `register` init container must succeed
before the workflow starts. Inspect its logs/events for missing credentials,
timeouts, permissions or image-pull errors. Temporarily retain failed Jobs with
`ttlSecondsAfterFinished: 300`. Check scaler errors for PAT/scope or DNS/TLS
problems, pending Pods for quotas, and restarts for OOM/throttling.

For live workflow smoke tests, use `runs-on: acs-go` with
`run: acs workflow test go`; substitute `nix` or `node24` as needed. The Buildah
test uses `acs-docker`, `acs workflow test docker`, workflow variables `ACR` and
`TARGET_IMAGE`, and push secrets `ACR_USERNAME`/`ACR_PASSWORD`. Create its target
repository first; the test requires Docker Hub access.

`--system aarch64-linux` selects ARM images; building/running them needs a native
ARM host or configured emulation. The Nix image supplies its own single-user Nix
tools. Workflow Pods have no Kubernetes token; Buildah does not provide Docker
service containers/actions.

Kind commands (`acs kind probe`, `test`, `network`, `diagnose`) use isolated
settings and never decrypt ACS credentials. Inspect `acs-kind-test-report.md`
before `acs kind cleanup --yes`, which deletes only owned fixtures. Full Kind
acceptance remains blocked by Service routing; offline tests are not live ACS
acceptance.
