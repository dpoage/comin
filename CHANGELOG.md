# Changelog

## Unreleased

### Added

- `services.comin.preflightCommand`: an absolute path to a command run before
  comin activates a candidate and on every reconcile tick for each candidate
  comin considers. Exit 0 admits the candidate; anything else (nonzero, killed,
  cannot start, timeout) refuses it. The first stdout line is the refusal reason
  shown by `comin status`. When this option is null, comin admits every
  candidate and this fork reverts any out-of-band change of the running system
  at the next reconcile tick. Default: null.
- `services.comin.preflightTimeout`: seconds before comin kills the preflight
  run (its process group) and counts it as a refusal. Default: 60.
- The configuration file keys `preflight_command` and `preflight_timeout`.
- The `reconcile` object in the state `comin status` reports (JSON included):
  `state` (`converged`, `waiting`, `deploying` or `failed`), `desired`
  (commit, branch, out path), `current`, `reason` (the preflight line or
  the comin line explaining the state), `since` (when this process first
  observed the state value), `observed_at` (when the reconcile pass that
  produced the object started, on the host clock; null before the first
  pass) and `candidates`, the current walk in
  preference order with one entry per candidate: branch, kind
  (`main`/`testing`), commit, out path, outcome (`admitted`, `refused`,
  `failed`, `pending` or `skipped`) and reason. `comin status` and
  `comin status --oneline` show the reconcile state and its reason.

### Removed

- The `boot` operation for `services.comin.remotes.*.branches.*.operation`.
  comin converges the running system; an operation that does not activate it
  never converges. Configurations that set `boot` now fail with an evaluation
  error.

### Changed

- `comin deployment switch-latest` reconciles now instead of re-deploying
  the last deployment with the `switch` operation: comin runs a reconcile
  pass at once, the preflight asks of the passes that serve the request
  carry `COMIN_TRIGGER=manual`, and a candidate whose latest deployment of
  the same out path failed is retried once. The command fails when the
  repository offers no candidate and returns without waiting for any
  deployment.
- `branches.*.operation` accepts only `switch` or `test`. The NixOS module
  rejects every other value with a type error naming the option. The
  configuration file reader rejects every other value with an error naming the
  branch, the option and the reason. Default operations (`switch` on main,
  `test` on testing) are unchanged.
- comin no longer checks out a commit in its repository after a fetch: a fetch
  never writes the repository's working tree or `.git/HEAD`, so a concurrent
  `nix eval git+file://…?rev=…` cannot read a half-written HEAD. The signature
  verdict `selected_commit_signed*` is the verdict of the selected commit, as
  before. When comin starts on a repository whose `.git/HEAD` an earlier
  version left detached at a commit, it points HEAD back to the unborn branch
  of a fresh repository, once, before its first fetch. The non-flake
  evaluation then clones the selected commit, with its history, whatever
  commit an earlier version last checked out.
