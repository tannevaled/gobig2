package gobig2test

import (
	"bytes"
	"errors"
	"testing"

	gobig2 "github.com/dkrisman/gobig2"

	"github.com/dkrisman/gobig2/internal/halftone"
	"github.com/dkrisman/gobig2/internal/page"
	"github.com/dkrisman/gobig2/internal/segment"
	"github.com/dkrisman/gobig2/internal/symbol"
)

func TestDefaultLimitsMatchPackageVars(t *testing.T) {
	d := gobig2.DefaultLimits()
	if d.MaxImagePixels != page.MaxImagePixels {
		t.Errorf("MaxImagePixels: default %d, package var %d",
			d.MaxImagePixels, page.MaxImagePixels)
	}
	if d.MaxSymbolsPerDict != segment.MaxSymbolsPerDict {
		t.Errorf("MaxSymbolsPerDict: default %d, package var %d",
			d.MaxSymbolsPerDict, segment.MaxSymbolsPerDict)
	}
	if d.MaxPatternsPerDict != halftone.MaxPatternsPerDict {
		t.Errorf("MaxPatternsPerDict: default %d, package var %d",
			d.MaxPatternsPerDict, halftone.MaxPatternsPerDict)
	}
	if d.MaxHalftoneGridCells != halftone.MaxGridCells {
		t.Errorf("MaxHalftoneGridCells: default %d, package var %d",
			d.MaxHalftoneGridCells, halftone.MaxGridCells)
	}
	if d.MaxRefaggninst != symbol.MaxRefaggninst {
		t.Errorf("MaxRefaggninst: default %d, package var %d",
			d.MaxRefaggninst, symbol.MaxRefaggninst)
	}
	if d.MaxSymbolPixels != symbol.MaxSymbolPixels {
		t.Errorf("MaxSymbolPixels: default %d, package var %d",
			d.MaxSymbolPixels, symbol.MaxSymbolPixels)
	}
	if d.MaxPixelsPerByte != segment.MaxPixelsPerByte {
		t.Errorf("MaxPixelsPerByte: default %d, package var %d",
			d.MaxPixelsPerByte, segment.MaxPixelsPerByte)
	}
	if d.MaxSymbolDictPixels != symbol.MaxSymbolDictPixels {
		t.Errorf("MaxSymbolDictPixels: default %d, package var %d",
			d.MaxSymbolDictPixels, symbol.MaxSymbolDictPixels)
	}
	if d.MaxBytesPerSegment != segment.MaxBytesPerSegment {
		t.Errorf("MaxBytesPerSegment: default %d, package var %d",
			d.MaxBytesPerSegment, segment.MaxBytesPerSegment)
	}
}

func TestLimitsApplyAndRestore(t *testing.T) {
	// Snapshot defaults, mutate via Apply, verify, restore.
	saved := gobig2.DefaultLimits()
	defer saved.Apply()

	custom := gobig2.Limits{
		MaxImagePixels:       1 << 20, // 1 megapixel
		MaxSymbolsPerDict:    4096,
		MaxPatternsPerDict:   256,
		MaxHalftoneGridCells: 1 << 16, // 64 K cells
		MaxRefaggninst:       8,
		MaxSymbolPixels:      32 * 1024,
		MaxPixelsPerByte:     2048,
		MaxSymbolDictPixels:  64 * 1024,
		MaxBytesPerSegment:   256 * 1024,
	}
	custom.Apply()

	if page.MaxImagePixels != custom.MaxImagePixels {
		t.Errorf("page.MaxImagePixels = %d, want %d",
			page.MaxImagePixels, custom.MaxImagePixels)
	}
	if segment.MaxSymbolsPerDict != custom.MaxSymbolsPerDict {
		t.Errorf("segment.MaxSymbolsPerDict = %d, want %d",
			segment.MaxSymbolsPerDict, custom.MaxSymbolsPerDict)
	}
	if halftone.MaxPatternsPerDict != custom.MaxPatternsPerDict {
		t.Errorf("halftone.MaxPatternsPerDict = %d, want %d",
			halftone.MaxPatternsPerDict, custom.MaxPatternsPerDict)
	}
	if halftone.MaxGridCells != custom.MaxHalftoneGridCells {
		t.Errorf("halftone.MaxGridCells = %d, want %d",
			halftone.MaxGridCells, custom.MaxHalftoneGridCells)
	}
	if symbol.MaxRefaggninst != custom.MaxRefaggninst {
		t.Errorf("symbol.MaxRefaggninst = %d, want %d",
			symbol.MaxRefaggninst, custom.MaxRefaggninst)
	}
	if symbol.MaxSymbolPixels != custom.MaxSymbolPixels {
		t.Errorf("symbol.MaxSymbolPixels = %d, want %d",
			symbol.MaxSymbolPixels, custom.MaxSymbolPixels)
	}
	if segment.MaxPixelsPerByte != custom.MaxPixelsPerByte {
		t.Errorf("segment.MaxPixelsPerByte = %d, want %d",
			segment.MaxPixelsPerByte, custom.MaxPixelsPerByte)
	}
	if symbol.MaxSymbolDictPixels != custom.MaxSymbolDictPixels {
		t.Errorf("symbol.MaxSymbolDictPixels = %d, want %d",
			symbol.MaxSymbolDictPixels, custom.MaxSymbolDictPixels)
	}
	if segment.MaxBytesPerSegment != custom.MaxBytesPerSegment {
		t.Errorf("segment.MaxBytesPerSegment = %d, want %d",
			segment.MaxBytesPerSegment, custom.MaxBytesPerSegment)
	}
}

// TestDefaultLimitsStableAfterApply pins DefaultLimits() returns
// same values before and after caller Apply()'d custom limits.
// Fields sourced from compile-time constants in owning internal
// packages, so custom Apply mutates only runtime caps - never the
// snapshot DefaultLimits hands back.
func TestDefaultLimitsStableAfterApply(t *testing.T) {
	saved := gobig2.DefaultLimits()
	defer saved.Apply()

	before := gobig2.DefaultLimits()

	// Apply distinctive set so any var-shadowed field surfaces.
	custom := gobig2.Limits{
		MaxImagePixels:       7,
		MaxSymbolsPerDict:    7,
		MaxPatternsPerDict:   7,
		MaxHalftoneGridCells: 7,
		MaxRefaggninst:       7,
		MaxSymbolPixels:      7,
		MaxPixelsPerByte:     7,
		MaxSymbolDictPixels:  7,
		MaxBytesPerSegment:   7,
	}
	custom.Apply()

	after := gobig2.DefaultLimits()
	if before != after {
		t.Errorf("DefaultLimits() drifted after Apply\n  before: %+v\n  after:  %+v",
			before, after)
	}
}

func TestLimitsApplyZeroDisables(t *testing.T) {
	// Zero field disables cap (per package var contracts:
	// 0 = unbounded). Apply must propagate.
	saved := gobig2.DefaultLimits()
	defer saved.Apply()

	(gobig2.Limits{}).Apply()
	if page.MaxImagePixels != 0 {
		t.Errorf("page.MaxImagePixels after zero-Apply = %d, want 0",
			page.MaxImagePixels)
	}
	if segment.MaxSymbolsPerDict != 0 {
		t.Errorf("segment.MaxSymbolsPerDict after zero-Apply = %d, want 0",
			segment.MaxSymbolsPerDict)
	}
	if halftone.MaxPatternsPerDict != 0 {
		t.Errorf("halftone.MaxPatternsPerDict after zero-Apply = %d, want 0",
			halftone.MaxPatternsPerDict)
	}
	if halftone.MaxGridCells != 0 {
		t.Errorf("halftone.MaxGridCells after zero-Apply = %d, want 0",
			halftone.MaxGridCells)
	}
	if symbol.MaxRefaggninst != 0 {
		t.Errorf("symbol.MaxRefaggninst after zero-Apply = %d, want 0",
			symbol.MaxRefaggninst)
	}
	if symbol.MaxSymbolPixels != 0 {
		t.Errorf("symbol.MaxSymbolPixels after zero-Apply = %d, want 0",
			symbol.MaxSymbolPixels)
	}
	if segment.MaxPixelsPerByte != 0 {
		t.Errorf("segment.MaxPixelsPerByte after zero-Apply = %d, want 0",
			segment.MaxPixelsPerByte)
	}
	if symbol.MaxSymbolDictPixels != 0 {
		t.Errorf("symbol.MaxSymbolDictPixels after zero-Apply = %d, want 0",
			symbol.MaxSymbolDictPixels)
	}
	if segment.MaxBytesPerSegment != 0 {
		t.Errorf("segment.MaxBytesPerSegment after zero-Apply = %d, want 0",
			segment.MaxBytesPerSegment)
	}
}

// TestLimitsRejectsOversizeImage verifies Apply'd caps gate at
// allocation site. 1-megapixel cap: PDF-embedded sample
// (3562x851 = 3,031,262 pixels = ~3 megapixels) should fail decode.
func TestLimitsRejectsOversizeImage(t *testing.T) {
	saved := gobig2.DefaultLimits()
	defer saved.Apply()

	// Bench much lower than the fixture's ~3 MP pixel count.
	tight := saved
	tight.MaxImagePixels = 1 << 20 // 1 megapixel
	tight.Apply()

	data := loadStandaloneSample(t)
	if _, err := gobig2.Decode(bytes.NewReader(data)); err == nil {
		t.Error("gobig2.Decode accepted oversize image despite tight gobig2.Limits")
	}
}

// TestLimitsRejectsHighPerSegmentDataLength verifies
// MaxBytesPerSegment cap rejects segments declaring more payload
// than cap allows. PDF-embedded sample's generic-region segment
// carries ~53 bytes; 32-byte cap rejects parsing.
func TestLimitsRejectsHighPerSegmentDataLength(t *testing.T) {
	saved := gobig2.DefaultLimits()
	defer saved.Apply()

	tight := saved
	tight.MaxBytesPerSegment = 32
	tight.Apply()

	data := loadStandaloneSample(t)
	_, err := gobig2.Decode(bytes.NewReader(data))
	if err == nil {
		t.Fatal("gobig2.Decode succeeded under tight MaxBytesPerSegment")
	}
	if !errors.Is(err, gobig2.ErrResourceBudget) {
		t.Errorf("err = %v, want errors.Is(err, gobig2.ErrResourceBudget)", err)
	}
}

// TestLimitsRejectsHighPixelsPerByteRatio verifies
// MaxPixelsPerByte cap rejects pages whose declared dimensions
// exceed input-byte budget by configured ratio. PDF-embedded sample
// is 94 bytes declaring ~3 MP, ratio ~32 K; 1 K cap rejects it.
func TestLimitsRejectsHighPixelsPerByteRatio(t *testing.T) {
	saved := gobig2.DefaultLimits()
	defer saved.Apply()

	tight := saved
	tight.MaxPixelsPerByte = 1024
	tight.Apply()

	data := loadStandaloneSample(t)
	if _, err := gobig2.Decode(bytes.NewReader(data)); err == nil {
		t.Error("gobig2.Decode accepted page-info despite tight MaxPixelsPerByte")
	}
}

func TestPerSymbolCapDoesNotBiteBeforeTheAggregateOne(t *testing.T) {
	// The per-symbol cap and the aggregate cap bound the same thing: pixels
	// decoded in one symbol dictionary. The aggregate is charged for every
	// symbol, so a dictionary cannot spend more than MaxSymbolDictPixels
	// however it splits the work up.
	//
	// A per-symbol cap BELOW the aggregate therefore bounds no work the
	// aggregate does not already bound. All it does is forbid a shape — one
	// big symbol rather than several — and real encoders emit that shape:
	// some put a page-sized region in a single symbol, and 7 of 403 JBIG2
	// streams taken from public scanned documents need between 7 and 8 MP for
	// one symbol. poppler reads all of them at its own defaults.
	//
	// So the two defaults are equal, and this says so, because setting the
	// per-symbol one lower again would silently refuse real documents.
	if symbol.DefaultMaxSymbolPixels != symbol.DefaultMaxSymbolDictPixels {
		t.Errorf("per-symbol default %d, aggregate default %d: a per-symbol cap "+
			"below the aggregate refuses documents without bounding work",
			symbol.DefaultMaxSymbolPixels, symbol.DefaultMaxSymbolDictPixels)
	}
}
