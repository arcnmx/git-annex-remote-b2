{ pkgs ? import <nixpkgs> { } }: with pkgs; callPackage ./derivation.nix { }
