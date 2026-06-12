{
  description = "media-fixer — self-healing media stack manager";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
  };

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];

      perSystem =
        {
          pkgs,
          ...
        }:
        let
          commonArgs = {
            version = "0.1.0";
            src = ./.;
            vendorHash = "sha256-4OnOEq97YkZCkXgQYFaQIAq/WjwMT/JQBboc0tbuP5M=";
            env.CGO_ENABLED = "0";
            ldflags = [
              "-s"
              "-w"
            ];
          };

          media-fixer = pkgs.buildGoModule (
            commonArgs
            // {
              pname = "media-fixer";
              subPackages = [ "." ];
              meta = {
                description = "Self-healing media stack manager";
                mainProgram = "mediafixer";
              };
            }
          );

          media-agent = pkgs.buildGoModule (
            commonArgs
            // {
              pname = "media-agent";
              subPackages = [ "cmd/media-agent" ];
              meta = {
                description = "media-agent sidecar for minz-media-0";
                mainProgram = "media-agent";
              };
            }
          );
        in
        {
          packages = {
            default = media-fixer;
            inherit media-fixer media-agent;
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
        nixosModules.default = import ./nix/media-fixer.nix;
        nixosModules.media-fixer = import ./nix/media-fixer.nix;
        nixosModules.media-agent = import ./nix/media-agent.nix;
      };
    };
}
