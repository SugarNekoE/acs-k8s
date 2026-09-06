{ pkgs, ... }:

{
  packages = with pkgs; [
    yaml-language-server
    package-version-server
    vscode-json-languageserver
    buildah
    skopeo
    crun
    kubernetes-helm
    kubectl
    sops
    errcheck
    go-tools
    kind
  ];

  languages = {
    go = {
      enable = true;
      version = "1.26.0";
    };
  };

  git-hooks = {
    enable = true;
    hooks = {
      convco = {
        enable = true;
      };
      nixfmt.enable = true;
      statix.enable = true;
    };
  };
}
