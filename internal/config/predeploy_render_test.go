package config

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderCominYaml renders comin.yaml through the fork's NixOS module with
// extraConfig added to services.comin, and returns the YAML store path.
func renderCominYaml(t *testing.T, extraConfig string) string {
	t.Helper()
	if _, err := exec.LookPath("nix-build"); err != nil {
		t.Skip("nix-build not found on PATH; this test requires the nix devshell/gate environment")
	}
	root := repoRootForNixRenderTest(t)
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
            ` + extraConfig + `
          };
        };
      }
    ];
  };
in (import (flake.outPath + "/nix/comin-config.nix") { config = eval.config; inherit pkgs lib; }).cominConfigYaml
`
	out, err := exec.Command("nix-build",
		"--extra-experimental-features", "nix-command flakes",
		"--no-out-link",
		"-E", expr,
	).CombinedOutput()
	require.NoError(t, err, "nix-build failed: %s", string(out))
	path := strings.TrimSpace(lastLine(string(out)))
	require.NotEmpty(t, path)
	return path
}

func TestNixRenderedConfigYamlRoundTripsPreDeploymentCommand(t *testing.T) {
	hook := "/nix/store/x-hook"
	path := renderCominYaml(t, `preDeploymentCommand = "`+hook+`";`)

	cfg, err := Read(path)
	require.NoError(t, err)
	assert.Equal(t, hook, cfg.PreDeploymentCommand)
}

func TestNixRenderedConfigYamlOmitsNullAndEmptyPreDeploymentCommand(t *testing.T) {
	unset := renderCominYaml(t, "")
	null := renderCominYaml(t, `preDeploymentCommand = null;`)
	empty := renderCominYaml(t, `preDeploymentCommand = "";`)

	unsetBytes, err := os.ReadFile(unset)
	require.NoError(t, err)
	assert.NotContains(t, string(unsetBytes), "pre_deployment_command")
	for name, path := range map[string]string{"null": null, "empty": empty} {
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, string(unsetBytes), string(got), "preDeploymentCommand = %s must render the same YAML as the option unset", name)
	}

	cfg, err := Read(empty)
	require.NoError(t, err)
	assert.Empty(t, cfg.PreDeploymentCommand)
}
