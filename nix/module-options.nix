{ config, pkgs, lib, ... }:
let
  cfg = config.services.comin;
in {
  imports = [
    (lib.mkRenamedOptionModule [ "services" "comin" "flakeSubdirectory" ] [ "services" "comin" "repositorySubdir" ])
    (lib.mkIf cfg.enable {
      assertions = [
        { assertion = cfg.hostname != null && cfg.hostname != ""; message = "You must set `networking.hostName` or `services.comin.hostname` explicitly in your NixOS configuration."; }
        { assertion = cfg.repositoryType == "nix" || cfg.repositoryType == "flake" && cfg.systemAttr == null; message = "When the `services.comin.repositoryType` is `flake`, the configuration attribute `services.comin.systemAttr` must not be set."; }
        { assertion = cfg.repositoryType == "flake" || cfg.repositoryType == "nix" && cfg.systemAttr != null; message = "When the `services.comin.repositoryType` is `nix`, the the configuration attribute `services.comin.systemAttr` must be set."; }
      ];
    })
  ];
  options = with lib; with types; {
    services.comin = {
      enable = mkOption {
        type = types.bool;
        default = false;
        description = ''
          Whether to run the comin service.
        '';
      };
      package = lib.mkPackageOption pkgs "comin" { nullable = true; } // {
        defaultText = "pkgs.comin or comin.packages.\${system}.default or null";
      };
      hostname = mkOption {
        type = str;
        default = config.networking.hostName;
        defaultText = lib.literalExpression "config.networking.hostName";
        description = ''
          The name of the configuration to evaluate and deploy.
          This value is used by comin to evaluate the flake output
          nixosConfigurations."<hostname>" or darwinConfigurations."<hostname>".
          Defaults to networking.hostName - you MUST set either this option
          or networking.hostName in your configuration.
        '';
      };
      repositoryType = mkOption {
        type = enum ["flake" "nix"];
        default = "flake";
        description = ''
          The type of the repository to fetch. It can either contains a flake or a classical Nix expression.
        '';
      };
      repositorySubdir = mkOption {
        type = str;
        default = ".";
        description = ''
          Subdirectory in the repository, containing a default.nix or a flake.nix file.
        '';
      };
      systemAttr = mkOption {
        type = nullOr str;
        default = null;
        description = ''
          This is the attribute containing the machine toplevel
          attribute. Note this is only used when the repositoryType is
          'nix'. When the repository type is 'flake', the attribute is
          derived from the hostname.
        '';
      };
      exporter = mkOption {
        description = "Options for the Prometheus exporter.";
        default = {};
        type = submodule {
          options = {
            listen_address = mkOption {
              type = str;
              description = ''
                Address to listen on for the Prometheus exporter. Empty string will listen on all interfaces.
              '';
              default = "";
            };
            port = mkOption {
              type = int;
              description = ''
                Port to listen on for the Prometheus exporter.
              '';
              default = 4243;
            };
            openFirewall = mkOption {
              type = types.bool;
              default = false;
              description = ''
                Open port in firewall for incoming connections to the Prometheus exporter.
              '';
            };
          };
        };
      };
      remotes = mkOption {
        description = "Ordered list of repositories to pull.";
        type = listOf (submodule {
          options = {
            name = mkOption {
              type = str;
              description = ''
                The name of the remote.
              '';
            };
            url = mkOption {
              type = str;
              description = ''
                The URL of the repository.
              '';
            };
            auth = mkOption {
              description = "Authentication options.";
              default = {};
              type = submodule {
                options = {
                  access_token_path = mkOption {
                    type = str;
                    default = "";
                    description = ''
                      The path of the auth file.
                    '';
                  };
                  username = mkOption {
                    type = str;
                    default = "comin";
                    description = ''
                      The username used to authenticate to the Git
                      remote repository. Note that any non empty
                      username is valid on GitLab and GitHub.
                    '';
                  };
                };
              };
            };
            timeout = mkOption {
              type = int;
              default = 300;
              description = ''
                Git fetch timeout in seconds.
              '';
            };
            branches = mkOption {
              description = "Branches to pull.";
              default = {};
              type = submodule {
                options = {
                  main = mkOption {
                    default = {};
                    description = "The main branch to fetch.";
                    type = submodule {
                      options = {
                        name = mkOption {
                          type = str;
                          default = "main";
                          description = "The name of the main branch.";
                        };
                        operation = mkOption {
                          type = enum ["switch" "test" "boot"];
                          default = "switch";
                          description = "The switch-to-configuration operation to do on this branch.";
                        };
                      };
                    };
                  };
                  testing = mkOption {
                    default = {};
                    description = "The testing branch to fetch.";
                    type = submodule {
                      options = {
                        name = mkOption {
                          type = str;
                          default = "testing-${config.services.comin.hostname}";
                          defaultText = lib.literalExpression "testing-\${config.services.comin.hostname}";
                          description = "The name of the testing branch.";
                        };
                        operation = mkOption {
                          type = enum ["switch" "test" "boot"];
                          default = "test";
                          description = "The switch-to-configuration operation to do on this branch.";
                        };
                      };
                    };
                  };
                };
              };
            };
            poller = mkOption {
              default = {};
              description = "The poller options.";
              type = submodule {
                options = {
                  period = mkOption {
                    type = types.int;
                    default = 60;
                    description = ''
                      The poller period in seconds.
                    '';
                  };
                };
              };
            };
          };
        });
      };
      debug = mkOption {
        type = types.bool;
        default = false;
        description = ''
          Whether to run comin in debug mode. Be careful, secrets are shown!.
        '';
      };
      machineId = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = ''
          The expected machine-id of the machine configured by
          comin. If not null, the configuration is only deployed
          when this specified machine-id is equal to the actual
          machine-id.
          This is mainly useful for server migration: this allows
          to migrate a configuration from a machine to another
          machine (with different hardware for instance) without
          impacting both.
          Note it is only used by comin at evaluation.
        '';
      };
      gpgPublicKeyPaths = mkOption {
        description = "A list of GPG public key file paths. Each of this file should contains an armored GPG key.";
        type = listOf str;
        default = [];
      };
      postDeploymentCommand = mkOption {
        description = "A path to a script executed after each
        deployment. comin provides to the script the following
        environment variables: `COMIN_GIT_SHA`, `COMIN_GIT_REF`,
        `COMIN_GIT_MSG`, `COMIN_HOSTNAME`, `COMIN_FLAKE_URL`,
        `COMIN_GENERATION`, `COMIN_STATUS`, `COMIN_ERROR_MSG` and
        `COMIN_OPERATION` (the deployment's operation: `switch`, `test`
        or `boot`).";
        type = nullOr path;
        default = null;
        example = lib.literalExpression ''
          pkgs.writers.writeBash "post" "echo $COMIN_GIT_SHA";
        '';
      };
      overrideLeaseFile = mkOption {
        description = "The path to a developer override lease file owned
        by another tool. comin reads only whether the file exists and its
        top-level `kind` field; it never parses any other field. While
        the file exists comin deploys no main-branch generation on its
        own; it holds back the newest one. A new testing-branch head
        deploys only with no lease or a lease of kind `git`, and under a
        `git` lease only when it descends from the running main commit.
        Once the file is gone and no deployment is queued or running,
        comin releases every testing deployment it made while this option
        was set: it never deploys their testing-branch heads again, and it
        never switches the machine back on its own. comin also deploys
        only over a system it owns: the last successful deployment (of
        the main branch, or under a `git` lease of either branch) or, when
        there is none yet, any system; or the system its own latest
        deployment activated, a failed one included. A released testing
        deployment is never comin's. Over any other running system comin
        deploys nothing on its own, not even a newer main-branch commit or
        a new testing-branch head, and `comin status --json` reports the
        drift; only `comin deployment switch-latest` deploys over it. That
        covers an override ended without a reboot, a break-glass or
        closure activated out of band, and a rollback made by another
        tool. Deployments resume by themselves once comin owns the system
        again: the running system is the last successful main-branch
        deployment again (after a reboot back to it, for example), or an
        operator runs `comin deployment switch-latest`. comin then deploys
        the newest fetched main-branch commit, unless it is the one
        running, within one poll and one fetch, across a comin restart too
        — except when a testing-branch head comin refused (fetched while a
        non-git lease was held, or while comin did not own the running
        system) descends from that main-branch commit: then the machine
        stays on its current main-branch deployment — `comin status --json`
        reports no drift — until the main or the testing branch moves
        again. A released testing-branch head
        is never selected for deployment, so it does not hold the main
        branch back while it is still the head of its branch, on the
        remote or only in comin's clone. If comin's lease state file is
        unreadable, comin never deploys the testing-branch head of a
        testing deployment made before again, and once no lease is held
        each such deployment counts as released. `null`
        disables only these lease behaviours. Whatever this option is set
        to, `comin status --json` reports `drift`, the post-deployment
        command gets `COMIN_OPERATION`, and `comin deployment
        switch-latest` deploys the last successful main-branch deployment
        (or, under a `git` lease, the last testing one) with `switch`,
        including after a restart.";
        type = nullOr str;
        default = null;
        example = "/opt/pattern/override.json";
      };
      buildConfirmer = mkOption {
        description = "The confirmer options for the build.";
        default = {};
        type = submodule {
          options = {
            mode = mkOption {
              type = enum [ "without" "auto" "manual" ];
              default = "without";
              description = ''
                The confirmer mode. "without" immediately confirms
                without any user interaction. "manual" requires a user
                confirmation. "auto" automatically confirms after
                waiting for the autoconfirm_duration.
              '';
            };
            autoconfirm_duration = mkOption {
              type = int;
              default = 120;
              description = ''
                The autoconfirm timer duration in seconds. After this
                duration, the action is automatically confirmed.
              '';
            };

          };
        };
      };
      deployConfirmer = mkOption {
        description = "The confirmer options for the deployment.";
        default = {};
        type = submodule {
          options = {
            mode = mkOption {
              type = enum [ "without" "auto" "manual" ];
              default = "without";
              description = ''
                The confirmer mode. "without" immediately confirms
                without any user interaction. "manual" requires a user
                confirmation. "auto" automatically confirms after
                waiting for the autoconfirm_duration.
              '';
            };
            autoconfirm_duration = mkOption {
              type = int;
              default = 120;
              description = ''
                The autoconfirm timer duration in seconds. After this
                duration, the action is automatically confirmed.
              '';
            };

          };
        };
      };
      desktop = {
        enable = mkEnableOption "Whether to run the comin desktop service. This user service send notifications over DBus.";
        title = mkOption {
          type = str;
          default = "comin";
          description = "The notification title.";
        };
      };
    };
  };
}
