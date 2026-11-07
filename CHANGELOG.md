# Changelog

## Unreleased

### Added

- `services.comin.preflightCommand`: an absolute path to a command run before
  comin activates a candidate and on every reconcile tick for each candidate
  comin considers. Exit 0 admits the candidate; anything else (nonzero, killed,
  cannot start, timeout) refuses it. The refusal reason shown by
  `comin status` is the first stdout line when it is not empty (cut to 4096
  bytes, invalid UTF-8 replaced), else comin's own `preflight: <cause>` line.
  When this option is null, comin admits every
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
- `services.comin.syncGateCommand`: an absolute path to a command that tells
  comin whether it may fetch, evaluate, build and deploy now. comin runs it
  with `COMIN_TRIGGER` (`poll` or `manual`) and `COMIN_SYNC_STAGE`: `fetch`
  before every fetch, `pass` at the start of every reconcile pass and `watch`
  every 2 seconds while an evaluation or a build runs. Exit 0 is open; any
  other outcome is closed, with the first stdout line as the reason. A closed
  `fetch` is skipped without a fetch error. A closed `pass` takes no
  evaluation, build, ask or deployment step (status `waiting`, reason
  `sync gate: <line>`), ends a `comin deployment switch-latest` request, and,
  like a closed `watch`, stops a running evaluation or build without failing
  it; the candidate is built again (evaluated again when its evaluation was
  stopped) at the first open pass. A deployment already handed to the
  deployer is not interrupted. When this option is null
  comin has no gate. Default: null.
- `services.comin.syncGateTimeout`: seconds before comin kills a sync gate
  run and counts it as closed. Default: 30.
- The configuration file key `sync_gate` with `command` and `timeout`.
- The `manual` object in the `reconcile` object of `comin status --json`:
  the last `comin deployment switch-latest` request's `requested_at`,
  `ended_at` (null while it waits for its decisive pass), `state` and
  `reason`. It is absent until the first request since comin started, and
  kept in memory only; the state file is unchanged.

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
- A candidate whose evaluation or build failed is evaluated again after a
  completed fetch (whether or not it brought a new commit) and after a
  `comin deployment switch-latest` request, at most once per signal: signals
  that arrive together are coalesced, and a fetch completing while the
  attempt runs is covered by it. A tick never retries it. A re-attempt waits
  while the builder reports an evaluation or build running, whichever
  candidate it belongs to, or while a candidate still offered is evaluating,
  building, or awaiting its build or its build confirmation (under a manual
  build confirmer, an unconfirmed build holds it until confirmed), so it
  never cancels or repeats another candidate's evaluation or build. A
  candidate no longer offered holds it only through a builder run still in
  progress. While it is evaluated again its status is `pending`. A
  machine-id mismatch is not retried.
