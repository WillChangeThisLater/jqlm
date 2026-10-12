{
  description = "jqlm: paul's jq fork with LLM builtins (gojq-based)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.05";

  outputs = { self, nixpkgs }: let
    pkgs = nixpkgs.legacyPackages.x86_64-linux;
  in {
    packages.x86_64-linux.jqlm = pkgs.buildGoModule rec {
      pname = "jqlm";
      version = "0.12.19";
      src = ./.;
      vendorHash = "sha256-6GJ19J3WDMPug7gvmYM2Dq43ZHIONh1hlRs3Mi0pAoY=";
      subPackages = [ "cmd/gojq" ];
      ldflags = [ "-s" "-w" "-X github.com/itchyny/gojq/cli.revision=nix" ];
      # installed as jqlm, built from the gojq cmd
      postInstall = ''
        mv $out/bin/gojq $out/bin/jqlm
      '';
    };
    packages.x86_64-linux.default = self.packages.x86_64-linux.jqlm;
  };
}