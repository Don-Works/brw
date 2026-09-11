# Homebrew tap

`brw.rb` is the template for the formula published in the `Don-Works/homebrew-tap`
repository. It targets the release tarballs built by `scripts/package-tarball.sh`,
which unpack anywhere and need no privileged step. The `.pkg` remains a separate
path and is not shipped as a cask.

## The tap repository

The tap lives in a repository of its own. Homebrew resolves
`don-works/tap` to `github.com/Don-Works/homebrew-tap`, so the name must be
exactly `homebrew-tap`:

```text
Don-Works/homebrew-tap
  Formula/
    brw.rb
  README.md
```

With that in place:

```sh
brew install don-works/tap/brw
```

`brew tap don-works/tap` first is optional; the three-part name taps implicitly.

## Bumping the formula for a release

`render-formula.sh` prints the formula with the version and the four archive
sha256 sums substituted. It reads the sums from `dist/release` when the archives
were just built locally, and otherwise from the published release's
`SHA256SUMS.txt`.

```sh
make homebrew-formula VERSION=0.11.0 > /path/to/homebrew-tap/Formula/brw.rb
```

Then commit and push that file in the tap repository. Nothing else in the tap
changes between releases.

## Verifying a bump before pushing

```sh
brew install --build-from-source /path/to/homebrew-tap/Formula/brw.rb
brew test brw
brew audit --strict --formula /path/to/homebrew-tap/Formula/brw.rb
```

`brew audit` warns about the non-standard top-level directories the formula
installs (`extension/`, `tests/`, `skills/`, `doc/`). That layout is deliberate:
it makes `$(brew --prefix brw)` a valid `brwctl --app-dir`, which is what
`brwctl doctor` checks.
