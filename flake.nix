{
  description = "media-fixer — self-healing media stack manager";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
  };

  outputs = inputs @ {flake-parts, ...}:
    flake-parts.lib.mkFlake {inherit inputs;} {
      systems = ["x86_64-linux" "aarch64-linux"];

      perSystem = {
        pkgs,
        system,
        ...
      }: let
        media-fixer = pkgs.buildGoModule {
          pname = "media-fixer";
          version = "0.1.0";

          src = ./.;

          vendorHash = "sha256-P2R0XeaOj+lUovqx2e3Ay+ztcvrEdNx/8mQvtWnjryA=";

          CGO_ENABLED = 0;

          ldflags = ["-s" "-w"];

          meta = {
            description = "Self-healing media stack manager";
            mainProgram = "mediafixer";
          };
        };
      in {
        packages = {
          default = media-fixer;
          inherit media-fixer;
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            gotools
            go-tools
          ];
        };
      };

      flake = {
        nixosModules.default = import ./nix/module.nix;
        nixosModules.media-fixer = import ./nix/module.nix;
      };
    };
}
