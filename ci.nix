{ pkgs, lib, config, channels, ... }: with lib; let
  inherit (pkgs.gitAndTools) git-annex;
  git-annex-remote-b2 = pkgs.callPackage ./derivation.nix { };
in {
  name = "git-annex-remote-b2";

  ci.gh-actions = {
    enable = true;
  };
  gh-actions.env = {
    B2_ACCOUNT_ID = "\${{secrets.B2_ACCOUNT_ID}}";
    B2_APP_KEY = "\${{secrets.B2_APP_KEY}}";
  };

  channels.nixpkgs = "stable";

  tasks = {
    build = {
      inputs = git-annex-remote-b2;
    };
    test = {
      inputs = pkgs.ci.command {
        name = "git-annex-remote-b2-test";
        displayName = "test";
        impure = true;
        command = ''
          export PATH="$PATH:${git-annex}/bin:${git-annex-remote-b2}/bin"
          bash -x ${./test.bash}
        '';
      };
      skip = builtins.getEnv "B2_ACCOUNT_ID" == "" || pkgs.hostPlatform != pkgs.buildPlatform;
    };
    release = {
      inputs = git-annex-remote-b2.override { enableStatic = true; };
    };
  };

  jobs = {
    linux = { pkgs, ... }: {
      system = "x86_64-linux";
      tasks = {
        win32 = {
          inputs = pkgs.pkgsCross.mingw32.callPackage ./derivation.nix { };
        };
        win64 = {
          inputs = pkgs.pkgsCross.mingw32.callPackage ./derivation.nix { };
        };
      };
    };
    mac = {
      system = "x86_64-darwin";
    };
  };
}
