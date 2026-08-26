{
  pkgs ? import ./nixpkgs.nix,
}:
let
  imageName = "ghcr.io/azimuth-cloud/capi-janitor-openstack-go";

  # Build the manager binary for a target GOARCH ("amd64" / "arm64").
  # CGO is off, so overriding GOARCH cross-compiles without `pkgsCross`, which
  # would link against the target glibc and bloat the arm64 image.
  buildManager =
    goarch:
    (pkgs.buildGoModule {
      pname = "capi-janitor-openstack-go";
      version = "0.0.0-dev";
      src = ../.;
      subPackages = [ "cmd" ];
      env.CGO_ENABLED = "0";
      ldflags = [
        "-s"
        "-w"
      ];
      # Run `nix-build nix -A manager` once; it will fail and print the real hash.
      vendorHash = "sha256-VbUZAmZtuSSa611i4xxI/1alwMSUAm84OSNfhk00vSk=";
      postInstall = ''
        # Cross builds land in $out/bin/$GOOS_$GOARCH.
        if [ -d "$out/bin/''${GOOS}_''${GOARCH}" ]; then
          mv "$out/bin/''${GOOS}_''${GOARCH}"/* "$out/bin/"
          rmdir "$out/bin/''${GOOS}_''${GOARCH}"
        fi
        mv $out/bin/cmd $out/bin/manager
      '';
      meta.mainProgram = "manager";
    }).overrideAttrs
      (old: {
        # GOOS is pinned to linux so the container binary is always built for the
        # image OS, regardless of the host.
        env = old.env // {
          GOOS = "linux";
          GOARCH = goarch;
        };
        dontStrip = true; # `-s -w` already strips; native strip rejects foreign ELFs
        doCheck = false; # cannot run a foreign-arch test binary
      });

  # `architecture` must be explicit — it otherwise defaults to the native
  # GOARCH, and CI reads it to assemble the manifest list.
  buildImage =
    architecture: m:
    pkgs.dockerTools.buildLayeredImage {
      name = imageName;
      tag = "latest";
      inherit architecture;
      contents = [
        pkgs.cacert
        m
      ];
      config = {
        Entrypoint = [ "/bin/manager" ];
        ExposedPorts."8081/tcp" = { };
        User = "65532:65532";
        Labels = {
          "org.opencontainers.image.source" = "https://github.com/azimuth-cloud/capi-janitor-openstack-go";
          "org.opencontainers.image.licenses" = "Apache-2.0";
        };
      };
    };

  # SBOM — reads Go build-info embedded in the static binary (survives -s -w).
  buildSbom =
    m:
    pkgs.runCommand "sbom.cdx.json"
      {
        nativeBuildInputs = [ pkgs.syft ];
      }
      ''
        export HOME=$TMPDIR
        syft scan ${m}/bin/manager \
          --output cyclonedx-json=$out \
          --quiet
      '';

  manager = buildManager "amd64";
  manager-arm64 = buildManager "arm64";

  # CI check: go fmt + go vet + unit tests (host GOARCH only).
  tests = (buildManager pkgs.go.GOARCH).overrideAttrs (old: {
    pname = "capi-janitor-openstack-go-tests";
    subPackages = [ ]; # build all packages, not just cmd/
    # GOOS=linux for the container binary.
    env = old.env // {
      GOOS = pkgs.go.GOOS;
    };
    doCheck = true;
    checkPhase = ''
      runHook preCheck

      bad=$(gofmt -l $(find . -name '*.go' \
            -not -path './vendor/*' -not -path './.git/*'))
      if [ -n "$bad" ]; then
        echo "Files not formatted with go fmt:"
        printf '%s\n' $bad
        exit 1
      fi

      go vet ./...

      go test -v $(go list ./... | grep -v '/test/e2e')

      runHook postCheck
    '';
    installPhase = "touch $out";
  });
in
{
  inherit manager manager-arm64 tests;

  image = buildImage "amd64" manager;
  image-arm64 = buildImage "arm64" manager-arm64;

  sbom = buildSbom manager;
  sbom-arm64 = buildSbom manager-arm64;
}
