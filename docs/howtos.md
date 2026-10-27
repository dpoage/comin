## How to test a NixOS configuration change

TLDR: push a commit to the `testing-<hostname>` branch (rebased on the
`main` branch) to deploy a change to the machine named `<hostname>`.

By default, each machine pulls configuration from the branch
`testing-<hostname>`. When this branch is on top of the `main` branch,
comin deploys this configuration by running `switch-to-configuration
test`: the bootloader configuration is not modified.

To test a configuration:

1. Create a `testing-<hostname>` branch in your configuration
   repository on top of the `main` branch
2. Add new commits to this branch and push it
3. comin runs `switch-to-configuration test` on the configuration: the bootload is not updated

Contrary to the main branch, this branch can be hard reset but always
has to be on top of the `main` branch.

To `nixos-rebuild switch` to this configuration, the `main` branch has
to be rebased on the `testing` branch.

## Iterate faster with local repository

By default, comin polls remotes every 60 seconds. You could however
add a local repository as a comin remote: comin could then poll this
branch every second. When you commit to this repository, comin is
starting to deploy the new configuration immediately.

However, be careful because this repository could then be used by an
attacker to update your machine.

Example of a configuration with a local repository:

```nix
services.comin = {
  enable = true;
  remotes = [
    {
      name = "local";
      url = "/your/local/infra/repository";
      poller.period = 2;
    }
  ];
}
```

## How to migrate a configuration from a machine to another one

Suppose you have a running NixOS machine and you want to move this
configuration to another machine while preserving the same
hostname. If you use a testing branch, both of these machines will be
updated when changes are pushed to the testing branch. 

To avoid such situation, we could set the option
`services.comin.machineId`. If the machine where comin is running
doesn't have this expected `machine-id` (compared to the content of
the `/etc/machine-id` file), comin won't deploy the configuration.

So, to migrate to another machine, you have to update this
option in the `testing-<hostname>` branch in order to only deploy this
configuration to the new machine.

## Check Git commit signatures

The option `services.comin.gpgPublicKeyPaths` allows to declare a list
of GPG public keys. If `services.comin.gpgPublicKeyPaths != []`, comin **only** evaluates commits signed
by one of these GPG keys. Note only the last commit needs to be signed.

The file containing a GPG public key has to be created with `gpg --armor  --export alice@cyb.org`.


## How to deploy a nix-darwin configuration

When comin is running on a Darwin system, it automatically builds and
deploys a configuration found in the flake output
`darwinConfigurations.hostname`. So, you only need to set this flake
output and run comin on the target machine.


## How to use comin without Nix flake

comin supports deploying configuration from a repository that doesn't
use Nix flake. Here is a configuration example:

```nix
services.comin = {
  repositoryType = "nix";
  systemAttr = "your.nixos.configuration.attribute";
  ...
  ...
}
```

Please note this is currently not supported by for nix-darwin configurations.

## How comin reconciles the running system with git

comin is level-triggered: on every tick (30 seconds) and after every
fetch, build and deployment, it compares the running system
(`/run/current-system`) with the configuration the repository says
should run, and deploys it when they differ.

The candidates are the head of the `testing-<hostname>` branch (when
it is selected) and then the head of the `main` branch. comin only
evaluates and builds the head of the `main` branch when the testing
head is refused or failed. The first candidate admitted by the
`services.comin.preflightCommand` is the one comin deploys, with the
operation configured for its branch. When no command is configured,
every candidate is admitted.

A candidate whose latest deployment of the same system failed is not
deployed again until the repository selects a different system.

**Fork behaviour**: when `services.comin.preflightCommand` is `null`,
comin reverts a change made outside of comin (`nixos-rebuild`, a
closure activated by hand, ...) at the next tick, without any new
commit. Upstream comin only deploys when a new commit is fetched. To
let a machine run something else than the repository, set a preflight
command that refuses the candidates while that is wanted.
