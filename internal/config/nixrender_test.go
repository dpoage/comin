package config

// C11: comin's YAML config is rendered by the fork's own NixOS module
// (nix/comin-config.nix, via nix/module-options.nix's overrideLeaseFile
// option) and must parse back into the same Go configuration this package
// reads. Before this round, yaml.v2 silently ignored a misspelled key
// (premortem probe: err=nil, empty field) - a key rename on either side of
// the module/Go boundary would go unnoticed without an end-to-end check
// like this one, which renders the YAML through the real Nix module rather
// than hand-writing it.
//
// This shells out to `nix-build` against the fork's own repository (the
// worktree this test runs from): it evaluates nix/module-options.nix with
// lib.evalModules and renders nix/comin-config.nix from the result. It is
// not a full nixosSystem evaluation, since comin-config.nix only ever
// reads config.services.comin.*.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func repoRootForNixRenderTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	assert.NoError(t, err)
	// internal/config -> repo root is two levels up.
	root, err := filepath.Abs(filepath.Join(wd, "..", ".."))
	assert.NoError(t, err)
	return root
}

func TestNixRenderedConfigYamlRoundTripsOverrideLeaseFile(t *testing.T) {
	if _, err := exec.LookPath("nix-build"); err != nil {
		t.Skip("nix-build not found on PATH; this test requires the nix devshell/gate environment")
	}
	root := repoRootForNixRenderTest(t)
	leasePath := "/opt/pattern/override.json"

	// The config is evaluated through module-options.nix's option
	// declarations (as module.nix imports them), so renaming the option
	// there breaks this test too.
	expr := `
let
  flake = builtins.getFlake "` + root + `";
  pkgs = import flake.inputs.nixpkgs { system = builtins.currentSystem; };
  lib = pkgs.lib;
  eval = lib.evalModules {
    modules = [
      (flake.outPath + "/nix/module-options.nix")
      {
        options = {
          assertions = lib.mkOption { type = lib.types.anything; default = [ ]; };
          networking.hostName = lib.mkOption { type = lib.types.str; default = "test-host"; };
        };
        config = {
          _module.args.pkgs = pkgs;
          services.comin = {
            enable = true;
            repositoryType = "nix";
            systemAttr = "test";
            remotes = [ ];
            overrideLeaseFile = "` + leasePath + `";
          };
        };
      }
    ];
  };
in (import (flake.outPath + "/nix/comin-config.nix") { config = eval.config; inherit pkgs lib; }).cominConfigYaml
`

	cmd := exec.Command("nix-build",
		"--extra-experimental-features", "nix-command flakes",
		"--no-out-link",
		"-E", expr,
	)
	out, err := cmd.CombinedOutput()
	assert.NoError(t, err, "nix-build failed: %s", string(out))

	renderedPath := strings.TrimSpace(lastLine(string(out)))
	assert.NotEmpty(t, renderedPath)

	cfg, err := Read(renderedPath)
	assert.NoError(t, err)
	assert.Equal(t, leasePath, cfg.OverrideLeaseFile, "the configured lease path must round-trip through the Nix module and the Go yaml decoder")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}
