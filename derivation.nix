{ lib
, buildGoModule
, nix-gitignore
, hostPlatform
, buildPlatform
, buildPackages
, enableStatic ? hostPlatform != buildPlatform
}: with lib; let
  crossGo = buildPackages.buildPackages.go.overrideAttrs (old: {
    passthru = old.passthru or {} // {
      inherit (buildPackages.go) GOOS GOARCH;
    };
  });
  build = if enableStatic && hostPlatform != buildPlatform
    then buildGoModule.override { go = crossGo; }
    else buildGoModule;
  attrs = {
    pname = "git-annex-remote-b2";
    version = "0.1.0";

    src = nix-gitignore.gitignoreSource [ ''
      /*.nix
      /.github/
    '' ] ./.;

    modSha256 = "0565nvb6gq90i1ypv08rq41ivf04xqa3pw3hvl7208vdxzmn9ry2";
  } // optionalAttrs enableStatic {
    CGO_ENABLED = "0";

    buildFlagsArray = ''
      -ldflags=
        -s -w
    '';
  };
in build attrs
