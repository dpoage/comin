package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const minimalConfig = `hostname: machine
state_dir: /var/lib/comin
repository_type: flake
remotes:
- name: origin
  url: https://example.com/infra
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "comin.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestReadPreflightOptions(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		command string
		timeout int
		wantErr string
	}{
		{name: "absent", yaml: minimalConfig, command: "", timeout: 60},
		{name: "timeout zero", yaml: minimalConfig + "preflight_timeout: 0\n", command: "", timeout: 60},
		{name: "both set", yaml: minimalConfig + "preflight_command: /x/y\npreflight_timeout: 7\n", command: "/x/y", timeout: 7},
		{name: "command only", yaml: minimalConfig + "preflight_command: /x/y\n", command: "/x/y", timeout: 60},
		{name: "timeout negative", yaml: minimalConfig + "preflight_timeout: -1\n", wantErr: "preflight_timeout"},
		{name: "command relative", yaml: minimalConfig + "preflight_command: x/y\n", wantErr: "preflight_command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := Read(writeConfig(t, tt.yaml))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.command, config.PreflightCommand)
			assert.Equal(t, tt.timeout, config.PreflightTimeout)
		})
	}
}

func TestReadBranchOperations(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantMain    string
		wantTesting string
		wantErr     []string
	}{
		{name: "defaults", yaml: minimalConfig, wantMain: "switch", wantTesting: "test"},
		{name: "main switch", yaml: withOperation("main", "switch"), wantMain: "switch", wantTesting: "test"},
		{name: "main test", yaml: withOperation("main", "test"), wantMain: "test", wantTesting: "test"},
		{name: "testing switch", yaml: withOperation("testing", "switch"), wantMain: "switch", wantTesting: "switch"},
		{name: "testing test", yaml: withOperation("testing", "test"), wantMain: "switch", wantTesting: "test"},
		{name: "main boot", yaml: withOperation("main", "boot"), wantErr: []string{"origin", "branches.main.operation", "converges"}},
		{name: "main dry-activate", yaml: withOperation("main", "dry-activate"), wantErr: []string{"origin", "branches.main.operation", "converges"}},
		{name: "main check", yaml: withOperation("main", "check"), wantErr: []string{"origin", "branches.main.operation", "converges"}},
		{name: "main typo", yaml: withOperation("main", "swich"), wantErr: []string{"origin", "branches.main.operation", "converges"}},
		{name: "testing boot", yaml: withOperation("testing", "boot"), wantErr: []string{"origin", "branches.testing.operation", "converges"}},
		{name: "testing dry-activate", yaml: withOperation("testing", "dry-activate"), wantErr: []string{"origin", "branches.testing.operation", "converges"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := Read(writeConfig(t, tt.yaml))
			if tt.wantErr != nil {
				require.Error(t, err)
				for _, want := range tt.wantErr {
					assert.Contains(t, err.Error(), want)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantMain, config.Remotes[0].Branches.Main.Operation)
			assert.Equal(t, tt.wantTesting, config.Remotes[0].Branches.Testing.Operation)
		})
	}
}

func withOperation(branch, operation string) string {
	return minimalConfig + fmt.Sprintf("  branches:\n    %s:\n      operation: %s\n", branch, operation)
}

// evalModules loads no NixOS base modules, so the options referenced by
// module-options.nix must be declared here, as nix/module-options-doc.nix does.
const driverTemplate = `{ pkgs ? import <nixpkgs> {}
, cominConfig ? {}
}:
let
  lib = pkgs.lib;
  moduleOptions = import @NIXDIR@/module-options.nix;
  eval = lib.evalModules {
    modules = [
      moduleOptions
      {
        options = {
          assertions = lib.mkOption { type = lib.types.anything; description = ""; };
          networking.hostName = lib.mkOption { type = lib.types.str; internal = true; default = "machine"; };
        };
        config.services.comin = {
          enable = true;
          remotes = [ { name = "origin"; url = "https://example.com/infra"; } ];
        } // cominConfig;
      }
    ];
  };
  cominConfigLib = import @NIXDIR@/comin-config.nix { inherit pkgs lib; config = eval.config; };
in
  cominConfigLib.cominConfigYaml
`

func TestRenderedConfigRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("nix-build"); err != nil {
		t.Skip("nix-build not found: cannot render the configuration through the nix module")
	}
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	nixDir := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(file))), "nix")

	render := func(t *testing.T, extraConfig string) (string, string, error) {
		t.Helper()
		dir := t.TempDir()
		driver := filepath.Join(dir, "driver.nix")
		require.NoError(t, os.WriteFile(driver, []byte(strings.ReplaceAll(driverTemplate, "@NIXDIR@", nixDir)), 0o600))
		rendered := filepath.Join(dir, "rendered.yaml")
		cmd := exec.CommandContext(t.Context(), "nix-build", driver, "-o", rendered, "--arg", "cominConfig", extraConfig)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		err := cmd.Run()
		return rendered, output.String(), err
	}

	t.Run("set", func(t *testing.T) {
		rendered, _, err := render(t, `{ preflightCommand = "/x/y"; preflightTimeout = 7; }`)
		require.NoError(t, err)
		config, err := Read(rendered)
		require.NoError(t, err)
		assert.Equal(t, "/x/y", config.PreflightCommand)
		assert.Equal(t, 7, config.PreflightTimeout)
	})

	t.Run("unset", func(t *testing.T) {
		rendered, _, err := render(t, `{ }`)
		require.NoError(t, err)
		config, err := Read(rendered)
		require.NoError(t, err)
		assert.Equal(t, "", config.PreflightCommand)
		assert.Equal(t, 60, config.PreflightTimeout)
	})

	t.Run("relative command", func(t *testing.T) {
		_, output, err := render(t, `{ preflightCommand = "rel/thing"; }`)
		require.Error(t, err)
		assert.Contains(t, output, "preflightCommand")
		assert.Contains(t, output, "path")
	})

	t.Run("operation boot", func(t *testing.T) {
		_, output, err := render(t, `{ remotes = [ { name = "origin"; url = "u"; branches.main.operation = "boot"; } ]; }`)
		require.Error(t, err)
		assert.Contains(t, output, "branches.main.operation")
		assert.Contains(t, output, `"switch", "test"`)
	})

	t.Run("operation dry-activate", func(t *testing.T) {
		_, output, err := render(t, `{ remotes = [ { name = "origin"; url = "u"; branches.testing.operation = "dry-activate"; } ]; }`)
		require.Error(t, err)
		assert.Contains(t, output, "branches.testing.operation")
		assert.Contains(t, output, `"switch", "test"`)
	})
}
