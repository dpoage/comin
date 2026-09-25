## assertions

*Type:*
anything



## services\.comin\.enable



Whether to run the comin service\.



*Type:*
boolean



*Default:*
` false `



## services\.comin\.package



The comin package to use\.



*Type:*
null or package



*Default:*
` "pkgs.comin or comin.packages.\${system}.default or null" `



## services\.comin\.buildConfirmer



The confirmer options for the build\.



*Type:*
submodule



*Default:*
` { } `



## services\.comin\.buildConfirmer\.autoconfirm_duration



The autoconfirm timer duration in seconds\. After this
duration, the action is automatically confirmed\.



*Type:*
signed integer



*Default:*
` 120 `



## services\.comin\.buildConfirmer\.mode



The confirmer mode\. “without” immediately confirms
without any user interaction\. “manual” requires a user
confirmation\. “auto” automatically confirms after
waiting for the autoconfirm_duration\.



*Type:*
one of “without”, “auto”, “manual”



*Default:*
` "without" `



## services\.comin\.debug



Whether to run comin in debug mode\. Be careful, secrets are shown!\.



*Type:*
boolean



*Default:*
` false `



## services\.comin\.deployConfirmer



The confirmer options for the deployment\.



*Type:*
submodule



*Default:*
` { } `



## services\.comin\.deployConfirmer\.autoconfirm_duration



The autoconfirm timer duration in seconds\. After this
duration, the action is automatically confirmed\.



*Type:*
signed integer



*Default:*
` 120 `



## services\.comin\.deployConfirmer\.mode



The confirmer mode\. “without” immediately confirms
without any user interaction\. “manual” requires a user
confirmation\. “auto” automatically confirms after
waiting for the autoconfirm_duration\.



*Type:*
one of “without”, “auto”, “manual”



*Default:*
` "without" `



## services\.comin\.desktop\.enable



Whether to enable Whether to run the comin desktop service\. This user service send notifications over DBus…



*Type:*
boolean



*Default:*
` false `



*Example:*
` true `



## services\.comin\.desktop\.title



The notification title\.



*Type:*
string



*Default:*
` "comin" `



## services\.comin\.exporter



Options for the Prometheus exporter\.



*Type:*
submodule



*Default:*
` { } `



## services\.comin\.exporter\.listen_address



Address to listen on for the Prometheus exporter\. Empty string will listen on all interfaces\.



*Type:*
string



*Default:*
` "" `



## services\.comin\.exporter\.openFirewall



Open port in firewall for incoming connections to the Prometheus exporter\.



*Type:*
boolean



*Default:*
` false `



## services\.comin\.exporter\.port



Port to listen on for the Prometheus exporter\.



*Type:*
signed integer



*Default:*
` 4243 `



## services\.comin\.gpgPublicKeyPaths



A list of GPG public key file paths\. Each of this file should contains an armored GPG key\.



*Type:*
list of string



*Default:*
` [ ] `



## services\.comin\.hostname



The name of the configuration to evaluate and deploy\.
This value is used by comin to evaluate the flake output
nixosConfigurations\.“\<hostname>” or darwinConfigurations\.“\<hostname>”\.
Defaults to networking\.hostName - you MUST set either this option
or networking\.hostName in your configuration\.



*Type:*
string



*Default:*
` config.networking.hostName `



## services\.comin\.machineId



The expected machine-id of the machine configured by
comin\. If not null, the configuration is only deployed
when this specified machine-id is equal to the actual
machine-id\.
This is mainly useful for server migration: this allows
to migrate a configuration from a machine to another
machine (with different hardware for instance) without
impacting both\.
Note it is only used by comin at evaluation\.



*Type:*
null or string



*Default:*
` null `



## services\.comin\.overrideLeaseFile



The path to a developer override lease file owned
by another tool\. comin reads only whether the file exists and its
top-level ` kind ` field; it never parses any other field\. While
the file exists comin deploys no main-branch generation on its
own; it holds back the newest one\. A new testing-branch head
deploys only with no lease or a lease of kind ` git `, and under a
` git ` lease only when it descends from the running main commit\.
Once the file is gone and no deployment is queued or running,
comin releases every testing deployment it made while this option
was set: it never deploys their testing-branch heads again, and it
never switches the machine back on its own\. comin also deploys
only over a system it owns: the last successful deployment (of
the main branch, or under a ` git ` lease of either branch) or, when
there is none yet, any system; or the system its own latest
deployment activated, a failed one included\. A released testing
deployment is never comin’s\. Over any other running system comin
deploys nothing on its own, not even a newer main-branch commit or
a new testing-branch head, and ` comin status --json ` reports the
drift; only ` comin deployment switch-latest ` deploys over it\. That
covers an override ended without a reboot, a break-glass or
closure activated out of band, and a rollback made by another
tool\. Deployments resume by themselves once comin owns the system
again: the running system is the last successful main-branch
deployment again (after a reboot back to it, for example), or an
operator runs ` comin deployment switch-latest `\. comin then deploys
the newest fetched main-branch commit, unless it is the one
running, within one poll and one fetch, across a comin restart too
— except when a testing-branch head comin refused (for example,
fetched while a non-git lease was held or while comin did not own
the running system) descends from that main-branch commit: then
the machine stays on its current main-branch deployment — ` comin         status --json ` reports no drift — until the main or the testing
branch moves again, or until comin restarts (a reboot included)
while it owns the system, which then deploys that testing-branch
head with test\. A released testing-branch head
is never selected for deployment, so it does not hold the main
branch back while it is still the head of its branch, on the
remote or only in comin’s clone\. If comin’s lease state file is
unreadable, comin never deploys the testing-branch head of a
testing deployment made before again, and once no lease is held
each such deployment counts as released\. ` null `
disables only these lease behaviours\. Whatever this option is set
to, ` comin status --json ` reports ` drift `, the post-deployment
command gets ` COMIN_OPERATION `, and ` comin deployment         switch-latest ` deploys the last successful main-branch deployment
(or, under a ` git ` lease, the last testing one) with ` switch `,
including after a restart\.



*Type:*
null or string



*Default:*
` null `



*Example:*
` "/opt/pattern/override.json" `



## services\.comin\.postDeploymentCommand



A path to a script executed after each
deployment\. comin provides to the script the following
environment variables: ` COMIN_GIT_SHA `, ` COMIN_GIT_REF `,
` COMIN_GIT_MSG `, ` COMIN_HOSTNAME `, ` COMIN_FLAKE_URL `,
` COMIN_GENERATION `, ` COMIN_STATUS `, ` COMIN_ERROR_MSG ` and
` COMIN_OPERATION ` (the deployment’s operation: ` switch `, ` test `
or ` boot `)\.



*Type:*
null or absolute path



*Default:*
` null `



*Example:*

```
pkgs.writers.writeBash "post" "echo $COMIN_GIT_SHA";

```



## services\.comin\.remotes



Ordered list of repositories to pull\.



*Type:*
list of (submodule)



## services\.comin\.remotes\.\*\.auth



Authentication options\.



*Type:*
submodule



*Default:*
` { } `



## services\.comin\.remotes\.\*\.auth\.access_token_path



The path of the auth file\.



*Type:*
string



*Default:*
` "" `



## services\.comin\.remotes\.\*\.auth\.username



The username used to authenticate to the Git
remote repository\. Note that any non empty
username is valid on GitLab and GitHub\.



*Type:*
string



*Default:*
` "comin" `



## services\.comin\.remotes\.\*\.branches



Branches to pull\.



*Type:*
submodule



*Default:*
` { } `



## services\.comin\.remotes\.\*\.branches\.main



The main branch to fetch\.



*Type:*
submodule



*Default:*
` { } `



## services\.comin\.remotes\.\*\.branches\.main\.name



The name of the main branch\.



*Type:*
string



*Default:*
` "main" `



## services\.comin\.remotes\.\*\.branches\.main\.operation



The switch-to-configuration operation to do on this branch\.



*Type:*
one of “switch”, “test”, “boot”



*Default:*
` "switch" `



## services\.comin\.remotes\.\*\.branches\.testing



The testing branch to fetch\.



*Type:*
submodule



*Default:*
` { } `



## services\.comin\.remotes\.\*\.branches\.testing\.name



The name of the testing branch\.



*Type:*
string



*Default:*
` testing-${config.services.comin.hostname} `



## services\.comin\.remotes\.\*\.branches\.testing\.operation



The switch-to-configuration operation to do on this branch\.



*Type:*
one of “switch”, “test”, “boot”



*Default:*
` "test" `



## services\.comin\.remotes\.\*\.name



The name of the remote\.



*Type:*
string



## services\.comin\.remotes\.\*\.poller



The poller options\.



*Type:*
submodule



*Default:*
` { } `



## services\.comin\.remotes\.\*\.poller\.period



The poller period in seconds\.



*Type:*
signed integer



*Default:*
` 60 `



## services\.comin\.remotes\.\*\.timeout



Git fetch timeout in seconds\.



*Type:*
signed integer



*Default:*
` 300 `



## services\.comin\.remotes\.\*\.url



The URL of the repository\.



*Type:*
string



## services\.comin\.repositorySubdir



Subdirectory in the repository, containing a default\.nix or a flake\.nix file\.



*Type:*
string



*Default:*
` "." `



## services\.comin\.repositoryType



The type of the repository to fetch\. It can either contains a flake or a classical Nix expression\.



*Type:*
one of “flake”, “nix”



*Default:*
` "flake" `



## services\.comin\.systemAttr



This is the attribute containing the machine toplevel
attribute\. Note this is only used when the repositoryType is
‘nix’\. When the repository type is ‘flake’, the attribute is
derived from the hostname\.



*Type:*
null or string



*Default:*
` null `


