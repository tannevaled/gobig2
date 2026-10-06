# TOOLS

Platform-agnostic tools available and encouraged for use in
this project.

## Canonical entry point: Taskfile

[Taskfile.yml](../Taskfile.yml) is the canonical command runner.
Always prefer `task <recipe>` over invoking `go test` / `golangci-lint`
directly - the recipes encode the project's flag conventions (build
tags, coverage profile paths, fuzz-time defaults, race-detector env)
that bare commands miss.

Install task once:

```sh
go install github.com/go-task/task/v3/cmd/task@v3.50.0
```

Discover all recipes:

```sh
task --list
```

## Common workflows

### Correctness gate (run before declaring a change done)

```sh
task check          # modverify + vet + test + lint + vuln
```

CI runs the wider gate:

```sh
task ci             # fmt:check + check + test:race + test:scripts
```

### Tests

```sh
task test                      # all packages, no race detector
task test:race                 # CGO_ENABLED=1, full race detector
task test:conformance          # SerenityOS corpus (always) + ITU-T T.88 Annex A (env-gated)
task cover                     # coverage summary
task cover:func                # per-function coverage
task cover:html                # HTML report (opens browser)
```

#### Conformance corpus env vars

`task test:conformance` consumes two pass-through env vars; both
are intentionally **never defaulted to a maintainer-local path**
so a fresh clone can't silently skip everything:

- `JBIG2_SERENITYOS_DIR` - optional override for the SerenityOS
  corpus. Unset = use bundled [testdata/serenityos/](../testdata/serenityos/),
  which always runs.
- `JBIG2_CONFORMANCE_DIR` - optional path to the
  `JBIG2_ConformanceData-A20180829` corpus from ITU-T T.88 Annex A.
  Unset = the test skips cleanly (the corpus is too large to
  commit). See [docs/design/ITU-SPEC-PROBLEMS.md](design/ITU-SPEC-PROBLEMS.md)
  for the per-TT pass/fail matrix.

Run a single test in one package:

```sh
go test -run '^TestSerenityOSCorpus$/sample-name' ./internal/gobig2test
```

### Lint & format

```sh
task fmt              # apply every formatter in .golangci.yml (gofumpt + goimports)
task fmt:check        # CI-friendly: fail if any file would be reformatted
task lint             # golangci-lint v2 against .golangci.yml
task lint:fix         # auto-apply lint fixes
```

The same formatters that `task lint` checks are what `task fmt`
rewrites - calling individual formatters by hand risks asymmetry
where `task fmt` produces code `task lint` then rejects.

### Fuzz

```sh
task fuzz                      # 3s smoke run across every Fuzz* target (default FUZZTIME=3s)
task fuzz:long                 # 10m sustained run (FUZZTIME=10m default; pass FUZZTIME=1h overnight)
```

Fuzz-found inputs land in `internal/<pkg>/testdata/fuzz/<TestName>/<hash>`.
Commit the failing ones - `go test` (without `-fuzz`) replays them
as regression seeds.

### Build

```sh
task build                                  # compile-check ./...
task build:release VERSION=v1.2.3           # stripped binaries to ./bin/, injects main.version
```

### Supply chain

```sh
task vuln           # govulncheck CVE scan
task modverify      # verify go.sum matches downloaded module zips
```

### Bench

```sh
task bench          # go test -bench=. -benchmem -run=^$ ./...
task bench:cross    # wall-clock bench vs jbig2dec / mutool / pdfimages / pdfbox
task bench:corpus   # synthesize ~600 dpi JBIG2 fixtures under ./tmp/perf-corpus/
```

`task bench:cross` builds the gobig2 CLI and the
[cmd/perf-cross/](../cmd/perf-cross/) orchestrator, then times
each installed decoder over a curated SerenityOS subset.
Decoders missing from `PATH` (jbig2dec, mutool, pdfimages) get a
"skip" cell instead of failing the run, so the recipe is
callable on any host as a gobig2-only smoke test. PDFBox is
opt-in via `PDFBOX_JAR=path/to/pdfbox-app.jar`. Output lands in
`./tmp/perf/cross-decoder.{md,json}`.

The bundled SerenityOS fixtures decode in 1-10 ms - too fast to
isolate decode-loop performance from subprocess setup. Run
`task bench:corpus` (Linux/macOS only; needs `jbig2enc` +
`imagemagick` from apt / brew) to generate ~600 dpi A4 fixtures
under `./tmp/perf-corpus/`, then point the bench at them:

```sh
task bench:cross CORPUS_DIR=./tmp/perf-corpus
```

The same orchestrator is invoked by
[.github/workflows/perf-linux.yml](../.github/workflows/perf-linux.yml),
which installs the C / Java decoders on `ubuntu-24.04`, runs
[scripts/perf/synthesize-corpus.sh](../scripts/perf/synthesize-corpus.sh)
(cached behind `actions/cache`), and publishes the Markdown
table to the run's job summary.

### Tooling install / refresh

```sh
task tools          # install pinned goimports, golangci-lint, govulncheck
```

Re-run after bumping `go.mod`'s `go` directive - `golangci-lint`
and `govulncheck` refuse to scan code targeting a newer Go than
they were built against. `GOTOOLCHAIN=go1.27.1` in the recipe
keeps tool binaries pinned to the version go.mod's `go`
directive declares.

## Other tooling notes

- [.golangci.yml](../.golangci.yml) - golangci-lint **v2** schema. If
  bumping past v2 someday, run `golangci-lint migrate` to convert
  the config in place.
