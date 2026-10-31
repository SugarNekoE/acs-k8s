# Project index

Standalone ACS/Forgejo runner CLI. Read [AGENTS.md](AGENTS.md) first.
Go handles operations and checks; Buildah builds images. Nix remains a workflow
tool inside the Nix pool; the root flake packages the CLI and development tools.

## File map

| Path | Purpose |
| --- | --- |
| [CLI entry](cli/cmd/acs/main.go) | Executable entry point |
| [Go implementation](cli/internal/app) | Configuration, manifests, bundles, deployment, diagnostics and Kind |
| [Image builder](cli/internal/app/image_build.go) | Restricted build contexts and Buildah archive exports |
| [Image checks](cli/internal/app/image.go) | Native rootless container validation |
| [Authentication](cli/internal/app/auth.go) | SOPS, private kubeconfig lifecycle and Secret rendering |
| [Embedded assets](cli/internal/app/assets) | Pinned KEDA chart and Kind registry fixture |
| [Test fixtures](cli/internal/app/testdata/nix-reference) | Former Nix outputs for regression comparison |
| [Containerfile](images/Containerfile), [rootfs](images/rootfs) | Four image targets and runtime configuration |
| [Image guide](images/README.md) | Add an image target, wire CLI checks, build, publish and connect a pool |
| [Operator example](acs-config.example.json) | Template for ignored `acs-config.json` |
| [Deployment example](acs-deployment.example.json) | Template for ignored `acs-deployment.json`; fill image references |
| [Deployment guide](acs-deployment-guide.md) | Quick setup and deployment commands |
| [Operations reference](docs/operations.md) | Credentials, VPC pulls, pool changes, cleanup and troubleshooting |
| [devenv.nix](devenv.nix), [devenv.yaml](devenv.yaml) | Go development environment and hooks |
| [flake.nix](flake.nix), [flake.lock](flake.lock) | Linux CLI packages, checks and tooling shell |
| [Forgejo cache workflow](.forgejo/workflows/cache.yaml) | Check/build the default package and upload to Cachix and niks3 (`amber.asnk.io`, Forgejo OIDC) |

## Standalone layout

- The installed CLI also runs from an operator directory without sources.
  Paths use `--repo`, the `--config` file's directory, or the nearest ACS
  directory (falling back to the current directory). Go sources live in `cli/`;
  image builds, CLI validation and the tooling shell need the ACS checkout.
- Set `sops_file` explicitly to an existing encrypted document. `acs shell`
  opens this root's flake development shell, including from subdirectories.
- Local operator/deployment JSON files are currently absent. Use the examples
  and use `nix develop` or `devenv shell` for Go, errcheck, staticcheck, Buildah,
  Skopeo, Helm, kubectl and SOPS.
- Preserve max 10 Jobs per runner pool and three KEDA components at
  250m CPU / 512Mi each. Keep secrets out of build contexts and logs.

## Validation

From `cli/`: `go test -race ./...`, `go vet ./...`,
`errcheck -blank -excludeonly ./...`, and `staticcheck ./...`.
`nix flake check` builds the CLI and runs these checks on the host system;
`nix build .#acs-cli` (also `.#default`) produces `result/bin/acs`. Both Linux architectures are
packaged; an ARM package build requires an ARM builder or configured emulation.
All four native amd64 image checks passed before relocation. Full Kind acceptance
remains pending due to the prior Service-routing blocker; offline tests are not
live ACS acceptance.
