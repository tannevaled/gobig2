package gobig2test

import (
	"bytes"
	"context"
	"image"
	"os"
	"runtime/debug"
	"testing"
	"time"

	gobig2 "github.com/tannevaled/gobig2"
)

// FuzzNewDecoder feeds random bytes to auto-detect entry and
// gobig2.Decode. Contract: no panic on any input, decode completes
// within wall-clock cap [runFuzzedDecode] enforces (8 s today; see
// helper rationale) + 256 MiB memory cap (well below production
// defaults; fuzz inputs can't allocate gigabytes).
//
// Seeds: JBIG2 file magic (fuzzer mutates header byte, explores
// probe.Configs branches), few raw segment-header shapes, +
// committed PDF-extracted sample as baseline to mutate.
func FuzzNewDecoder(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		runFuzzedDecode(t, func(ctx context.Context) (image.Image, error) {
			dec, err := gobig2.NewDecoder(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			return dec.DecodeContext(ctx)
		})
	})
}

// FuzzNewDecoderEmbedded targets PDF-embedded path - random bytes
// as segment stream with no file header. Surface PDF readers
// expose; panic or 30-second decode on any input = CVE candidate.
func FuzzNewDecoderEmbedded(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		runFuzzedDecode(t, func(ctx context.Context) (image.Image, error) {
			dec, err := gobig2.NewDecoderEmbedded(bytes.NewReader(data), nil)
			if err != nil {
				return nil, err
			}
			return dec.DecodeContext(ctx)
		})
	})
}

// FuzzNewDecoderWithGlobals exercises 'could be either shape'
// constructor against random bytes + separate random globals
// stream. Two independent inputs - constructor behavior diverges
// on empty vs non-empty globals.
func FuzzNewDecoderWithGlobals(f *testing.F) {
	f.Add([]byte{}, []byte{})
	f.Add(jbig2Magic, []byte{})
	f.Add(jbig2Magic, []byte{0, 0, 0, 0, 51, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data, globals []byte) {
		runFuzzedDecode(t, func(ctx context.Context) (image.Image, error) {
			dec, err := gobig2.NewDecoderWithGlobals(bytes.NewReader(data), globals)
			if err != nil {
				return nil, err
			}
			return dec.DecodeContext(ctx)
		})
	})
}

// FuzzParseGlobalsAndReset exercises PDF-reader hot path real
// client uses across many image XObjects sharing single
// /JBIG2Globals: gobig2.ParseGlobals once,
// gobig2.NewDecoderEmbeddedWithGlobals + gobig2.DecodeContext per
// image, Reset between images.
//
// Two independent inputs - parser behavior diverges on empty
// (no-op handle path) vs non-empty (real gobig2.ParseGlobals work
// + shared global ctx) globals.
func FuzzParseGlobalsAndReset(f *testing.F) {
	// Seeds: representative shapes existing pathological + corpus
	// tests cover, + tiny non-empty globals pair so workflow
	// exercises parse + bind path.
	f.Add([]byte{}, []byte{})
	f.Add(jbig2Magic, []byte{})
	f.Add(jbig2Magic, []byte{0, 0, 0, 0, 51, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, image1, globals []byte) {
		runFuzzedDecode(t, func(ctx context.Context) (img image.Image, err error) {
			parsed, perr := gobig2.ParseGlobals(globals)
			if perr != nil {
				// Pre-parse failures fine - contract is "no
				// panic"; malformed globals slice should surface
				// as clean error.
				return nil, perr
			}
			dec, cerr := gobig2.NewDecoderEmbeddedWithGlobals(bytes.NewReader(image1), parsed)
			if cerr != nil {
				return nil, cerr
			}
			img, err = dec.DecodeContext(ctx)
			if err != nil {
				return img, err
			}
			// Reset to same image bytes + decode again -
			// exercises resettable-state lifecycle for nil-globals
			// callers and random globals + image pairs.
			if rerr := dec.Reset(bytes.NewReader(image1)); rerr != nil {
				return img, rerr
			}
			return dec.DecodeContext(ctx)
		})
	})
}

// runFuzzedDecode runs decode in goroutine inside fuzz-tightened
// resource budget from [fuzzLimits] + 8 s wall-clock cap.
//
// Tightened gobig2.Limits keep adversarial inputs from allocating
// way to 30-second decode within cap; smaller cap catches the "0.5
// s decode for 30 bytes" patterns fuzzer surfaces (12 K x 12 K
// bitmap from malformed page-info segment is canonical example,
// captured as pathological seed in pathological_test.go).
//
// Why 8 s vs 3 s: under default `go test -fuzz` concurrency (4
// workers fighting cores), legitimate inputs taking ~500 ms
// standalone can spike past 3 s under load. 8 s well above
// legitimate fuzz-input decode times, still tight enough for
// multi-second pathologies.
//
// Panics in decode caught by goroutine recover, surfaced to fuzzer
// as test failure (panics = contract violation; actual decode
// errors expected on most random input, silently dropped).
//
// Closure receives cancellation ctx plumbed from 8 s wall-clock
// budget; must hand it to gobig2.DecodeContext so decoder unwinds
// at next segment boundary on budget fire. Otherwise pathological
// inputs leave decode goroutines alive past t.Fatalf; harness can
// accumulate concurrent gobig2.Decoder / Document graphs under
// -fuzz parallelism.
func runFuzzedDecode(t *testing.T, fn func(ctx context.Context) (image.Image, error)) {
	saved := gobig2.DefaultLimits()
	fuzzLimits.Apply()
	defer saved.Apply()

	prev := debug.SetMemoryLimit(-1)
	debug.SetMemoryLimit(256 << 20)
	defer debug.SetMemoryLimit(prev)

	type result struct {
		img image.Image
		err error
	}
	done := make(chan result, 1)
	panicked := make(chan any, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked <- r
			}
		}()
		img, err := fn(ctx)
		done <- result{img, err}
	}()

	select {
	case r := <-done:
		// Any non-nil error fine for fuzzing; contract is "no
		// panic" + "completes promptly", not "succeeds".
		_ = r.err
		_ = r.img
	case p := <-panicked:
		t.Fatalf("decode panicked: %v", p)
	case <-ctx.Done():
		t.Fatalf("decode exceeded 8 s wall-clock budget")
	}
}

// fuzzLimits = resource-cap profile fuzz harnesses run under.
// Built from DefaultLimits so every cap (including later additions
// like MaxHalftoneGridCells, MaxPixelsPerByte, MaxSymbolDictPixels)
// stays enforced - bare struct literal would zero-default every
// unlisted field, silently disabling corresponding cap and letting
// fuzz seeds slip past allocation guards present at production
// defaults.
//
// Overrides below tighten specific caps so adversarial declared
// dimensions trip allocation guard early vs consuming full memory
// budget while waiting on wall clock. Real PDF readers should pick
// limits to match document mix.
var fuzzLimits = func() gobig2.Limits {
	l := gobig2.DefaultLimits()
	// 1 megapixel (~1K x 1K) well above any real-world individual
	// symbol bitmap (glyphs = tens of pixels per side), well below
	// size where generic-region decoding trips wall-clock budget
	// under 4-worker parallel fuzz.
	l.MaxImagePixels = 1 * 1024 * 1024
	l.MaxSymbolsPerDict = 64
	l.MaxPatternsPerDict = 64
	l.MaxRefaggninst = 4
	// 64 KP per-symbol bitmap cap - below any real glyph even at
	// 1200 DPI, tight enough adversarial SDD inputs trip cap
	// before iterating generic-region template loop. Without it,
	// fuzz seeds with one 16-megapixel "glyph" take ~10 s.
	l.MaxSymbolPixels = 64 * 1024
	// 1 MB per-segment-data-length cap. Bundled corpus fixtures
	// <= 5 KB per segment; 1 MB blocks adversarial 4 GB-DataLength
	// shapes at parse time without rejecting any legitimate
	// fuzz-seed input.
	l.MaxBytesPerSegment = 1 << 20
	return l
}()

// jbig2Magic is 8-byte JBIG2 file-header magic used as seed
// prefix. probe.Configs locks onto inputs starting with these bytes.
var jbig2Magic = []byte{0x97, 0x4A, 0x42, 0x32, 0x0D, 0x0A, 0x1A, 0x0A}

// addSeeds adds small representative seed corpus to f. Seeds get
// fuzzer past early-rejection branches into segment-parser body
// where interesting bugs live.
func addSeeds(f *testing.F) {
	// Empty + magic-only.
	f.Add([]byte{})
	f.Add(jbig2Magic)
	// Magic + flags = sequential, no page count.
	f.Add(append(append([]byte{}, jbig2Magic...), 0x03))
	// Committed PDF-embedded sample as seed for embedded path.
	// Missing (testdata not in fuzz cache) -> skip silently.
	if data, err := os.ReadFile("../../testdata/pdf-embedded/sample.jb2"); err == nil {
		f.Add(data)
	}
	// Garbage-but-plausible segment header: segNum=0, type=48
	// (page info), refByte=0, pageAssoc=1, dataLen=19, followed by
	// 19 bytes of region-info-shaped junk.
	f.Add(append([]byte{
		0, 0, 0, 0, // segNum
		0x30, // flags: type 48 page info
		0,    // refByte
		1,    // pageAssoc
		0, 0, 0, 19,
	}, make([]byte, 19)...))
}
