{
  description = "Development environment for github.com/mdlayher/frrtest";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            # go.mod's language version floor is what this must
            # satisfy; bump alongside it.
            pkgs.go_1_27
          ]
          ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [
            # The module's own tests, for development and CI only:
            # frr on $PATH puts vtysh where the oracle discovers the
            # daemons. This is not a pin. The oracle's tests read the
            # version they enforce from this FRR's zebra, so a flake
            # update moves it without touching Go; consumers pin their
            # own FRR.
            pkgs.frr
          ];
        };
      });
    };
}
