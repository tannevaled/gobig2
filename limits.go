package gobig2

import (
	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/halftone"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/segment"
	"github.com/tannevaled/gobig2/internal/symbol"
)

// Limits bundles resource caps the codec consults when
// allocating bitmaps and dictionaries. Each bounds a different
// attacker-controlled allocation or work site:
//
//   - MaxImagePixels caps any single bitmap NewImage allocates.
//     1200-DPI A4 = ~140 megapixels; default 256 megapixels
//     leaves ~10x headroom, blocks pathological dimensions.
//   - MaxSymbolsPerDict caps SDNUMNEWSYMS / SDNUMEXSYMS at parse
//     and aggregate input-symbol pool a text region or symbol
//     dict assembles across referenced symbol-dict segments.
//     Real dicts rarely exceed few thousand (corpus max: 308);
//     1 M default well above legit, bounds pre-decode pointer-
//     slice allocation.
//   - MaxPatternsPerDict caps the halftone HDPATS array.
//   - MaxHalftoneGridCells caps halftone HGW x HGH grid product.
//     Per-side state.MaxImageSize rejects oversized HGW/HGH;
//     this cap covers when both fit per-side but product drives
//     per-cell rendering into multi-second decode regardless of
//     output region size.
//   - MaxIaidCodeLen caps SBSYMCODELEN before IAID context array
//     allocated. Array sizes 1<<SBSYMCODELEN; cap 30 holds worst
//     case below 16 GiB.
//   - MaxRefaggninst caps REFAGGNINST per aggregate symbol;
//     blocks adversarial inner-refinement decode hangs.
//   - MaxSymbolPixels caps SYMWIDTH x HCHEIGHT per symbol;
//     blocks multi-megapixel glyph driving generic-region
//     template loop into multi-second decode.
//   - MaxSymbolDictPixels caps aggregate SYMWIDTH x HCHEIGHT
//     sum across all symbols in one SDD call; blocks "many
//     small symbols" passing per-symbol cap but accumulating
//     to hundreds of megapixels.
//   - MaxPixelsPerByte caps ratio of declared page-info width
//     x height to total input-byte budget; blocks
//     30-byte -> 152-megapixel page-info shapes at parse.
//   - MaxBytesPerSegment caps per-segment DataLength; blocks
//     adversarial 4 GB segment-length at parse before any
//     per-segment work.
//
// Zero on any field = "no cap". Callers wanting no limits can
// use [Limits]{} but should pair with own wall-clock budget.
//
// Concurrency. Limits.Apply mutates process-wide package vars;
// not safe across goroutines or concurrent with active decodes.
// Configure once at startup, then spawn goroutines. Concurrent
// Decoder instances calling Decode / DecodeContext on
// independent inputs are safe - each owns its own Document;
// package Limits read-only after Apply returns.
//
// Tests. Tests mutating caps (via Limits.Apply or direct var
// writes) must not call `t.Parallel` and must save/restore via
// deferred reset. Prefer DefaultLimits().Apply snapshots when
// swapping complete profiles; direct-var fine for one-knob
// tests touching a single cap.
type Limits struct {
	// MaxImagePixels caps the total pixel count of any single
	// bitmap NewImage allocates. Default 256 megapixels.
	MaxImagePixels int64
	// MaxSymbolsPerDict caps SDNUMNEWSYMS / SDNUMEXSYMS at parse
	// and also the aggregate input-symbol pool a text region or
	// symbol dict assembles across all referenced symbol-dict
	// segments. Default 1 M.
	MaxSymbolsPerDict uint32
	// MaxPatternsPerDict caps the halftone HDPATS array length.
	// Default 1 M.
	MaxPatternsPerDict uint32
	// MaxHalftoneGridCells caps halftone HGW x HGH grid product.
	// Each cell expands to HPW x HPH pixels, so cell count is
	// order of magnitude below rendered pixel count: 1200-DPI A4
	// (~140 megapixels) with 8x8 patterns = ~2 megacells; even
	// 2x2 stays under ~35 megacells. Default 64 megacells past
	// legit use, bounds worst-case per-cell rendering at ~2 s
	// CPU. Without cap, adversarial HGW/HGH each under per-side
	// state.MaxImageSize can still declare multi-gigacell grid
	// against tiny output region.
	MaxHalftoneGridCells uint64
	// MaxIaidCodeLen caps SBSYMCODELEN before IAID context array
	// allocated. Max practical 30. Cap is `const` in
	// internal/arith; field here for API symmetry but
	// [Limits.Apply] ignores it.
	MaxIaidCodeLen uint8
	// MaxRefaggninst caps REFAGGNINST per aggregate symbol. Real
	// glyphs rarely exceed few dozen per aggregate; default 1024
	// well above legit use.
	MaxRefaggninst uint32
	// MaxSymbolPixels caps SYMWIDTH x HCHEIGHT per symbol bitmap.
	// Real glyphs are tens of pixels/side; default 4 megapixels
	// = two orders beyond legit, well below page-level cap.
	// Adversarial input drives single glyph to multi-megapixel
	// then iterates generic-region template loop per pixel (~10
	// s CPU per 16 megapixel adversarial symbol on dev VM).
	MaxSymbolPixels uint64
	// MaxPixelsPerByte caps ratio of declared page-info
	// `width x height` to total input-byte budget. Default 1 M
	// pixels/byte (~30x headroom over highest-ratio fixture)
	// rejects 152-megapixel from 30-byte adversarial pages
	// while leaving room for tight encodings (bundled
	// testdata/pdf-embedded/sample.jb2 ratio ~32 K).
	MaxPixelsPerByte uint64
	// MaxSymbolDictPixels caps aggregate SYMWIDTH x HCHEIGHT sum
	// across all symbols in one SDD call. Complements
	// MaxSymbolPixels: adversarial dict can declare hundreds of
	// small symbols each passing per-symbol cap but accumulating
	// to hundreds of megapixels of template-loop work. Real
	// text-heavy fixtures top out at few megapixels per dict;
	// default 16 megapixels.
	MaxSymbolDictPixels uint64
	// MaxBytesPerSegment caps DataLength declared in each segment
	// header. Real segments rarely exceed few MB; default 16 MB
	// rejects 4 GB adversarial declarations at parse before any
	// per-segment work. 0xFFFFFFFF "unknown length" streaming
	// sentinel is exempt.
	MaxBytesPerSegment uint64
}

// DefaultLimits returns the codec's stock caps - values package
// vars carry before any [Limits.Apply] call. Starting point for
// customized Limits.
//
// Stability: every field sourced from compile-time constant in
// owning internal package, so DefaultLimits() returns same
// values regardless of prior [Limits.Apply] call. Per-package
// var Apply mutates is initialized from same constant.
func DefaultLimits() Limits {
	return Limits{
		MaxImagePixels:       page.DefaultMaxImagePixels,
		MaxSymbolsPerDict:    segment.DefaultMaxSymbolsPerDict,
		MaxPatternsPerDict:   halftone.DefaultMaxPatternsPerDict,
		MaxHalftoneGridCells: halftone.DefaultMaxGridCells,
		MaxIaidCodeLen:       arith.MaxIaidCodeLen,
		MaxRefaggninst:       symbol.DefaultMaxRefaggninst,
		MaxSymbolPixels:      symbol.DefaultMaxSymbolPixels,
		MaxPixelsPerByte:     segment.DefaultMaxPixelsPerByte,
		MaxSymbolDictPixels:  symbol.DefaultMaxSymbolDictPixels,
		MaxBytesPerSegment:   segment.DefaultMaxBytesPerSegment,
	}
}

// Apply writes l into package-level caps internal decoders
// consult. Process-wide; not safe concurrent with itself or
// active Decode (package vars read by every decoder, mid-decode
// mutation could race). Concurrent Decode on independent
// Decoder instances safe - share read-only Limits, each owns
// own Document state. Field = 0 disables that cap entirely.
//
// FOOTGUN: callers tweaking one or two caps must start from
// [DefaultLimits] not a bare struct literal:
//
//	// WRONG - silently disables every cap you didn't list:
//	gobig2.Limits{MaxImagePixels: 100_000_000}.Apply()
//
//	// RIGHT - preserves the documented defaults you didn't change:
//	limits := gobig2.DefaultLimits()
//	limits.MaxImagePixels = 100_000_000
//	limits.Apply()
//
// Bare-literal form only when intentionally disabling
// everything except listed fields (e.g. fuzz harnesses needing
// permissive profile).
//
// MaxIaidCodeLen is `const` in internal/arith and cannot be
// reassigned at runtime - field on Limits for API symmetry but
// Apply ignores. To loosen IAID cap, rebuild codec with
// constant changed; bound is hard ceiling on pre-allocation
// context array, not configurable knob.
func (l Limits) Apply() {
	page.MaxImagePixels = l.MaxImagePixels
	segment.MaxSymbolsPerDict = l.MaxSymbolsPerDict
	halftone.MaxPatternsPerDict = l.MaxPatternsPerDict
	halftone.MaxGridCells = l.MaxHalftoneGridCells
	symbol.MaxRefaggninst = l.MaxRefaggninst
	symbol.MaxSymbolPixels = l.MaxSymbolPixels
	segment.MaxPixelsPerByte = l.MaxPixelsPerByte
	symbol.MaxSymbolDictPixels = l.MaxSymbolDictPixels
	segment.MaxBytesPerSegment = l.MaxBytesPerSegment
}
