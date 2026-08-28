> **This is a fork of [dkrisman/gobig2](https://github.com/dkrisman/gobig2), not a competing project.**
>
> It exists so the go-gfx and go-pdfkit fleets can depend on a *tagged* version carrying one fix while that fix is offered upstream:
>
> - **[dkrisman/gobig2#2](https://github.com/dkrisman/gobig2/pull/2)** — the per-symbol pixel cap defaulted below what real scanned documents contain, refusing 7 of 403 JBIG2 streams taken from public Internet Archive scans. It is raised to the aggregate cap, which already bounds the work.
> - **[dkrisman/gobig2#1](https://github.com/dkrisman/gobig2/issues/1)** — the remaining report: `Limits.Apply()` writes process-global state, so a library cannot use it.
>
> The module path is renamed only so it can be imported. **Use upstream if you can.** When the fix lands there and upstream publishes a tag, this fork goes away.
>
> All credit for the decoder is upstream's. Measured against poppler over 403 real streams, it is the only pure-Go JBIG2 decoder of four tested that never decoded a stream wrongly.

# gobig2

[![CI](https://github.com/dkrisman/gobig2/actions/workflows/ci.yml/badge.svg)](https://github.com/dkrisman/gobig2/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dkrisman/gobig2.svg)](https://pkg.go.dev/github.com/dkrisman/gobig2)
[![Go Report Card](https://goreportcard.com/badge/github.com/dkrisman/gobig2)](https://goreportcard.com/report/github.com/dkrisman/gobig2)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Pure-Go decoder for ITU-T T.88 / ISO/IEC 14492 **JBIG2** streams.

`gobig2` was built first for PDF readers - the `/JBIG2Decode`
filter is the dominant use of JBIG2 in the wild - and then for
general-purpose standalone `.jb2` / `.jbig2` decoding. No cgo,
no third-party runtime dependencies; every length, count, and
dimension that derives from input bytes is gated against a
configurable [`Limits`](limits.go) cap before allocation, so the
codec is safe to feed adversarial bytes from a PDF crawler or
similar untrusted source.

> [!WARNING]
> Pre-1.0. The public API is settling but not yet frozen; the
> module version is `"0.0.0-dev"` until the first tagged release.
> Conformance status against the ITU-T T.88 Annex A corpus is
> documented in [docs/design/ITU-SPEC-PROBLEMS.md](docs/design/ITU-SPEC-PROBLEMS.md) -
> the short version is that TT1, TT9, TT10 decode and TT2-TT8 fail
> for reasons shared with every other open-source JBIG2 decoder
> (the corpus encoder ships spec-deviating shapes no production
> encoder emits).

## Performance

Cross-decoder wall-clock benchmark on `ubuntu-24.04` (GitHub-hosted
runner), best-of-7 with one warm-up, decoded straight to PBM so
the cell tracks decode work rather than encoder overhead. Numbers
in milliseconds; the per-push run lives at
[.github/workflows/perf-linux.yml](.github/workflows/perf-linux.yml).

| fixture                          | gobig2 | jbig2dec | mutool | pdfimages |
| -------------------------------- | -----: | -------: | -----: | --------: |
| `bitmap`                         |   2.71 |     1.78 |   4.19 |     10.08 |
| `bitmap-mmr`                     |   2.26 |     1.28 |   3.58 |      8.53 |
| `bitmap-halftone`                |   2.53 |     1.65 |   3.73 |      8.81 |
| `bitmap-symbol`                  |   1.94 |     1.29 |   3.67 |      8.65 |
| `bitmap-symbol-symhuff-texthuff` |   2.05 |     2.12 |   4.43 |      8.86 |
| `perf-text-generic` (33 Mpx)     | 231.81 |   175.12 | 252.60 |    313.08 |
| `perf-text-symbol` (33 Mpx)      |  25.94 |    18.31 |  95.38 |     57.33 |

- Pure Go, no asm, no cgo - within **1.3-1.6x** of jbig2dec (C,
  hand-tuned reference) across every fixture.
- Beats jbig2dec on `bitmap-symbol-symhuff-texthuff` and beats
  every PDF-toolchain decoder on every fixture.

Reproduce locally (Linux / macOS, needs `jbig2enc` + `imagemagick`
for fixture synthesis):

```sh
task bench:corpus            # generate the perf-text-* fixtures under ./tmp/perf-corpus/
task bench:cross CORPUS_DIR=./tmp/perf-corpus
```

## Install

```sh
go get github.com/dkrisman/gobig2
```

Go 1.25 toolchain or newer (see [go.mod](go.mod)).

## Quickstart - PDF-embedded stream

The canonical flow a PDF reader uses: pull the
`JBIG2Decode`-filtered image XObject stream and the optional
`/JBIG2Globals` parameter object out of the PDF, hand both to
`NewDecoderEmbedded`, and decode the page bitmap.

```go
package main

import (
    "bytes"
    "fmt"
    "image"
    "os"

    "github.com/dkrisman/gobig2"
)

func main() {
    imageStream, err := os.ReadFile("page.jb2")
    if err != nil {
        panic(err)
    }
    var globalsBytes []byte // pulled from /JBIG2Globals if present, else nil

    dec, err := gobig2.NewDecoderEmbedded(bytes.NewReader(imageStream), globalsBytes)
    if err != nil {
        // Adversarial / non-JBIG2 input is rejected up front,
        // before any allocation derived from declared dimensions.
        fmt.Println("decode error:", err)
        return
    }

    img, err := dec.Decode()
    if err != nil {
        fmt.Println("decode error:", err)
        return
    }

    // *image.Gray with ink as 0 (black), paper as 255 (white).
    g := img.(*image.Gray)
    fmt.Printf("decoded %dx%d\n", g.Bounds().Dx(), g.Bounds().Dy())
}
```

See [example_pdf_test.go](example_pdf_test.go) for the same flow
as a runnable `Example`.

## Public API

### Constructors

JBIG2 has two on-the-wire forms - pick the constructor that
matches your input:

| Input shape | Constructor |
|---|---|
| Standalone `.jb2` / `.jbig2` with T.88 Annex E file header | [`NewDecoder`](jbig2.go) |
| PDF-embedded segment stream (header stripped by `/JBIG2Decode`) | [`NewDecoderEmbedded`](jbig2.go) |
| Either shape, with optional external globals | [`NewDecoderWithGlobals`](jbig2.go) |

The standalone constructor auto-registers with `image.Decode`
under the format name `"jbig2"`.

### Decode methods

| Method | Returns | Use when |
|---|---|---|
| [`Decoder.Decode`](jbig2.go) | `image.Image` (`*image.Gray`) | You want a stdlib image you can `png.Encode` |
| [`Decoder.DecodePacked`](jbig2.go) | [`PackedPage`](jbig2.go) | You consume bilevel data directly - PBM writers, 1-bpp PNG, bit-blit. Saves ~12 ms wall + ~35 MB alloc on a 600 dpi A4 page over the `image.Gray` conversion. |
| [`Decoder.DecodeContext`](jbig2.go) / [`Decoder.DecodePackedContext`](jbig2.go) | same, plus `context.Context` | You need cancellation / a wall-clock budget |

[`PackedPage.Data`](jbig2.go) aliases the decoder's internal
buffer until the next call on the same `Decoder`; copy it if
you need it to outlive that boundary.

### Resource budgets

JBIG2 is a denial-of-service vector - a 100-byte segment header
can declare a 30 GiB region. Every attacker-controlled allocation
is gated by a cap on the [`Limits`](limits.go) struct (image
pixels, symbols per dict, halftone grid cells, IAID code length,
refinement aggregates, per-symbol pixels, etc.). Always start
from [`DefaultLimits`](limits.go) and override the fields you
want; a bare struct literal silently disables every other cap
because zero means "no cap".

```go
limits := gobig2.DefaultLimits()
limits.MaxImagePixels = 100 * 1024 * 1024
limits.Apply()
```

`Apply` is process-wide and not safe to call concurrently with
active decodes; configure once at startup, then spawn workers.
Pair with a wall-clock budget via
[`Decoder.DecodeContext`](jbig2.go) - the segment-parser loop
checks `ctx.Err()` between segments.

### Error classification

Every decode failure wraps one of three sentinels:

| Sentinel | Meaning | Caller action |
|---|---|---|
| `ErrMalformed` | Input bytes are not legal JBIG2 | Skip the image |
| `ErrResourceBudget` | A configured `Limits` cap fired | Raise the cap or accept the rejection |
| `ErrUnsupported` | Legal but uses an unimplemented feature | Fall back to another decoder |

`Decoder.Decode` returns `io.EOF` after the final page on
multi-page input; cancellation paths wrap `context.Canceled` /
`context.DeadlineExceeded`. See [errors.go](errors.go) for the
recommended `switch` idiom.

## CLI tools

Binaries under [cmd/](cmd/):

- **[`cmd/gobig2`](cmd/gobig2/)** - decode a standalone or
  PDF-embedded JBIG2 stream into a PNG, PBM, or raw bitmap.
  Flags and exit codes documented in the binary's package doc.
- **[`cmd/extract-jbig2`](cmd/extract-jbig2/)** - walk a PDF and
  dump every `/JBIG2Decode` image XObject (and any
  `/JBIG2Globals` stream) as separate `.jb2` files, suitable as
  gobig2 test fixtures.
- **[`cmd/perf-cross`](cmd/perf-cross/)** - dev / CI tool that
  drives the cross-decoder benchmark table at the top of this
  README. Wrapped by `task bench:cross`.

Build with:

```sh
task build              # ./... compile-check
task build:release      # stripped, PGO-optimized binaries in ./bin/
```

## Development

The canonical command runner is [Taskfile.yml](Taskfile.yml).
Common targets:

```sh
task test               # all tests
task test:race          # race detector (requires CGO)
task test:conformance   # SerenityOS corpus + ITU-T T.88 Annex A if JBIG2_CONFORMANCE_DIR set
task lint               # golangci-lint v2
task fuzz               # 3s smoke fuzz across every Fuzz* target
task fuzz:long          # 10m sustained fuzz
task bench              # in-process micro-benchmarks
task bench:cross        # cross-decoder wall-clock bench
task ci                 # full gate: fmt:check + check + test:race
```

The `internal/gobig2test` package owns the public-API contract
tests, conformance corpus harness, fuzz targets, and pathological
input regressions.

## Repository layout

- [jbig2.go](jbig2.go), [errors.go](errors.go), [limits.go](limits.go) - public API surface.
- [internal/](internal/) - decoder packages, one per JBIG2 spec area (see [QUICKSTART.md](QUICKSTART.md) for the code map).
- [cmd/](cmd/) - CLI binaries.
- [docs/](docs/) - project docs and design notes.
- [testdata/](testdata/) - SerenityOS conformance fixtures, PDF-embedded samples, perf corpora, fuzz seeds.

## License

[Apache 2.0](LICENSE). See [NOTICE](NOTICE) for attribution.
