{ pkgs ? import <nixpkgs> { } }: with pkgs; with pkgs.lib; with builtins; mkShell {
  nativeBuildInputs = [ gitAndTools.git-annex go ];

  shellHook = ''
    unset GOPATH
  '' + optionalString (!(any (p: p.prefix == "ci") nixPath)) ''
    export NIX_PATH="''${NIX_PATH}:ci=https://github.com/arcnmx/ci/archive/master.tar.gz"
  '';
}
