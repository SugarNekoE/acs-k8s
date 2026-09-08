# Add a runner image

Buildah builds runner images from [Containerfile](Containerfile). The root Nix
flake packages the CLI and development tools. Run the commands below from the
repository root, inside `nix develop` or `devenv shell`.

To add tools to an existing image, edit its stage, extend its smoke test in
[`workflow.go`](../cli/internal/app/workflow.go), then follow the build/check and
publish steps below with that image name. To reuse an existing image for another
workflow label, follow [Change pools](../docs/operations.md#change-pools).

## 1. Add a Containerfile target

Choose a lowercase name, such as `rust`, and append a stage:

```dockerfile
FROM common AS rust
USER 0:0
RUN dnf -y install rust cargo && dnf clean all
USER 1000:1000
```

`common` supplies Forgejo Runner, the embedded `acs` CLI, Node 24 for actions,
certificates, `/data`, and `CMD ["forgejo-runner", "one-job"]`. Keep that command,
the empty entrypoint, and the writable `/data/home` owned by the runtime UID.
Deployed Pods pass `one-job --wait`; the runner must support that flag.
Use `USER 0:0` only for installation in this example; its runtime UID/GID is
`1000:1000`. Preserve the existing Nix and Buildah images' explicit user settings.

Pin any additional external base image by digest. Put public files needed by
`COPY` under `images/rootfs/` and reference them as `rootfs/...`. The CLI stages
only Go build inputs, the Containerfile, and rootfs files; local JSON, test data,
and SOPS documents must stay outside the build context. Build through `acs` to
retain this filtering. Do not add credentials to a layer or build argument.

## 2. Register the image and its tests

Adding a stage alone does not make `acs images build rust` valid. Update these
integration points before building the image:

| File | Change |
| --- | --- |
| [`app.go`](../cli/internal/app/app.go), [`operations.go`](../cli/internal/app/operations.go) | Append `rust` to `pools`; update help/usage and the positional `check images` argument count/order. |
| [`workflow.go`](../cli/internal/app/workflow.go) | Add a `rust` switch case and usage text. Compile and run a tiny Rust program in the temporary work directory, verify its output, and handle every error. The `image-test` path must work without networking. |
| [`image.go`](../cli/internal/app/image.go), [`bundle.go`](../cli/internal/app/bundle.go) | Extend the matching archive order in `imageChecks` and `validate images`; retain user, ownership, certificate, entrypoint, network and capability checks. |
| [`kind_testflow.go`](../cli/internal/app/kind_testflow.go) | Extend the report's four-pool/count assumptions and messages so acceptance requires evidence for every configured image. Keep job, pod and scale-to-zero checks. |
| [`app_test.go`](../cli/internal/app/app_test.go), [`image_build_test.go`](../cli/internal/app/image_build_test.go) | Cover selection/building, smoke-test failure propagation, archive ordering and complete Kind evidence for the new image. Keep secret-exclusion and cleanup tests. |

The existing builder and publisher derive the Containerfile target, archive name
`runner-rust-image`, and registry repository `forgejo-runner-rust` from `pools`.
Keep all four existing images and their tests. A new name without a workflow
switch case would otherwise pass only the shared Node check.

## 3. Build and check locally

Rebuild the workstation CLI first; the image build also embeds the updated CLI:

```bash
mkdir -p .output
go -C cli build -o ../.output/acs ./cmd/acs
./.output/acs validate cli
./.output/acs --output-dir .output/images images build rust
./.output/acs check image rust .output/images/runner-rust-image
```

These commands build and check locally. Run as your normal user with working
rootless Buildah storage and registry configuration. Image checks execute the
actual filesystem with networking disabled and all capabilities dropped except
`DAC_OVERRIDE` for Nix. The Nix workflow container needs this capability to move
read-only fixed-output directories into its local store; its registration
container retains no capabilities. The Nix smoke test builds a fresh fixed-output
directory to verify that operation. For ARM64, build and check on a native ARM64
host with `--system aarch64-linux`.
Run `nix flake check` for race tests and static analysis as well.

## 4. Publish and connect a pool

When ready to publish, create the ACR repository `forgejo-runner-rust`, configure
the registry/namespace in local operator JSON, and use separate push credentials:

```bash
./.output/acs registry login
./.output/acs images publish rust
./.output/acs registry digests
```

`images publish` rebuilds before pushing; keep the checked source unchanged.
For ACS VPC pulls, configure `pull_registry` in operator settings as described in
the [VPC setup reference](../docs/operations.md#vpc-image-pulls). Publishing stays on
`registry`; generated runner manifests use the pull endpoint with the same digest.
Add this entry under `runners` in local `acs-deployment.json`, using the published
digest instead of the placeholder:

```json
"rust": {
  "image": "REGISTRY/NAMESPACE/forgejo-runner-rust@sha256:DIGEST",
  "labels": ["acs-rust"],
  "maxReplicas": 10
}
```

The default runtime UID is 1000 and default resources are 2 CPU / 4 GiB. Preserve
existing pool sizes, the 10-Job ceiling, global scope, separate credentials and
KEDA settings. If updating the versioned deployment example, leave its image
reference empty. Follow [Deploy and update](../acs-deployment-guide.md#deploy-and-update)
to regenerate/inspect snapshots, validate, and explicitly deploy using the rebuilt CLI.

Select the new pool in Forgejo with `runs-on: acs-rust` and smoke-test it with
`run: acs workflow test rust`. Include the pool in local deployment settings when
running isolated Kind acceptance. Native image checks do not prove live ACS
scaling; full Kind acceptance remains pending on the Service-routing blocker.
