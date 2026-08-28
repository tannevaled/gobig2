package gobig2test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	gobig2 "github.com/tannevaled/gobig2"
)

// PDF-reader hot-path benches. Two patterns:
//
//   - **Per-image re-parse** (`BenchmarkPerImageGlobalsReparse`):
//     legacy, one gobig2.Decoder per image, drainGlobals re-runs.
//   - **Shared globals + Reset** (`BenchmarkParseGlobalsReuseReset`):
//     parse globals once via [gobig2.ParseGlobals], one Decoder via
//     [gobig2.NewDecoderEmbeddedWithGlobals], [gobig2.Decoder.Reset]
//     between images.
//
// sample.jb2 fixture has no globals, so gobig2.ParseGlobals(nil) is
// no-op; both paths do near-identical work. Bench locks invariant:
// new path not slower than legacy. Regression = added per-Reset cost.
//
// Real PDF-reader win (skip drainGlobals re-exec across many images
// sharing one /JBIG2Globals) needs non-empty globals fixture. Part
// of deferred PDF corpus workstream (see [../DEFERRED.md](../DEFERRED.md)).
func BenchmarkPerImageGlobalsReparse(b *testing.B) {
	data, err := os.ReadFile("../../testdata/pdf-embedded/sample.jb2")
	if err != nil {
		b.Skipf("sample.jb2 not present: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec, err := gobig2.NewDecoderEmbedded(bytes.NewReader(data), nil)
		if err != nil {
			b.Fatalf("ctor: %v", err)
		}
		if _, err := dec.Decode(); err != nil {
			b.Fatalf("decode: %v", err)
		}
	}
}

func BenchmarkParseGlobalsReuseReset(b *testing.B) {
	data, err := os.ReadFile("../../testdata/pdf-embedded/sample.jb2")
	if err != nil {
		b.Skipf("sample.jb2 not present: %v", err)
	}
	g, err := gobig2.ParseGlobals(nil)
	if err != nil {
		b.Fatalf("gobig2.ParseGlobals: %v", err)
	}
	dec, err := gobig2.NewDecoderEmbeddedWithGlobals(bytes.NewReader(data), g)
	if err != nil {
		b.Fatalf("ctor: %v", err)
	}
	if _, err := dec.Decode(); err != nil {
		b.Fatalf("first decode: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := dec.Reset(bytes.NewReader(data)); err != nil {
			b.Fatalf("reset: %v", err)
		}
		if _, err := dec.Decode(); err != nil {
			b.Fatalf("decode: %v", err)
		}
	}
}

// Per-fixture-category throughput benches. Each picks a tiny bundled
// fixture, decodes in tight loop. Regression markers for four major
// codepaths (generic-region template loop, MMR uncompress2D,
// arith-coded symbol dict + text region, Huffman-coded symbol dict +
// text region). Fixtures well-formed; bench measures end-to-end
// decode cost (ctor + parse + decode + return).
//
// Fixture sizes tiny (300-600 bytes) deliberately - measures
// parser/template overhead, not gross throughput on 30 MB fax page.
// Future F01_200 bench would measure latter; until then these catch
// per-call setup + decode hot-path regressions.
func benchFixture(b *testing.B, name string) {
	b.Helper()
	path := filepath.Join("..", "..", "testdata", "serenityos", name)
	data, err := os.ReadFile(path)
	if err != nil {
		b.Skipf("%s not present: %v", path, err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dec, err := gobig2.NewDecoder(bytes.NewReader(data))
		if err != nil {
			b.Fatalf("ctor: %v", err)
		}
		if _, err := dec.Decode(); err != nil {
			b.Fatalf("decode: %v", err)
		}
	}
}

// BenchmarkGenericRegion exercises generic-region template hot loop
// (decodeTemplateUnopt + arith.Decoder.Decode per pixel). CHANGELOG:
// 99% CPU on originally-cataloged slow seeds spent here. Regression
// in GetPixel/SetPixel/decoder call shows as throughput drop.
func BenchmarkGenericRegion(b *testing.B) {
	benchFixture(b, "bitmap.jbig2")
}

// BenchmarkMMRRegion exercises MMR (T.6 / G4) decoder -
// uncompress2D state-machine + offsets walk. Fax pages (F01_200
// family) live or die on this path's throughput.
func BenchmarkMMRRegion(b *testing.B) {
	benchFixture(b, "bitmap-mmr.jbig2")
}

// BenchmarkSymbolDictArith exercises SDDProc.DecodeArith hot path
// via symbol-dict + text-region pair (JBIG2 text-encoding mainline).
func BenchmarkSymbolDictArith(b *testing.B) {
	benchFixture(b, "bitmap-symbol.jbig2")
}

// BenchmarkSymbolDictHuffman exercises SDDProc.DecodeHuffman path.
// Also hits cached huffman.NewStandardTable lookups
// (parseSymbolDictInner builds SDHUFFDH/DW/BMSIZE/AGGINST from
// standard tables).
func BenchmarkSymbolDictHuffman(b *testing.B) {
	benchFixture(b, "bitmap-symbol-symhuff-texthuff.jbig2")
}

// BenchmarkHalftoneRegion exercises the halftone-region path
// (pattern-dict + halftone composition).
func BenchmarkHalftoneRegion(b *testing.B) {
	benchFixture(b, "bitmap-halftone.jbig2")
}
