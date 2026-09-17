{
  description = "easy-alarms - a simple desktop alarm clock (Fyne GUI + alarmctl CLI)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };

        # Runtime libraries needed by Fyne (GL/X11/Wayland) and the beep
        # audio backend (ALSA via oto/cgo).
        runtimeLibs = with pkgs; [
          libGL
          libx11
          libxcursor
          libxi
          libxinerama
          libxrandr
          libxxf86vm
          libxext
          libxcb
          libxkbcommon
          wayland
          alsa-lib
        ];

        nativeBuildTools = with pkgs; [
          pkg-config
        ];
      in
      {
        packages.default = pkgs.buildGoModule {
          pname = "easy-alarms";
          version = "0.1.0";
          src = ./.;

          # Update after go.mod/go.sum changes: nix build will report the
          # correct hash on mismatch.
          vendorHash = "sha256-sB6NQcLwAWrIWEsYUOGkbx7KdPQ+N7mn2F9uX3Ad/mY=";

          subPackages = [ "cmd/easy-alarms" "cmd/alarmctl" ];

          nativeBuildInputs = nativeBuildTools ++ [ pkgs.patchelf ];
          buildInputs = runtimeLibs;

          # Fyne/oto require cgo (GL bindings + ALSA).
          env.CGO_ENABLED = "1";

          ldflags = [
            "-X main.version=${self.shortRev or "dev"}"
          ];

          # Only easy-alarms (the Fyne GUI, cgo-linked) actually needs the GL/X11/
          # Wayland/ALSA runtime libs. alarmctl is a plain internally-linked Go
          # binary with no such deps: patchelf rewriting its rpath to this long
          # string corrupts the ELF (segfaults on exec before main runs), so
          # leave it untouched.
          postFixup = ''
            if [ -f "$out/bin/easy-alarms" ]; then
              patchelf --set-rpath "${pkgs.lib.makeLibraryPath runtimeLibs}" "$out/bin/easy-alarms"
            fi
          '';

          meta = with pkgs.lib; {
            description = "A simple desktop alarm clock";
            homepage = "https://github.com/";
            license = licenses.mit;
            mainProgram = "easy-alarms";
          };
        };

        apps.default = flake-utils.lib.mkApp {
          drv = self.packages.${system}.default;
          name = "easy-alarms";
        };

        devShells.default = pkgs.mkShell {
          buildInputs = runtimeLibs ++ nativeBuildTools ++ (with pkgs; [
            go
            gopls
            go-tools
            gotools
            go-task
            librsvg
          ]);

          LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath runtimeLibs;

          shellHook = ''
            export CGO_ENABLED=1
            echo "easy-alarms dev shell ready. Try: task build"
          '';
        };
      });
}
