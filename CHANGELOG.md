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

### Removed

- The `boot` operation for `services.comin.remotes.*.branches.*.operation`.
  comin converges the running system; an operation that does not activate it
  never converges. Configurations that set `boot` now fail with an evaluation
  error.

### Changed

- `branches.*.operation` accepts only `switch` or `test`. The NixOS module
  rejects every other value with a type error naming the option. The
  configuration file reader rejects every other value with an error naming the
  branch, the option and the reason. Default operations (`switch` on main,
  `test` on testing) are unchanged.
- comin no longer checks out a commit in its repository after a fetch: a fetch
  never writes the repository's working tree or `.git/HEAD`, so a concurrent
  `nix eval git+file://…?rev=…` cannot read a half-written HEAD. The signature
  verdict `selected_commit_signed*` is the verdict of the selected commit, as
  before. The non-flake evaluation clones the selected commit from the `archive`
  reference instead of the repository's HEAD.
