{ platform, ... }:

platform.buildGo.program {
  name = "mud";

  srcs = [
    ./mud.go
  ];

  deps = [
    platform.lib.nix.archive
    platform.lib.nix.base32
    platform.lib.tempfile
  ] ++ (with platform.third_party; [
    gopkgs."golang.org".x.mod.module
    gopkgs."golang.org".x.tools.go.packages
    gopkgs."go.uber.org".multierr
  ]);
}
