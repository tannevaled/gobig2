package symbol

import (
	"errors"
	"fmt"

	"github.com/dkrisman/gobig2/internal/arith"
	"github.com/dkrisman/gobig2/internal/bio"
	"github.com/dkrisman/gobig2/internal/errs"
	"github.com/dkrisman/gobig2/internal/generic"
	"github.com/dkrisman/gobig2/internal/huffman"
	"github.com/dkrisman/gobig2/internal/intmath"
	"github.com/dkrisman/gobig2/internal/page"
	"github.com/dkrisman/gobig2/internal/refinement"
	"github.com/dkrisman/gobig2/internal/state"
)

// SDDProc is the symbol-dictionary decoding procedure.
type SDDProc struct {
	SDHUFF        bool
	SDREFAGG      bool
	SDMMR         bool
	SDRTEMPLATE   bool
	SDTEMPLATE    uint8
	SDNUMINSYMS   uint32
	SDNUMNEWSYMS  uint32
	SDNUMEXSYMS   uint32
	SDINSYMS      []*page.Image
	SDHUFFDH      *huffman.Table
	SDHUFFDW      *huffman.Table
	SDHUFFBMSIZE  *huffman.Table
	SDHUFFAGGINST *huffman.Table
	SDAT          [8]int8
	SDRAT         [4]int8
}

// MaxRefaggninst caps REFAGGNINST per aggregate symbol. Real
// fixtures rarely exceed a few dozen; 1024 well above legitimate
// use, tight enough to block multi-second refinement-loop hangs.
// Set to 0 to disable. Override at startup for higher ceiling.
var MaxRefaggninst uint32 = DefaultMaxRefaggninst

// DefaultMaxRefaggninst is the codec's stock cap for [MaxRefaggninst].
const DefaultMaxRefaggninst uint32 = 1024

// MaxSymbolPixels caps SYMWIDTH x HCHEIGHT per symbol bitmap in
// SDDProc. page.MaxImagePixels gates page-sized + single
// generic-region alloc; this is per-symbol-dict-entry. Exists
// because adversarial inputs push single glyph to multi-megapixel
// then iterate generic-region template over every pixel - 16 MP
// "symbol" = ~10 s CPU on dev VM at default 256 MP page cap.
//
// Not every large symbol is adversarial. "Real glyphs tens of
// pixels per side" does not hold across real scanned documents:
// some encoders emit a page-sized region as ONE symbol, and 7 of
// 403 JBIG2 streams taken from public Internet Archive scans need
// between 7 and 8 MP for a single symbol. At 4 MP they are refused;
// poppler decodes all of them at its own defaults.
//
// The default is therefore MaxSymbolDictPixels, and that costs
// nothing. Total work in one dictionary is already bounded by the
// aggregate cap, which is charged for every symbol a few lines
// below this check: a dictionary cannot spend more than
// MaxSymbolDictPixels however it splits it up. Capping ONE symbol
// lower than the aggregate forbids a SHAPE, not an amount of work,
// so raising it to the aggregate widens no worst case. The
// adversarial 16 MP symbol that motivated this cap is stopped by
// the aggregate check, on the very next statement.
//
// Set 0 to disable.
var MaxSymbolPixels uint64 = DefaultMaxSymbolPixels

// DefaultMaxSymbolPixels is the codec's stock cap for [MaxSymbolPixels].
// It is [DefaultMaxSymbolDictPixels]: a lower per-symbol cap would
// forbid shapes real encoders emit without bounding any work the
// aggregate cap does not already bound.
const DefaultMaxSymbolPixels uint64 = DefaultMaxSymbolDictPixels

// MaxSymbolDictPixels caps sum of SYMWIDTH x HCHEIGHT across
// every symbol in one SDDProc call. Complements per-symbol
// [MaxSymbolPixels]: adversarial dict can declare hundreds of
// small symbols each passing per-symbol cap but accumulating to
// hundreds of MP of template-loop work (one fuzz seed: 198 MP
// across 538 symbols, ~1.7 s on dev VM).
//
// Real text-heavy dicts top out at a few MP total; 16 MP
// comfortably above legitimate use (600 DPI A4 is ~33 MP total,
// dict carries only unique glyphs) and bounds adversarial decode
// at ~135 ms vs uncapped 1.7 s. Set 0 to disable.
var MaxSymbolDictPixels uint64 = DefaultMaxSymbolDictPixels

// DefaultMaxSymbolDictPixels is the codec's stock cap for
// [MaxSymbolDictPixels].
const DefaultMaxSymbolDictPixels uint64 = 16 * 1024 * 1024

// NewSDDProc creates a new symbol-dictionary decoder.
func NewSDDProc() *SDDProc {
	return &SDDProc{}
}

// DecodeArith performs arithmetic decoding.
func (s *SDDProc) DecodeArith(arithDecoder *arith.Decoder, gbContexts, grContexts []arith.Ctx) (*Dict, error) {
	IADH := arith.NewIntDecoder()
	IADW := arith.NewIntDecoder()
	IAAI := arith.NewIntDecoder()
	IARDX := arith.NewIntDecoder()
	IARDY := arith.NewIntDecoder()
	IAEX := arith.NewIntDecoder()
	IADT := arith.NewIntDecoder()
	IAFS := arith.NewIntDecoder()
	IADS := arith.NewIntDecoder()
	IAIT := arith.NewIntDecoder()
	IARI := arith.NewIntDecoder()
	IARDW := arith.NewIntDecoder()
	IARDH := arith.NewIntDecoder()
	// Sum in + new symbol counts in uint64 so caller that
	// disabled per-dict cap via Limits.Apply can't wrap running
	// total. Reject > math.MaxUint32 - downstream consumers
	// (SBSYMCODELEN math, EXFLAGS alloc, run-end bounds) use
	// uint32 arithmetic, would reason about wrapped universe.
	totalSymbols, ok := totalSymbolsU32(s.SDNUMINSYMS, s.SDNUMNEWSYMS)
	if !ok {
		return nil, fmt.Errorf("sdd: SDNUMINSYMS=%d + SDNUMNEWSYMS=%d overflows uint32: %w",
			s.SDNUMINSYMS, s.SDNUMNEWSYMS, errs.ErrResourceBudget)
	}
	// SBSYMCODELENA = ceil(log2(totalSymbols)). total in (0, 1)
	// yields 0; counts past uint32 shift width clamp at 32.
	SBSYMCODELENA := intmath.CeilLog2U32(totalSymbols)
	IAID := arith.NewIaidDecoder(SBSYMCODELENA)
	SDNEWSYMS := make([]*page.Image, s.SDNUMNEWSYMS)
	HCHEIGHT := uint32(0)
	NSYMSDECODED := uint32(0)
	// Aggregate per-call symbol-pixel budget. Each symbol gated
	// by MaxSymbolPixels; this cap covers "many small symbols"
	// shape where each passes per-symbol cap but running total
	// balloons (one fuzz seed: 198 MP across 538 symbols in
	// single SDD call).
	aggPx := uint64(0)
	for NSYMSDECODED < s.SDNUMNEWSYMS {
		var BS *page.Image
		HCDH, ok := IADH.Decode(arithDecoder)
		if !ok {
			return nil, errors.New("failed to decode hcdh")
		}
		HCHEIGHT = uint32(int32(HCHEIGHT) + HCDH)
		if HCHEIGHT > state.MaxImageSize {
			return nil, fmt.Errorf("image height too large: %w", errs.ErrResourceBudget)
		}
		SYMWIDTH := uint32(0)
		for {
			DW, ok := IADW.Decode(arithDecoder)
			// Two height-class terminators:
			//   - encoder emits OOB on IADW (standard path);
			//   - count reached regardless of OOB - guards
			//     pathological cases where DW never contains
			//     OOB. OOB read still happens above so bit
			//     stream stays in sync; stop processing once
			//     declared count in.
			if !ok || NSYMSDECODED >= s.SDNUMNEWSYMS {
				break
			}
			SYMWIDTH = uint32(int32(SYMWIDTH) + DW)
			if SYMWIDTH > state.MaxImageSize {
				return nil, fmt.Errorf("image width too large: %w", errs.ErrResourceBudget)
			}
			if HCHEIGHT == 0 || SYMWIDTH == 0 {
				NSYMSDECODED++
				continue
			}
			// Per-symbol pixel cap. Real glyphs tens of pixels per
			// side; cap rejects multi-MP "glyphs" that iterate
			// generic-region template loop over every pixel
			// (~10 s CPU per 16 MP adversarial symbol).
			if MaxSymbolPixels > 0 &&
				uint64(SYMWIDTH)*uint64(HCHEIGHT) > MaxSymbolPixels {
				return nil, fmt.Errorf("symbol bitmap exceeds MaxSymbolPixels: %w", errs.ErrResourceBudget)
			}
			// Aggregate cap across all symbols in dict. SYMWIDTH
			// grows monotonically within height class, so
			// adversarial inputs passing per-symbol cap still
			// drive 100+ MP decode (one fuzz seed: 198 MP across
			// 538 symbols).
			aggPx += uint64(SYMWIDTH) * uint64(HCHEIGHT)
			if MaxSymbolDictPixels > 0 && aggPx > MaxSymbolDictPixels {
				return nil, fmt.Errorf("symbol dict aggregate pixels exceeds MaxSymbolDictPixels: %w", errs.ErrResourceBudget)
			}
			if !s.SDREFAGG {
				pGRD := generic.NewProc()
				pGRD.MMR = false
				pGRD.GBW = SYMWIDTH
				pGRD.GBH = HCHEIGHT
				pGRD.GBTEMPLATE = s.SDTEMPLATE
				pGRD.TPGDON = false
				pGRD.USESKIP = false
				copy(pGRD.GBAT[:], s.SDAT[:])
				var err error
				BS, err = pGRD.DecodeArith(arithDecoder, gbContexts)
				if err != nil {
					return nil, err
				}
			} else {
				REFAGGNINST, ok := IAAI.Decode(arithDecoder)
				if !ok {
					return nil, errors.New("failed to decode refaggninst")
				}
				// REFAGGNINST = instances in aggregate symbol.
				// Real fixtures: rarely > few hundred per glyph;
				// arith decoder returns millions on adversarial
				// input. Cap vs MaxRefaggninst - tighter than
				// parseTextRegion's MaxSymbolsPerDict*16 because
				// each instance triggers per-instance refinement
				// decode that dominates wall-clock.
				if REFAGGNINST < 0 || uint32(REFAGGNINST) > MaxRefaggninst {
					return nil, fmt.Errorf("refaggninst out of range: %w", errs.ErrResourceBudget)
				}
				if REFAGGNINST > 1 {
					pDecoder := NewTRDProc()
					pDecoder.SBHUFF = s.SDHUFF
					pDecoder.SBREFINE = true
					pDecoder.SBW = SYMWIDTH
					pDecoder.SBH = HCHEIGHT
					pDecoder.SBNUMINSTANCES = uint32(REFAGGNINST)
					pDecoder.SBSTRIPS = 1
					pDecoder.SBNUMSYMS = s.SDNUMINSYMS + NSYMSDECODED
					// SBSYMCODELEN = ceil(log2(SBNUMSYMS)).
					// CeilLog2U32 covers count 0/1 + clamps at 32.
					pDecoder.SBSYMCODELEN = intmath.CeilLog2U32(pDecoder.SBNUMSYMS)
					pDecoder.SBSYMS = make([]*page.Image, pDecoder.SBNUMSYMS)
					copy(pDecoder.SBSYMS, s.SDINSYMS)
					for i := 0; i < int(NSYMSDECODED); i++ {
						pDecoder.SBSYMS[int(s.SDNUMINSYMS)+i] = SDNEWSYMS[i]
					}
					pDecoder.SBDEFPIXEL = false
					pDecoder.SBCOMBOP = page.ComposeOr
					pDecoder.TRANSPOSED = false
					pDecoder.REFCORNER = CornerTopLeft
					pDecoder.SBDSOFFSET = 0
					pDecoder.SBRTEMPLATE = s.SDRTEMPLATE
					pDecoder.SBRAT = s.SDRAT
					ids := &IntDecoderState{
						IADT: IADT, IAFS: IAFS, IADS: IADS, IAIT: IAIT,
						IARI: IARI, IARDW: IARDW, IARDH: IARDH, IARDX: IARDX, IARDY: IARDY,
						IAID: IAID,
					}
					var err error
					BS, err = pDecoder.DecodeArith(arithDecoder, grContexts, ids)
					if err != nil {
						return nil, err
					}
				} else if REFAGGNINST == 1 {
					SBNUMSYMS := s.SDNUMINSYMS + NSYMSDECODED
					IDI, err := IAID.Decode(arithDecoder)
					if err != nil {
						return nil, err
					}
					if IDI >= SBNUMSYMS {
						return nil, errors.New("idi out of bounds")
					}
					var sbsymsIdi *page.Image
					if IDI < s.SDNUMINSYMS {
						sbsymsIdi = s.SDINSYMS[IDI]
					} else {
						sbsymsIdi = SDNEWSYMS[IDI-s.SDNUMINSYMS]
					}
					if sbsymsIdi == nil {
						return nil, errors.New("referenced symbol is nil")
					}
					RDXI, okX := IARDX.Decode(arithDecoder)
					if !okX {
						return nil, errors.New("sdd: IARDX.Decode failed (refinement RDX)")
					}
					RDYI, okY := IARDY.Decode(arithDecoder)
					if !okY {
						return nil, errors.New("sdd: IARDY.Decode failed (refinement RDY)")
					}
					pGRRD := refinement.NewProc()
					pGRRD.GRW = SYMWIDTH
					pGRRD.GRH = HCHEIGHT
					pGRRD.GRTEMPLATE = s.SDRTEMPLATE
					pGRRD.GRREFERENCE = sbsymsIdi
					pGRRD.GRREFERENCEDX = RDXI
					pGRRD.GRREFERENCEDY = RDYI
					pGRRD.TPGRON = false
					pGRRD.GRAT = s.SDRAT
					BS, err = pGRRD.Decode(arithDecoder, grContexts)
					if err != nil {
						return nil, err
					}
				}
			}
			SDNEWSYMS[NSYMSDECODED] = BS
			NSYMSDECODED++
		}
	}
	EXFLAGS := make([]bool, totalSymbols)
	CUREXFLAG := false
	EXINDEX := uint32(0)
	numExSyms := uint32(0)
	// Cap EXFLAGS loop on consecutive zero-runlength reads.
	// EXRUNLENGTH == 0 legal per spec (toggles flag without
	// consuming symbol slot), but adversarial input drives IAEX
	// to return 0 indefinitely - infinite loop. Two consecutive
	// zero-runs give decoder chance to flip; third = no-progress.
	zeroStreak := 0
	for EXINDEX < totalSymbols {
		// Some encoders leave int decoder signaling OOB on
		// legitimate zero EXFLAGS run-length; early bail drops
		// spec-valid fixtures. Fall through with whatever
		// EXRUNLENGTH decoder produced.
		EXRUNLENGTH, _ := IAEX.Decode(arithDecoder)
		// Reject negative EXRUNLENGTH + check run-end in uint64
		// so EXINDEX + EXRUNLENGTH can't wrap near MaxInt32.
		if EXRUNLENGTH < 0 {
			return nil, errors.New("exrunlength out of bounds")
		}
		runEnd := uint64(EXINDEX) + uint64(EXRUNLENGTH)
		if runEnd > uint64(totalSymbols) {
			return nil, errors.New("exrunlength out of bounds")
		}
		if EXRUNLENGTH == 0 {
			zeroStreak++
			if zeroStreak > 2 {
				return nil, errors.New("exrunlength stuck at zero (likely malformed input)")
			}
		} else {
			zeroStreak = 0
		}
		if CUREXFLAG {
			numExSyms += uint32(EXRUNLENGTH)
		}
		for i := uint32(0); i < uint32(EXRUNLENGTH); i++ {
			EXFLAGS[EXINDEX+i] = CUREXFLAG
		}
		EXINDEX += uint32(EXRUNLENGTH)
		CUREXFLAG = !CUREXFLAG
	}
	if numExSyms > s.SDNUMEXSYMS {
		return nil, errors.New("too many exported symbols")
	}
	dict := NewDict()
	for i := uint32(0); i < totalSymbols; i++ {
		if !EXFLAGS[i] {
			continue
		}
		if i < s.SDNUMINSYMS {
			img := s.SDINSYMS[i]
			if img != nil {
				newImg := img.Duplicate()
				dict.AddImage(newImg)
			} else {
				dict.AddImage(nil)
			}
		} else {
			dict.AddImage(SDNEWSYMS[i-s.SDNUMINSYMS])
		}
	}
	return dict, nil
}

// DecodeHuffman performs Huffman decoding.
func (s *SDDProc) DecodeHuffman(stream *bio.BitStream, gbContexts, grContexts []arith.Ctx) (*Dict, error) {
	// Same uint32-wrap guard as DecodeArith. See totalSymbolsU32.
	totalSymbols, ok := totalSymbolsU32(s.SDNUMINSYMS, s.SDNUMNEWSYMS)
	if !ok {
		return nil, fmt.Errorf("sdd: SDNUMINSYMS=%d + SDNUMNEWSYMS=%d overflows uint32: %w",
			s.SDNUMINSYMS, s.SDNUMNEWSYMS, errs.ErrResourceBudget)
	}
	huffmanDecoder := huffman.NewDecoder(stream)
	SDNEWSYMS := make([]*page.Image, s.SDNUMNEWSYMS)
	var SDNEWSYMWIDTHS []uint32
	if !s.SDREFAGG {
		SDNEWSYMWIDTHS = make([]uint32, s.SDNUMNEWSYMS)
	}
	HCHEIGHT := uint32(0)
	NSYMSDECODED := uint32(0)
	aggPx := uint64(0)
	for NSYMSDECODED < s.SDNUMNEWSYMS {
		var HCDH int32
		if res := huffmanDecoder.DecodeAValue(s.SDHUFFDH, &HCDH); res != 0 {
			return nil, errors.New("failed to decode hcdh")
		}
		HCHEIGHT = uint32(int32(HCHEIGHT) + HCDH)
		if HCHEIGHT > state.MaxImageSize {
			return nil, fmt.Errorf("image height too large: %w", errs.ErrResourceBudget)
		}
		SYMWIDTH := uint32(0)
		TOTWIDTH := uint32(0)
		HCFIRSTSYM := NSYMSDECODED
		for {
			// Some encoders skip trailing OOB after last dict
			// symbol; others emit it. Try the read; if already
			// had enough symbols and read wasn't OOB, rewind
			// stream so BMSIZE picks up from right position.
			// Huffman path only - bio.BitStream pointer is only
			// state to undo; arith decoder's internal state
			// machine can't roll back same way.
			savedBitPos := stream.GetBitPos()
			var DW int32
			res := huffmanDecoder.DecodeAValue(s.SDHUFFDW, &DW)
			if res < 0 {
				return nil, errors.New("failed to decode dw")
			}
			if res == state.OOB {
				break
			}
			if NSYMSDECODED >= s.SDNUMNEWSYMS {
				stream.SetBitPos(savedBitPos)
				break
			}
			SYMWIDTH = uint32(int32(SYMWIDTH) + DW)
			if SYMWIDTH > state.MaxImageSize {
				return nil, fmt.Errorf("image width too large: %w", errs.ErrResourceBudget)
			}
			// Add in uint64 + reject overflow before assigning
			// to TOTWIDTH (uint32). Default MaxSymbolDictPixels
			// cap fires long before TOTWIDTH wraps; disabling
			// policy cap shouldn't kill arithmetic-safety
			// invariant. Reject > state.MaxImageSize so
			// downstream `int32(TOTWIDTH)` cast at BHC NewImage
			// stays representable.
			sumWidth := uint64(TOTWIDTH) + uint64(SYMWIDTH)
			if sumWidth > uint64(state.MaxImageSize) {
				return nil, fmt.Errorf("collective TOTWIDTH %d exceeds state.MaxImageSize: %w",
					sumWidth, errs.ErrResourceBudget)
			}
			TOTWIDTH = uint32(sumWidth)
			if HCHEIGHT == 0 || SYMWIDTH == 0 {
				NSYMSDECODED++
				continue
			}
			// Per-symbol pixel cap (Huffman path).
			if MaxSymbolPixels > 0 &&
				uint64(SYMWIDTH)*uint64(HCHEIGHT) > MaxSymbolPixels {
				return nil, fmt.Errorf("symbol bitmap exceeds MaxSymbolPixels: %w", errs.ErrResourceBudget)
			}
			// Aggregate cap (Huffman path).
			aggPx += uint64(SYMWIDTH) * uint64(HCHEIGHT)
			if MaxSymbolDictPixels > 0 && aggPx > MaxSymbolDictPixels {
				return nil, fmt.Errorf("symbol dict aggregate pixels exceeds MaxSymbolDictPixels: %w", errs.ErrResourceBudget)
			}
			var BS *page.Image
			if s.SDREFAGG {
				var REFAGGNINST int32
				if huffmanDecoder.DecodeAValue(s.SDHUFFAGGINST, &REFAGGNINST) != 0 {
					return nil, errors.New("failed to decode refaggninst")
				}
				// Same adversarial bound as arith path: input can
				// push REFAGGNINST > MaxImageSize, inner TRD then
				// iterates. Cap vs MaxRefaggninst - tighter than
				// parseTextRegion's MaxSymbolsPerDict*16 because
				// each instance triggers per-instance refinement
				// decode that dominates wall-clock.
				if REFAGGNINST < 0 || uint32(REFAGGNINST) > MaxRefaggninst {
					return nil, fmt.Errorf("refaggninst out of range: %w", errs.ErrResourceBudget)
				}
				if REFAGGNINST > 1 {
					pDecoder := NewTRDProc()
					pDecoder.SBHUFF = s.SDHUFF
					pDecoder.SBREFINE = true
					pDecoder.SBW = SYMWIDTH
					pDecoder.SBH = HCHEIGHT
					pDecoder.SBNUMINSTANCES = uint32(REFAGGNINST)
					pDecoder.SBSTRIPS = 1
					pDecoder.SBNUMSYMS = s.SDNUMINSYMS + NSYMSDECODED
					pDecoder.SBSYMCODES = make([]huffman.Code, pDecoder.SBNUMSYMS)
					// Per T.88 §6.5.8.2.4: SBSYMCODELEN is dict-wide,
					// from FULL SDNUMINSYMS + SDNUMNEWSYMS, not
					// running pre-this-symbol count. Running count
					// drops 1 bit per IDI for symbols below next
					// power-of-2, silently misaligns bit stream.
					// totalSymbols already uint32-wrap-checked at entry.
					fullSym := totalSymbols
					// nTmp = ceil(log2(fullSym)) per T.88: strict
					// spec returns 0 at fullSym == 1 (single symbol,
					// no bits needed). 1-bit floor would mis-encode
					// single-symbol case; no corpus fixture
					// exercises it but spec explicit.
					nTmp := uint32(intmath.CeilLog2U32(fullSym))
					for i := uint32(0); i < pDecoder.SBNUMSYMS; i++ {
						pDecoder.SBSYMCODES[i].Codelen = int32(nTmp)
						pDecoder.SBSYMCODES[i].Code = int32(i)
					}
					pDecoder.SBSYMS = make([]*page.Image, pDecoder.SBNUMSYMS)
					copy(pDecoder.SBSYMS, s.SDINSYMS)
					for i := 0; i < int(NSYMSDECODED); i++ {
						pDecoder.SBSYMS[int(s.SDNUMINSYMS)+i] = SDNEWSYMS[i]
					}
					pDecoder.SBDEFPIXEL = false
					pDecoder.SBCOMBOP = page.ComposeOr
					pDecoder.TRANSPOSED = false
					pDecoder.REFCORNER = CornerTopLeft
					pDecoder.SBDSOFFSET = 0
					pDecoder.SBHUFFFS = huffman.NewStandardTable(6)
					pDecoder.SBHUFFDS = huffman.NewStandardTable(8)
					pDecoder.SBHUFFDT = huffman.NewStandardTable(11)
					pDecoder.SBHUFFRDW = huffman.NewStandardTable(15)
					pDecoder.SBHUFFRDH = huffman.NewStandardTable(15)
					pDecoder.SBHUFFRDX = huffman.NewStandardTable(15)
					pDecoder.SBHUFFRDY = huffman.NewStandardTable(15)
					pDecoder.SBHUFFRSIZE = huffman.NewStandardTable(1)
					pDecoder.SBRTEMPLATE = s.SDRTEMPLATE
					pDecoder.SBRAT = s.SDRAT
					var err error
					BS, err = pDecoder.DecodeHuffman(stream, grContexts)
					if err != nil {
						return nil, err
					}
				} else if REFAGGNINST == 1 {
					// Per T.88 §6.5.8.2.4, IDI width is dict-wide
					// ceil(log2(SDNUMINSYMS + SDNUMNEWSYMS)), once
					// per dict, NOT running (SDNUMINSYMS +
					// NSYMSDECODED). Running count drops one bit
					// per IDI for symbols below power-of-2,
					// misaligns stream, surfaces as "idi out of
					// bounds" or "read 1 bit failed" on
					// refinement-heavy fixtures.
					// totalSymbols already uint32-wrap-checked at entry.
					SBNUMSYMS := totalSymbols
					// SBSYMCODELEN = ceil(log2(SBNUMSYMS)) per
					// T.88 - strict spec returns 0 at single-
					// symbol case. Floor dropped alongside halftone
					// HBPP follow-up; no corpus fixture exercises
					// SBNUMSYMS == 1.
					SBSYMCODELEN := intmath.CeilLog2U32(SBNUMSYMS)
					// Bound check still uses NSYMSDECODED -
					// encoder may reference any of SDNUMINSYMS
					// imported + NSYMSDECODED decoded-new symbols.
					// IDI >= SDNUMINSYMS+NSYMSDECODED illegal even
					// if bit field has room.
					boundSyms := s.SDNUMINSYMS + NSYMSDECODED
					IDI := uint32(0)
					for n := uint32(0); n < uint32(SBSYMCODELEN); n++ {
						val, err := stream.Read1Bit()
						if err != nil {
							return nil, err
						}
						IDI = (IDI << 1) | val
					}
					if IDI >= boundSyms {
						return nil, fmt.Errorf("idi out of bounds: IDI=%d SBNUMSYMS=%d NSYMSDECODED=%d byteIdx=%d",
							IDI, boundSyms, NSYMSDECODED, stream.GetOffset())
					}
					var sbsymsIdi *page.Image
					if IDI < s.SDNUMINSYMS {
						sbsymsIdi = s.SDINSYMS[IDI]
					} else {
						sbsymsIdi = SDNEWSYMS[IDI-s.SDNUMINSYMS]
					}
					if sbsymsIdi == nil {
						return nil, errors.New("referenced symbol is nil")
					}
					SBHUFFRDX := huffman.NewStandardTable(15)
					SBHUFFRSIZE := huffman.NewStandardTable(1)
					var RDXI, RDYI, nVal int32
					if huffmanDecoder.DecodeAValue(SBHUFFRDX, &RDXI) != 0 ||
						huffmanDecoder.DecodeAValue(SBHUFFRDX, &RDYI) != 0 ||
						huffmanDecoder.DecodeAValue(SBHUFFRSIZE, &nVal) != 0 {
						return nil, errors.New("failed to decode refinement values")
					}
					stream.AlignByte()
					pGRRD := refinement.NewProc()
					pGRRD.GRW = SYMWIDTH
					pGRRD.GRH = HCHEIGHT
					pGRRD.GRTEMPLATE = s.SDRTEMPLATE
					pGRRD.GRREFERENCE = sbsymsIdi
					pGRRD.GRREFERENCEDX = RDXI
					pGRRD.GRREFERENCEDY = RDYI
					pGRRD.TPGRON = false
					pGRRD.GRAT = s.SDRAT
					arithDecoder := arith.NewDecoder(stream)
					var err error
					BS, err = pGRRD.Decode(arithDecoder, grContexts)
					if err != nil {
						return nil, err
					}
					stream.AlignByte()
					stream.AddOffset(2)
					// nVal = encoder-declared refinement payload
					// size; spec allows mismatch with actual
					// stream advance under some legal encodings.
					// No parity check.
					_ = nVal
				}
				SDNEWSYMS[NSYMSDECODED] = BS
			}
			if !s.SDREFAGG {
				SDNEWSYMWIDTHS[NSYMSDECODED] = SYMWIDTH
			}
			NSYMSDECODED++
		}
		if !s.SDREFAGG {
			var BMSIZE int32
			if huffmanDecoder.DecodeAValue(s.SDHUFFBMSIZE, &BMSIZE) != 0 {
				return nil, errors.New("failed to decode bmsize")
			}
			// Negative BMSIZE would wrap to a huge uint32 below
			// and then panic in make([]byte, BMSIZE). Reject up
			// front - the Huffman decoder can return negatives.
			if BMSIZE < 0 {
				return nil, errors.New("bmsize negative")
			}
			stream.AlignByte()
			var BHC *page.Image
			if BMSIZE == 0 {
				stride := (TOTWIDTH + 7) / 8
				if stream.GetByteLeft() < stride*HCHEIGHT {
					return nil, errors.New("insufficient data for grid")
				}
				BHC = page.NewImage(int32(TOTWIDTH), int32(HCHEIGHT))
				if BHC == nil {
					// TOTWIDTH * HCHEIGHT past page.MaxImagePixels -
					// adversarial dim. Error not panic at BHC.Data().
					return nil, errors.New("collective bitmap exceeds image budget")
				}
				data := stream.GetPointer()
				bhcData := BHC.Data()
				for i := uint32(0); i < HCHEIGHT; i++ {
					copy(bhcData[int32(i)*BHC.Stride():], data[i*stride:i*stride+stride])
				}
				stream.AddOffset(stride * HCHEIGHT)
			} else {
				pGRD := generic.NewProc()
				if s.SDHUFF {
					pGRD.MMR = true
				} else {
					pGRD.MMR = s.SDMMR
				}
				pGRD.GBW = TOTWIDTH
				pGRD.GBH = HCHEIGHT
				if !pGRD.MMR {
					pGRD.GBAT = [8]int8{0, 0, 0, 0, 0, 0, 0, 0}
					gbContexts := make([]arith.Ctx, 65536)
					arithDecoder := arith.NewDecoder(stream)
					var err error
					BHC, err = pGRD.DecodeArith(arithDecoder, gbContexts)
					if err != nil {
						return nil, err
					}
				} else {
					if stream.GetByteLeft() < uint32(BMSIZE) {
						return nil, errors.New("insufficient data for mmr")
					}
					mmrData := make([]byte, BMSIZE)
					for i := int32(0); i < BMSIZE; i++ {
						val, err := stream.Read1Byte()
						if err != nil {
							return nil, err
						}
						mmrData[i] = val
					}
					mmrStream := bio.NewBitStream(mmrData, 0)
					pGRD.StartDecodeMMR(&BHC, mmrStream)
				}
			}
			if BHC != nil {
				nTmp := uint32(0)
				currentSym := HCFIRSTSYM
				for i := uint32(0); i < NSYMSDECODED-HCFIRSTSYM; i++ {
					idx := currentSym + i
					SDNEWSYMS[idx] = BHC.SubImage(int32(nTmp), 0, int32(SDNEWSYMWIDTHS[idx]), int32(HCHEIGHT))
					nTmp += SDNEWSYMWIDTHS[idx]
				}
			}
		}
	}
	EXFLAGS := make([]bool, totalSymbols)
	CUREXFLAG := false
	EXINDEX := uint32(0)
	numExSyms := uint32(0)
	pTable := huffman.NewStandardTable(1)
	zeroStreak := 0
	for EXINDEX < totalSymbols {
		var EXRUNLENGTH int32
		if res := huffmanDecoder.DecodeAValue(pTable, &EXRUNLENGTH); res != 0 {
			return nil, errors.New("failed to decode exrunlength")
		}
		// Reject negative EXRUNLENGTH explicitly + check run-end
		// in uint64 so EXINDEX + EXRUNLENGTH can't wrap.
		if EXRUNLENGTH < 0 {
			return nil, errors.New("exrunlength out of bounds")
		}
		runEnd := uint64(EXINDEX) + uint64(EXRUNLENGTH)
		if runEnd > uint64(totalSymbols) {
			return nil, errors.New("exrunlength out of bounds")
		}
		if EXRUNLENGTH == 0 {
			zeroStreak++
			if zeroStreak > 2 {
				return nil, errors.New("exrunlength stuck at zero (likely malformed input)")
			}
		} else {
			zeroStreak = 0
		}
		if CUREXFLAG {
			numExSyms += uint32(EXRUNLENGTH)
		}
		for i := uint32(0); i < uint32(EXRUNLENGTH); i++ {
			EXFLAGS[EXINDEX+i] = CUREXFLAG
		}
		EXINDEX += uint32(EXRUNLENGTH)
		CUREXFLAG = !CUREXFLAG
	}
	if numExSyms > s.SDNUMEXSYMS {
		return nil, errors.New("too many exported symbols")
	}
	dict := NewDict()
	for i := uint32(0); i < totalSymbols; i++ {
		if !EXFLAGS[i] {
			continue
		}
		if i < s.SDNUMINSYMS {
			img := s.SDINSYMS[i]
			if img != nil {
				newImg := img.Duplicate()
				dict.AddImage(newImg)
			} else {
				dict.AddImage(nil)
			}
		} else {
			dict.AddImage(SDNEWSYMS[i-s.SDNUMINSYMS])
		}
	}
	return dict, nil
}

// totalSymbolsU32 sums two symbol counts in uint64, reports if
// result fits uint32. Used by SDDProc.Decode* for SBSYMCODELEN /
// SBNUMSYMS / EXFLAGS bounds without wrapping when caller
// disables per-dict cap.
func totalSymbolsU32(ins, news uint32) (uint32, bool) {
	sum := uint64(ins) + uint64(news)
	if sum > 0xFFFFFFFF {
		return 0, false
	}
	return uint32(sum), true
}
