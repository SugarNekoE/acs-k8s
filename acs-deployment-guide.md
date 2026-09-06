# ACS deployment quick guide

Run from the repository root as your normal user. Install Buildah, Skopeo, Helm,
kubectl and SOPS; image builds need working rootless user namespaces. Deployment settings:
`acs-deployment.json`, created from the [example](acs-deployment.example.json).
Source map: [project index](PROJECT_INDEX.md).

## Setup

`nix build .#acs-cli` packages the CLI as `result/bin/acs`; `nix run .#acs-cli -- --help`
runs it directly. `nix develop` (or `acs shell`) provides the CLI and required tools.
Use `nix flake check` for package tests, race tests and Go analysis. The Forgejo
workflow builds the default package and uploads it to the `acs-k8s` Cachix cache.

```bash
# Optional tooling shell on this NixOS workstation:
nix develop
# First setup only; preserve an existing local config.
cp acs-config.example.json acs-config.json
cp acs-deployment.example.json acs-deployment.json
acs config show
acs connect
```

Set `sops_file` to your existing encrypted credential document (absolute or
relative to this root); there is no default secret location.
Match local registry settings to the deployment image references, and `context`
to an exact SOPS kubeconfig context. Both local JSON files are ignored by Git and
read on each command. The deployment example leaves all runner images empty;
fill them before building a bundle. Keep the example files versioned.
Use `--config FILE` for another profile or `--repo PATH` outside this checkout.

The SOPS document contains `kubeconfig` (complete YAML as a string),
`forgejo.registration-token`, `forgejo.scaler-token`, and
`acr.address`, `acr.namespace`, `acr.username`, `acr.password`.
Scope is **global**: use a global registration token and a separate administrator
queue-reading PAT. SOPS ACR credentials are pull-only and must match local JSON.
The CLI uses an explicit context and a temporary private kubeconfig.
Go owns deployment generation, image builds/checks and Kind orchestration.
Buildah builds the images; workstation Nix is only an optional CLI installation/tool source. Use `--deployment FILE` for another deployment JSON.

## Deploy and update

Create ACR repositories `forgejo-runner-{nix,go,node24,docker}`. Publish using
separate push credentials:

```bash
acs registry login
acs images publish           # Append pool names for a subset; "build" skips push
acs registry digests
```

Set every `runners.<pool>.image` to a full image reference, preferably
`REGISTRY/NAMESPACE/forgejo-runner-POOL@sha256:DIGEST`, then:

```bash
acs bundle build
acs bundle inspect
acs bundle snapshots
acs validate platform
acs namespaces
acs secrets apply
acs deploy
acs status
```

To check an already built pool, use `acs check image go PATH-TO-ARCHIVE`.
Image archives keep the `runner-POOL-image` output names. Rebuild/publish all four
once when migrating from the former Nix images, and update deployment digests.

For configuration-only updates, repeat bundle build/inspect/snapshots, validation
and deployment; rebuild images only when their contents change. Deployment checks
Secrets, Helm ownership, server validation and readiness. Inspect failures before retrying.

Outputs use `~/.local/state/acs-forgejo` or `$XDG_STATE_HOME/acs-forgejo`.
Keep YAML snapshots versioned. Bundle generation uses the embedded pinned KEDA
chart and local Helm; it does not evaluate Nix or contact a cluster.
For existing ESO, use the [template](acs-external-secret-template.yaml) and skip
manual writes to ESO-owned Secrets.

## Pools and containers

| Pool / workflow label | CPU / memory | Max Jobs |
| --- | --- | --- |
| `nix` / `acs-nix` | 8 / 16 GiB | 10 |
| `go` / `acs-go` | 2 / 4 GiB | 10 |
| `node24` / `acs-node24` | 4 / 8 GiB | 10 |
| `docker` / `acs-docker` | 4 / 8 GiB | 10 |

Pools use amd64 and 30 GiB temporary storage, scaling to zero when idle.
`--system aarch64-linux` selects ARM images; foreign-architecture RUN steps require
preconfigured emulation or a native ARM builder. KEDA keeps
three components running, each at one replica and 250m CPU / 512Mi requests/limits.

**Add tools:** edit the common or pool-specific stage in the
[Containerfile](images/Containerfile), then rebuild/publish that image, update its
deployment digest and redeploy. Base images are digest-pinned. The CLI copies only
Go build inputs and image recipes into its temporary build context; local JSON
and SOPS documents stay outside it.

**Add a pool:** add an entry under `runners` in `acs-deployment.json`:

```json
"extra-go": {
  "image": "COPY_THE_EXISTING_GO_POOL_IMAGE_REFERENCE",
  "labels": ["acs-extra-go"],
  "maxReplicas": 10
}
```

Use unique lowercase pool names and bare labels. Omitted settings inherit defaults
(2 CPU / 4 GiB); specify `resources.requests` and `resources.limits` to change them.
Rebuild snapshots, validate and deploy; workflows select `runs-on: acs-extra-go`.
No Nix module or fixed-pool test fixture is needed for another pool. For a new
image family, follow [Add a runner image](images/README.md): Containerfile target,
CLI integration, native checks, publishing and deployment.

**Remove a pool:** stop its workflows; in the ACS console annotate its ScaledJob
`autoscaling.keda.sh/paused: "true"` and drain active Jobs. Remove its JSON entry
and rebuild. In `forgejo-runners`, delete that pool's ScaledJob and
TriggerAuthentication `acs-runner-NAME`, plus ConfigMap `acs-runner-NAME-config`;
validate and deploy the remaining bundle. Deployment does not prune live resources.
Keep shared Secrets, ServiceAccount, KEDA and at least one pool. Remove an image
only after all references and Jobs are gone.

## Test and troubleshoot

In a manual Forgejo workflow, use `runs-on: acs-go` with a step
`run: acs workflow test go`; substitute `nix` or `node24` for the other pools.
For `acs-docker`, run `acs workflow test docker` with workflow variables `ACR`,
`TARGET_IMAGE` and push secrets `ACR_USERNAME`/`ACR_PASSWORD`. Create the target
repository first. No checkout is needed; the Buildah test requires Docker Hub.

```bash
acs status                   # All pool conditions/events and recent KEDA logs
acs watch                    # Jobs/pods; Ctrl+C to stop
acs logs POD-NAME
acs validate cli             # Go tests, vet and errcheck
acs validate images          # Buildah builds; Go runs isolated container checks
```

Use `nix run .#acs-cli -- status` for the latest CLI. Check scaler failures for
PAT/scope/Secret or DNS/TLS errors; image-pull failures for registry permissions;
pending pods for quotas; restarts for OOM/throttling. Save logs before Job TTL cleanup.
The Nix pool includes Nix/devenv/cache tools and uses single-user Nix without
sandboxing; no host Nix is needed to build it. Buildah supports Dockerfile builds, not Docker service
containers/actions. Workflow pods have no Kubernetes token. Offline checks do not
prove live scaling or ACS Buildah compatibility.

Kind bundles are generated by Go from the deployment JSON with isolated overrides.
Run `acs kind probe`, `test`, `network`, `diagnose`; after inspecting the
`acs-kind-test-report.md`, `acs kind cleanup --yes` removes only owned
fixtures from the existing `kind-acs-test` cluster. Service routing remains a test blocker.
