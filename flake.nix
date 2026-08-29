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
            vendorHash = "sha256-poi3xB2+kRpiSgh5Mwm5U8tUBV3gToG3ci2nWFuYlXA=";
            env.CGO_ENABLED = "0";
            ldflags = [
              "-s"
              "-w"
            ];
          };

          media-fixer = pkgs.buildGo127Module (
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

          media-agent = pkgs.buildGo127Module (
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

          media-fixer-check = pkgs.buildGo127Module (
            commonArgs
            // {
              pname = "media-fixer-check";
              subPackages = [ "cmd/media-fixer-check" ];
              meta = {
                description = "Live tool-availability check for media-fixer's agent tools";
                mainProgram = "media-fixer-check";
              };
            }
          );
        in
        {
          packages = {
            default = media-fixer;
            inherit media-fixer media-agent media-fixer-check;
          };

          devShells.default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go_1_27
              gopls
              gotools
              go-tools
              golangci-lint
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
