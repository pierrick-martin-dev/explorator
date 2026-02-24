{
  description = "A very basic flake";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
    treefmt-nix.url = "github:numtide/treefmt-nix";
  };

  outputs =
    {
      self,
      nixpkgs,
      treefmt-nix,
      systems,
    }:
    let
      pkgsx86Linux = nixpkgs.legacyPackages.x86_64-linux;
      eachSystem = f: nixpkgs.lib.genAttrs (import systems) (system: f nixpkgs.legacyPackages.${system});

      treefmtEval = eachSystem (pkgs: treefmt-nix.lib.evalModule pkgs ./treefmt.nix);
    in
    {
      formatter = eachSystem (pkgs: treefmtEval.${pkgs.system}.config.build.wrapper);

      devShells.x86_64-linux.default =
        let
          pkgs = pkgsx86Linux;
        in
        pkgsx86Linux.mkShell {
          buildInputs = [
            pkgs.google-cloud-sdk
            pkgs.gemini-cli

            pkgs.llvmPackages_20.clang-tools

            pkgs.repomix

            pkgs.tokei

            pkgs.go

            pkgs.tree
          ];
        };

    };
}
