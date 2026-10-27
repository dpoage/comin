package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nlewo/comin/internal/types"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

func Read(path string) (config types.Configuration, err error) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close() // nolint

	d := yaml.NewDecoder(file)
	if err := d.Decode(&config); err != nil {
		return config, err
	}
	for i, remote := range config.Remotes {
		if remote.Auth.AccessTokenPath != "" {
			content, err := os.ReadFile(remote.Auth.AccessTokenPath)
			if err != nil {
				return config, err
			}
			config.Remotes[i].Auth.AccessToken = strings.TrimSpace(string(content))
		}
		// On GitLab and GitHub, any non blank username is working
		if remote.Auth.Username == "" {
			config.Remotes[i].Auth.Username = "comin"
		}
		if remote.Timeout == 0 {
			config.Remotes[i].Timeout = 300
		}
		if remote.Branches.Main.Operation == "" {
			config.Remotes[i].Branches.Main.Operation = "switch"
		}
		if remote.Branches.Testing.Operation == "" {
			config.Remotes[i].Branches.Testing.Operation = "test"
		}
		supportedOperations := []string{"switch", "test"}
		if !slices.Contains(supportedOperations, config.Remotes[i].Branches.Main.Operation) {
			return config, fmt.Errorf("config: remote '%s': branches.main.operation is '%s' while comin converges the running system and an operation that does not activate it never converges: it must be one of '%s'", remote.Name, config.Remotes[i].Branches.Main.Operation, supportedOperations)
		}
		if !slices.Contains(supportedOperations, config.Remotes[i].Branches.Testing.Operation) {
			return config, fmt.Errorf("config: remote '%s': branches.testing.operation is '%s' while comin converges the running system and an operation that does not activate it never converges: it must be one of '%s'", remote.Name, config.Remotes[i].Branches.Testing.Operation, supportedOperations)
		}
	}

	if config.PreflightCommand != "" && !filepath.IsAbs(config.PreflightCommand) {
		return config, fmt.Errorf("config: preflight_command is '%s' while it must be an absolute path", config.PreflightCommand)
	}
	if config.PreflightTimeout < 0 {
		return config, fmt.Errorf("config: preflight_timeout is '%d' seconds while it must be a positive number of seconds", config.PreflightTimeout)
	}
	if config.PreflightTimeout == 0 {
		config.PreflightTimeout = 60
	}
	if config.ApiServer.ListenAddress == "" {
		config.ApiServer.ListenAddress = "127.0.0.1"
	}
	if config.ApiServer.Port == 0 {
		config.ApiServer.Port = 4242
	}
	if config.Exporter.ListenAddress == "" {
		config.Exporter.ListenAddress = "0.0.0.0"
	}
	if config.Exporter.Port == 0 {
		config.Exporter.Port = 4243
	}
	if config.StateFilepath == "" {
		config.StateFilepath = filepath.Join(config.StateDir, "state.json")
	}
	if config.RepositorySubdir == "" {
		config.RepositorySubdir = "."
	}
	supportedRepositoryTypes := []string{"flake", "nix"}
	if !slices.Contains(supportedRepositoryTypes, config.RepositoryType) {
		return config, fmt.Errorf("config: repository type is '%s' while it be one of '%s'", config.RepositoryType, supportedRepositoryTypes)
	}
	if config.Grpc.UnixSocketPath == "" {
		config.Grpc.UnixSocketPath = filepath.Join(config.StateDir, "grpc.sock")
	}
	logrus.Debugf("Config is '%#v'", config)
	return
}

func MkGitConfig(config types.Configuration) types.GitConfig {
	return types.GitConfig{
		Path:              filepath.Join(config.StateDir, "repository"),
		Dir:               config.RepositorySubdir,
		Remotes:           config.Remotes,
		GpgPublicKeyPaths: config.GpgPublicKeyPaths,
	}
}
