# SPDX-License-Identifier: Apache-2.0
#
# Trial dev shell for #536 (docs/research/2026-11-nix-flake-trial.md). Nothing in the repository
# uses this yet: no Makefile target, hack script or workflow calls it. `go tool` pins stay in go.mod.
#
#   nix --extra-experimental-features 'nix-command flakes' develop
#
# nixpkgs is pinned in flake.lock to a nixos-unstable channel revision that is at least 3 days old
# (ADR 0079). Every tool below matches the repository's pins at that revision; `overlays.exactPins`
# shows how to pin a tool to an exact upstream release (reusing the sha256s in
# hack/e2e-toolchain/Dockerfile) for the day nixpkgs lags. It is not applied.
{
  description = "Koinon development shell (trial, #536)";

  inputs = {
    # nixos-unstable channel head pushed 2026-10-28T02:53Z (commit date 2026-10-27).
    nixpkgs.url = "github:NixOS/nixpkgs/31ecbaca1bdefa45dffc409f0250bd8039ea0010";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      # Exact upstream releases of helm and kind, for when nixpkgs lags behind the Dockerfile pins.
      # Linux hashes are the Dockerfile's; darwin hashes are the release's own .sha256sum files.
      # Use: pkgs = import nixpkgs { inherit system; overlays = [ self.overlays.exactPins ]; };
      overlays.exactPins =
        final: prev:
        let
          plat =
            {
              x86_64-linux = "linux-amd64";
              aarch64-linux = "linux-arm64";
              x86_64-darwin = "darwin-amd64";
              aarch64-darwin = "darwin-arm64";
            }
            .${final.stdenv.hostPlatform.system};
          helmVersion = "v4.3.1";
          helmSha256 = {
            linux-amd64 = "0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b";
            linux-arm64 = "8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88";
            darwin-amd64 = "34bec4eba761ba5c16d1965ee6b8f5b0abe5993974a802274e2a4f0418893d89";
            darwin-arm64 = "6b4e61c8cd27d99e5a801e63ff25dd5c70b36875c2cd659d3e9ce6b76fc23198";
          };
          kindVersion = "v0.33.0";
          kindSha256 = {
            linux-amd64 = "aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d";
            linux-arm64 = "20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2";
            darwin-amd64 = "5a99f26f57246dc9319dd294803313197a0f34d33c525b3ea8b655db5916ece0";
            darwin-arm64 = "0c8c7dbe5e23594a198b786c4bc13dacc101fa6196b0cb0b23a1ca44e61f4b4f";
          };
        in
        {
          kubernetes-helm = final.stdenvNoCC.mkDerivation {
            pname = "helm";
            version = final.lib.removePrefix "v" helmVersion;
            src = final.fetchurl {
              url = "https://get.helm.sh/helm-${helmVersion}-${plat}.tar.gz";
              sha256 = helmSha256.${plat};
            };
            sourceRoot = plat;
            installPhase = "install -Dm755 helm $out/bin/helm";
          };
          kind = final.stdenvNoCC.mkDerivation {
            pname = "kind";
            version = final.lib.removePrefix "v" kindVersion;
            src = final.fetchurl {
              url = "https://github.com/kubernetes-sigs/kind/releases/download/${kindVersion}/kind-${plat}";
              sha256 = kindSha256.${plat};
            };
            dontUnpack = true;
            installPhase = "install -Dm755 $src $out/bin/kind";
          };
        };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShellNoCC {
          packages = with pkgs; [
            # Shell and userland: the same bash 5 and GNU tools on macOS and Linux.
            bashInteractive
            coreutils
            gnused
            gnugrep
            gawk
            # Lint
            shellcheck
            actionlint
            golangci-lint
            gitleaks
            # Go: go.mod says 1.27.3; nixpkgs' default `go` is still 1.26 at this revision.
            go_1_27
            ko
            # Cluster and GitHub
            kubernetes-helm
            kind
            kubectl
            gh
            jq
          ];
        };
      });
    };
}
