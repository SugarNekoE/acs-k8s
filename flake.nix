{
  description = "ACS Forgejo runner operations CLI";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          acs-cli = pkgs.buildGo126Module {
            pname = "acs-cli";
            version = "0-unstable";
            src = pkgs.lib.fileset.toSource {
              root = ./cli;
              fileset = pkgs.lib.fileset.unions [
                ./cli/go.mod
                ./cli/go.sum
                (pkgs.lib.fileset.fileFilter (file: file.hasExt "go") ./cli/internal)
                ./cli/internal/app/assets
                ./cli/internal/app/testdata
                (pkgs.lib.fileset.fileFilter (file: file.hasExt "go") ./cli/cmd)
              ];
            };
            vendorHash = "sha256-MYHY7N+LqNnfNuIdUqtb4Cr24UQqi+4tbc1ysdzJ2BY=";
            subPackages = [ "cmd/acs" ];
            env.CGO_ENABLED = 0;
            ldflags = [
              "-s"
              "-w"
            ];
            nativeCheckInputs = [
              pkgs.errcheck
              pkgs.go-tools
              pkgs.kubernetes-helm
            ];
            checkPhase = ''
              runHook preCheck
              export XDG_CACHE_HOME="$TMPDIR/cache"
              go test ./...
              go vet ./...
              errcheck -blank -excludeonly ./...
              staticcheck ./...
              runHook postCheck
            '';
            meta = {
              description = "Manage ACS-hosted Forgejo runners";
              mainProgram = "acs";
              platforms = systems;
            };
          };
        in
        {
          inherit acs-cli;
          default = acs-cli;
        }
      );

      apps = forAllSystems (
        system:
        let
          acs-cli = {
            type = "app";
            program = "${self.packages.${system}.acs-cli}/bin/acs";
            meta.description = "Manage ACS-hosted Forgejo runners";
          };
        in
        {
          inherit acs-cli;
          default = acs-cli;
        }
      );

      checks = forAllSystems (system: {
        inherit (self.packages.${system}) acs-cli;
        race = self.packages.${system}.acs-cli.overrideAttrs (old: {
          pname = "acs-cli-race";
          env = old.env // {
            CGO_ENABLED = 1;
          };
          checkPhase = ''
            runHook preCheck
            go test -race ./...
            runHook postCheck
          '';
        });
      });

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              self.packages.${system}.acs-cli
              go_1_26
              errcheck
              go-tools
              buildah
              skopeo
              crun
              kubectl
              kubernetes-helm
              sops
              kind
              cacert
              less
              bashInteractive
            ];
          };
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
