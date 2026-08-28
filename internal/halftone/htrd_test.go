package halftone

import (
	"errors"
	"testing"

	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/state"
)

// TestDecodeMMRHNumPats1 pins HNUMPATS == 1: strict T.88 §6.6.5.3
// hbpp is 0 so gsbpp == 0. Naive code writes gsplanes[gsbpp-1] =
// gsplanes[-1] -> panic. Arith path short-circuits on gsbpp == 0;
// MMR path must route gsbpp == 0 to decodeImage with nil planes.
func TestDecodeMMRHNumPats1(t *testing.T) {
	pat := page.NewImage(1, 1)
	if pat == nil {
		t.Fatal("NewImage failed for pattern")
	}
	pat.Fill(true)

	h := &HTRDProc{
		HBW: 1, HBH: 1,
		HGW: 1, HGH: 1,
		HNUMPATS: 1,
		HPATS:    []*page.Image{pat},
		HPW:      1,
		HPH:      1,
	}
	// Empty stream fine - gsbpp == 0 skips MMR decode. Without
	// guard, panic before stream read.
	img, err := h.DecodeMMR(bio.NewBitStream(nil, 0))
	if err != nil {
		t.Fatalf("DecodeMMR HNUMPATS=1 failed: %v", err)
	}
	if img == nil {
		t.Fatal("DecodeMMR returned nil image")
	}
}

// TestDecodeMMROversizeHGW pins DecodeMMR rejects HGW/HGH past
// state.MaxImageSize before run-offset slice alloc in
// mmr.Decompressor.Uncompress. Hostile HGW = 256M (passes
// per-bitmap cap at HGH = 1) drives ~4 GiB.
//
// Length-only test - never constructs Decompressor.
func TestDecodeMMROversizeHGW(t *testing.T) {
	pat := page.NewImage(1, 1)
	if pat == nil {
		t.Fatal("NewImage failed for pattern")
	}
	pat.Fill(true)

	h := &HTRDProc{
		HBW: 16, HBH: 16,
		HGW: state.MaxImageSize + 1, HGH: 1,
		HNUMPATS: 2,
		HPATS:    []*page.Image{pat, pat},
		HPW:      1,
		HPH:      1,
	}
	_, err := h.DecodeMMR(bio.NewBitStream(nil, 0))
	if err == nil {
		t.Fatal("DecodeMMR accepted oversize HGW")
	}
	if !errors.Is(err, errs.ErrResourceBudget) {
		t.Errorf("oversize HGW err should wrap ErrResourceBudget, got: %v", err)
	}
}

// TestDecodeArithRejectsOversizeGrid pins shared entry-point gate:
// HGW x HGH past MaxGridCells fails before skip-plane alloc or
// per-cell loop. Naive shape allocates HGW x HGH skip pixels and
// walks loop before any size check; 2-megacell-per-side burns
// CPU even when output region tiny.
func TestDecodeArithRejectsOversizeGrid(t *testing.T) {
	pat := page.NewImage(1, 1)
	if pat == nil {
		t.Fatal("NewImage failed for pattern")
	}
	pat.Fill(true)

	h := &HTRDProc{
		HBW: 16, HBH: 16,
		// Each side under state.MaxImageSize; product past MaxGridCells.
		HGW: 16384, HGH: 16384,
		HNUMPATS: 2,
		HPATS:    []*page.Image{pat, pat},
		HPW:      1,
		HPH:      1,
	}
	_, err := h.DecodeArith(nil, nil)
	if err == nil {
		t.Fatal("DecodeArith accepted oversize HGW*HGH")
	}
	if !errors.Is(err, errs.ErrResourceBudget) {
		t.Errorf("oversize grid err should wrap ErrResourceBudget, got: %v", err)
	}
}

// TestDecodeMMRRejectsOversizeGrid mirrors arith-path test on MMR
// path: shared validateGridBudget gate rejects oversize HGW*HGH
// for both modes.
func TestDecodeMMRRejectsOversizeGrid(t *testing.T) {
	pat := page.NewImage(1, 1)
	if pat == nil {
		t.Fatal("NewImage failed for pattern")
	}
	pat.Fill(true)

	h := &HTRDProc{
		HBW: 16, HBH: 16,
		HGW: 16384, HGH: 16384,
		HNUMPATS: 2,
		HPATS:    []*page.Image{pat, pat},
		HPW:      1,
		HPH:      1,
	}
	_, err := h.DecodeMMR(bio.NewBitStream(nil, 0))
	if err == nil {
		t.Fatal("DecodeMMR accepted oversize HGW*HGH")
	}
	if !errors.Is(err, errs.ErrResourceBudget) {
		t.Errorf("oversize grid err should wrap ErrResourceBudget, got: %v", err)
	}
}

// TestDecodeImageRejectsOutOfRangeGSVal pins loud-fail when
// assembled gray value out of range. Naive clamp to HNUMPATS-1
// masks corrupt gray-plane bits + paints last pattern over
// corrupted cell. decodeImage returns ErrMalformed-wrapped error.
//
// Verified safe vs full 206-fixture corpus pre-promotion (clamp
// never fired on legal input).
func TestDecodeImageRejectsOutOfRangeGSVal(t *testing.T) {
	// One pattern, gray plane has 1 bit at only cell -> gsval = 1
	// exceeds HNUMPATS = 1, must reject.
	pat := page.NewImage(1, 1)
	if pat == nil {
		t.Fatal("NewImage failed for pattern")
	}
	pat.Fill(false)

	gsplane := page.NewImage(1, 1)
	if gsplane == nil {
		t.Fatal("NewImage failed for gray plane")
	}
	gsplane.SetPixel(0, 0, 1)

	h := &HTRDProc{
		HBW: 1, HBH: 1,
		HGW: 1, HGH: 1,
		HNUMPATS: 1,
		HPATS:    []*page.Image{pat},
		HPW:      1,
		HPH:      1,
	}
	_, err := h.decodeImage([]*page.Image{gsplane})
	if err == nil {
		t.Fatal("decodeImage accepted gsval >= HNUMPATS; want error")
	}
	if !errors.Is(err, errs.ErrMalformed) {
		t.Errorf("expected ErrMalformed wrap, got: %v", err)
	}
}

// TestDecodeImageHNumPats1ZeroPlanes pins HNUMPATS == 1: strict
// T.88 §6.6.5.3 HBPP is 0 (no gray planes; single pattern every
// cell). decodeImage accepts empty gsplanes + produces pattern-0
// without tripping gsval check. Dropped max(1,...) floor -> empty
// gsplanes -> gsval = 0 every cell, in range for HNUMPATS == 1.
func TestDecodeImageHNumPats1ZeroPlanes(t *testing.T) {
	pat := page.NewImage(1, 1)
	if pat == nil {
		t.Fatal("NewImage failed for pattern")
	}
	pat.Fill(true)

	h := &HTRDProc{
		HBW: 1, HBH: 1,
		HGW: 1, HGH: 1,
		HNUMPATS: 1,
		HPATS:    []*page.Image{pat},
		HPW:      1,
		HPH:      1,
	}
	img, err := h.decodeImage(nil) // hbpp == 0 -> zero planes
	if err != nil {
		t.Fatalf("decodeImage HNUMPATS=1 with zero planes failed: %v", err)
	}
	if img == nil {
		t.Fatal("decodeImage returned nil image")
	}
}

// TestDecodeImageAcceptsInRangeGSVal is negative pin: strict
// check must not reject legal in-range case.
func TestDecodeImageAcceptsInRangeGSVal(t *testing.T) {
	pat0 := page.NewImage(1, 1)
	pat1 := page.NewImage(1, 1)
	if pat0 == nil || pat1 == nil {
		t.Fatal("NewImage failed for patterns")
	}
	pat0.Fill(false)
	pat1.Fill(true)

	gsplane := page.NewImage(1, 1)
	if gsplane == nil {
		t.Fatal("NewImage failed for gray plane")
	}
	gsplane.SetPixel(0, 0, 1)

	h := &HTRDProc{
		HBW: 1, HBH: 1,
		HGW: 1, HGH: 1,
		HNUMPATS: 2,
		HPATS:    []*page.Image{pat0, pat1},
		HPW:      1,
		HPH:      1,
	}
	if _, err := h.decodeImage([]*page.Image{gsplane}); err != nil {
		t.Errorf("decodeImage rejected legal in-range gsval=1 (HNUMPATS=2): %v", err)
	}
}
