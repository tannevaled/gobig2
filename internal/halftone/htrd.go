package halftone

import (
	"errors"
	"fmt"

	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/generic"
	"github.com/tannevaled/gobig2/internal/intmath"
	"github.com/tannevaled/gobig2/internal/mmr"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/state"
)

// HTRDProc is the halftone-region decoding procedure.
type HTRDProc struct {
	HBW, HBH    uint32
	HMMR        bool
	HTEMPLATE   uint8
	HNUMPATS    uint32
	HPATS       []*page.Image
	HDEFPIXEL   bool
	HCOMBOP     page.ComposeOp
	HENABLESKIP bool
	HGW, HGH    uint32
	HGX, HGY    int32
	HRX, HRY    uint16
	HPW, HPH    uint8
}

// NewHTRDProc creates a new halftone-region decoder.
func NewHTRDProc() *HTRDProc { return &HTRDProc{} }

// MaxGridCells caps total halftone-grid cells (HGW x HGH). Each
// side under state.MaxImageSize but their product drives the
// per-cell loop in [HTRDProc.decodeImage] and skip-plane alloc
// in [HTRDProc.DecodeArith] - neither bounded by output region.
// Tiny output can still declare multi-gigacell grid + burn CPU.
//
// Real grids smaller than output bitmap (each cell expands to
// HPW x HPH pattern): 1200-DPI A4 (~140 MP) with 8x8 patterns
// is ~2 megacells, 2x2 stays under ~35 megacells. Default 64
// megacells past legitimate use, bounds worst-case at ~2 s on
// dev VM. Set to 0 to disable.
var MaxGridCells uint64 = DefaultMaxGridCells

// DefaultMaxGridCells is the codec's stock cap for [MaxGridCells].
// See [internal/page.DefaultMaxImagePixels] for var+const pairing rationale.
const DefaultMaxGridCells uint64 = 64 << 20

// validateGridBudget rejects HGW/HGH past per-side or total-cell
// caps. Both decode paths call at entry so skip-plane alloc and
// per-cell loop run only on accepted budgets.
func (h *HTRDProc) validateGridBudget() error {
	if h.HGW > state.MaxImageSize || h.HGH > state.MaxImageSize {
		return fmt.Errorf("halftone: HGW/HGH exceeds MaxImageSize: %w", errs.ErrResourceBudget)
	}
	if MaxGridCells > 0 && uint64(h.HGW)*uint64(h.HGH) > MaxGridCells {
		return fmt.Errorf("halftone: HGW*HGH (%d) exceeds MaxGridCells (%d): %w",
			uint64(h.HGW)*uint64(h.HGH), MaxGridCells, errs.ErrResourceBudget)
	}
	return nil
}

// DecodeArith performs arithmetic decoding.
func (h *HTRDProc) DecodeArith(arithDecoder *arith.Decoder, gbContexts []arith.Ctx) (*page.Image, error) {
	if err := h.validateGridBudget(); err != nil {
		return nil, err
	}
	var hSkip *page.Image
	if h.HENABLESKIP {
		hSkip = page.NewImage(int32(h.HGW), int32(h.HGH))
		if hSkip == nil {
			return nil, errors.New("failed to create skip image")
		}
		for mg := uint32(0); mg < h.HGH; mg++ {
			for ng := uint32(0); ng < h.HGW; ng++ {
				mgInt := int64(mg)
				ngInt := int64(ng)
				x := (int64(h.HGX) + mgInt*int64(h.HRY) + ngInt*int64(h.HRX)) >> 8
				y := (int64(h.HGY) + mgInt*int64(h.HRX) - ngInt*int64(h.HRY)) >> 8
				if (x+int64(h.HPW) <= 0) || (x >= int64(h.HBW)) || (y+int64(h.HPH) <= 0) || (y >= int64(h.HBH)) {
					hSkip.SetPixel(int32(ng), int32(mg), 1)
				} else {
					hSkip.SetPixel(int32(ng), int32(mg), 0)
				}
			}
		}
	}
	// hbpp = ceil(log2(HNUMPATS)) per T.88 §6.6.5.3. HNUMPATS == 1
	// -> 0: no gray planes, gsval always 0, pattern 0 every cell.
	hbpp := uint32(intmath.CeilLog2U32(h.HNUMPATS))
	grd := generic.NewProc()
	grd.MMR = h.HMMR
	grd.GBW = h.HGW
	grd.GBH = h.HGH
	grd.GBTEMPLATE = h.HTEMPLATE
	grd.TPGDON = false
	grd.USESKIP = h.HENABLESKIP
	grd.SKIP = hSkip
	if h.HTEMPLATE <= 1 {
		grd.GBAT[0] = 3
	} else {
		grd.GBAT[0] = 2
	}
	grd.GBAT[1] = -1
	if grd.GBTEMPLATE == 0 {
		grd.GBAT[2] = -3
		grd.GBAT[3] = -1
		grd.GBAT[4] = 2
		grd.GBAT[5] = -2
		grd.GBAT[6] = -2
		grd.GBAT[7] = -2
	}
	gsbpp := int(hbpp)
	gsplanes := make([]*page.Image, gsbpp)
	for i := gsbpp - 1; i >= 0; i-- {
		var pImage *page.Image
		st := &generic.ProgressiveArithDecodeState{
			Image:        &pImage,
			ArithDecoder: arithDecoder,
			GbContexts:   gbContexts,
		}
		status := grd.StartDecodeArith(st)
		if status == state.Error {
			return nil, errors.New("arith decoding failure")
		}
		if pImage == nil {
			return nil, errors.New("failed to decode plane")
		}
		gsplanes[i] = pImage
		if i < gsbpp-1 {
			gsplanes[i].ComposeFrom(0, 0, gsplanes[i+1], page.ComposeXor)
		}
	}
	return h.decodeImage(gsplanes)
}

// DecodeMMR performs MMR decoding.
func (h *HTRDProc) DecodeMMR(stream *bio.BitStream) (*page.Image, error) {
	// Per-side + total-cell budget. mmr.Decompressor.Uncompress
	// allocates `make([]int, m.width+5)` x 2 - run-offset buffers
	// proportional to width, not packed bitmap bytes. Hostile
	// HGW = 256M (passes page.MaxImagePixels at HGH = 1) drives
	// ~4 GiB. validateGridBudget shared with [HTRDProc.DecodeArith]
	// so both modes reject same shape.
	if err := h.validateGridBudget(); err != nil {
		return nil, err
	}
	// hbpp = ceil(log2(HNUMPATS)) per T.88 §6.6.5.3. See
	// DecodeArith for single-pattern case.
	hbpp := uint32(intmath.CeilLog2U32(h.HNUMPATS))
	gsbpp := int(hbpp)
	// HNUMPATS == 1 -> gsbpp == 0 -> no gray planes; every cell
	// renders pattern 0. Arith path short-circuits on gsbpp == 0;
	// this path writes gsplanes[gsbpp-1] unconditionally, panics
	// at gsplanes[-1]. Mirror arith path: decodeImage(nil).
	if gsbpp == 0 {
		return h.decodeImage(nil)
	}
	gsplanes := make([]*page.Image, gsbpp)
	j := gsbpp - 1
	decoder := mmr.NewDecompressor(int(h.HGW), int(h.HGH), stream)
	pImage, err := decoder.Uncompress()
	if err != nil {
		return nil, err
	}
	gsplanes[j] = pImage
	for j > 0 {
		j--
		decoder = mmr.NewDecompressor(int(h.HGW), int(h.HGH), stream)
		pImg, err := decoder.Uncompress()
		if err != nil {
			return nil, err
		}
		gsplanes[j] = pImg
		gsplanes[j].ComposeFrom(0, 0, gsplanes[j+1], page.ComposeXor)
	}
	return h.decodeImage(gsplanes)
}

// decodeImage assembles the halftone region from gray-scale planes.
func (h *HTRDProc) decodeImage(gsplanes []*page.Image) (*page.Image, error) {
	htReg := page.NewImage(int32(h.HBW), int32(h.HBH))
	if htReg == nil {
		return nil, errors.New("failed to create target image")
	}
	// page.NewImage zero-inits. Only Fill on ink default.
	if h.HDEFPIXEL {
		htReg.Fill(true)
	}
	for mg := uint32(0); mg < h.HGH; mg++ {
		for ng := uint32(0); ng < h.HGW; ng++ {
			gsval := uint32(0)
			for i := 0; i < len(gsplanes); i++ {
				bit := gsplanes[i].GetPixel(int32(ng), int32(mg))
				gsval |= uint32(bit) << i
			}
			// gsval = gray-plane index into HPATS. Out-of-range
			// means corrupt gray plane or malformed HNUMPATS.
			// Fail loud; clamp would silently render wrong pattern.
			patIndex := gsval
			if patIndex >= h.HNUMPATS {
				return nil, fmt.Errorf("halftone: gsval %d out of range [0,%d): %w",
					patIndex, h.HNUMPATS, errs.ErrMalformed)
			}
			mgInt := int64(mg)
			ngInt := int64(ng)
			x := (int64(h.HGX) + mgInt*int64(h.HRY) + ngInt*int64(h.HRX)) >> 8
			y := (int64(h.HGY) + mgInt*int64(h.HRX) - ngInt*int64(h.HRY)) >> 8
			pat := h.HPATS[patIndex]
			if pat != nil {
				pat.ComposeTo(htReg, int32(x), int32(y), h.HCOMBOP)
			}
		}
	}
	return htReg, nil
}
