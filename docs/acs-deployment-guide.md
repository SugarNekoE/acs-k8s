# ACS deployment quick guide

## Setup

From this checkout, `nix develop` supplies the CLI and tools. Alternatively,
install `acs`, Buildah, Skopeo, Helm, kubectl and SOPS. Build images as your normal
user with working rootless Buildah. `nix build .#acs-cli` produces `result/bin/acs`.

Create local `acs-config.json` and `acs-deployment.json` from the
[operator](acs-config.example.json) and [deployment](acs-deployment.example.json)
examples; preserve existing files. Both local files are ignored by Git.

Set these operator fields:

- `context`: the exact cluster context in your SOPS kubeconfig.
- `sops_file`: your existing encrypted credential document.
- `registry` and `registry_namespace`: the public publishing destination.
- `pull_registry`: the same ACR instance's VPC hostname for ACS pulls; omit to use `registry`.

Use global Forgejo scope, separate registration/scaler tokens, and separate ACR
push/pull credentials. See [credentials and VPC setup](docs/operations.md).
Outside this checkout, use `--config FILE`; source commands need `--repo PATH`.

## Deploy and update

If images need building, create ACR repositories `forgejo-runner-{nix,go,node24,docker}`:

```sh
acs registry login
acs images publish           # Append pool names for a subset
acs registry digests
```

Fill each `runners.<pool>.image` in local deployment JSON with a full image
reference, preferably `REGISTRY/NAMESPACE/forgejo-runner-POOL@sha256:DIGEST`.
The example leaves images empty. Bundle generation substitutes `pull_registry`
for matching public hosts without changing tags or digests.

```sh
acs config show
acs connect
acs bundle build
acs bundle inspect
acs bundle snapshots
acs validate platform
acs namespaces
acs secrets apply
acs deploy
acs status
```

For configuration changes, repeat bundle generation, validation and deployment;
rebuild images only when their contents change. Changing `pull_registry` also
requires refreshing the pull Secret. For ESO-managed Secrets, use the
[ESO template](acs-external-secret-template.yaml) instead of `acs secrets apply`.
Outputs default to `~/.local/state/acs-forgejo` (or `$XDG_STATE_HOME/acs-forgejo`).

## Pools and containers

| Pool / workflow label | Example CPU / memory | Max Jobs |
| --- | --- | --- |
| `nix` / `acs-nix` | 8 / 16 GiB | 10 |
| `go` / `acs-go` | 2 / 4 GiB | 10 |
| `node24` / `acs-node24` | 4 / 8 GiB | 10 |
| `docker` / `acs-docker` | 4 / 8 GiB | 10 |

Pools use amd64, 30 GiB temporary storage, and scale to zero. KEDA runs three
components, each with one replica and 250m CPU / 512Mi requests and limits.
The `docker` pool uses Buildah; it has no Docker daemon or socket.

Use `acs status`, `acs watch`, and `acs logs POD-NAME` to diagnose failures.
Finished Jobs default to immediate deletion; temporarily set a pool's
`ttlSecondsAfterFinished` to `300` when collecting failure logs.

[Add images](images/README.md) · [Change pools, cleanup and troubleshooting](docs/operations.md)
· [Project index](PROJECT_INDEX.md). Full Kind acceptance remains pending on the
Service-routing blocker; offline checks do not prove live ACS acceptance.
