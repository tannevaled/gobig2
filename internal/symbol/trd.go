package symbol

import (
	"errors"
	"fmt"

	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/huffman"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/refinement"
	"github.com/tannevaled/gobig2/internal/state"
)

// errTRDAllocFailed is the resource-budget rejection both TRD
// decode paths use when page.NewImage rejects SBW x SBH. Fires
// on non-positive dim, int32 overflow, or page.MaxImagePixels.
var errTRDAllocFailed = fmt.Errorf("trd: SBW/SBH bitmap allocation rejected: %w", errs.ErrResourceBudget)

// ComposeData holds composition placement data.
type ComposeData struct {
	x, y      int32
	increment int32
}

// Corner identifies a corner of a region.
type Corner int

const (
	CornerBottomLeft  Corner = 0
	CornerTopLeft     Corner = 1
	CornerBottomRight Corner = 2
	CornerTopRight    Corner = 3
)

// TRDProc is the text-region decoding procedure.
type TRDProc struct {
	SBHUFF         bool
	SBREFINE       bool
	SBRTEMPLATE    bool
	TRANSPOSED     bool
	SBDEFPIXEL     bool
	SBDSOFFSET     int8
	SBSYMCODELEN   uint8
	SBW            uint32
	SBH            uint32
	SBNUMINSTANCES uint32
	SBSTRIPS       uint32
	SBNUMSYMS      uint32
	SBSYMCODES     []huffman.Code
	SBSYMS         []*page.Image
	SBCOMBOP       page.ComposeOp
	REFCORNER      Corner
	SBHUFFFS       *huffman.Table
	SBHUFFDS       *huffman.Table
	SBHUFFDT       *huffman.Table
	SBHUFFRDW      *huffman.Table
	SBHUFFRDH      *huffman.Table
	SBHUFFRDX      *huffman.Table
	SBHUFFRDY      *huffman.Table
	SBHUFFRSIZE    *huffman.Table
	SBRAT          [4]int8
}

// IntDecoderState bundles the integer-decoder state for a text region.
type IntDecoderState struct {
	IADT, IAFS, IADS, IAIT, IARI *arith.IntDecoder
	IARDW, IARDH, IARDX, IARDY   *arith.IntDecoder
	IAID                         *arith.IaidDecoder
}

// NewTRDProc creates a new text-region decoder.
func NewTRDProc() *TRDProc {
	return &TRDProc{
		SBSTRIPS: 1,
	}
}

// GetComposeData returns composition placement for one symbol instance.
// SI, TI: instance reference coords. WI, HI: symbol width/height.
func (t *TRDProc) GetComposeData(SI, TI int32, WI, HI uint32) ComposeData {
	var results ComposeData
	s := SI
	tVal := TI
	if !t.TRANSPOSED {
		results.x = s
		results.y = tVal
		switch t.REFCORNER {
		case CornerBottomLeft:
			results.y = tVal - int32(HI) + 1
		case CornerBottomRight:
			results.x = s - int32(WI) + 1
			results.y = tVal - int32(HI) + 1
		case CornerTopLeft:
			results.x = s
			results.y = tVal
		case CornerTopRight:
			results.x = s - int32(WI) + 1
		}
		results.increment = int32(WI) - 1
	} else {
		results.x = tVal
		results.y = s
		switch t.REFCORNER {
		case CornerBottomLeft:
			results.x = tVal - int32(HI) + 1
		case CornerBottomRight:
			results.x = tVal - int32(HI) + 1
			results.y = s - int32(WI) + 1
		case CornerTopLeft:
			results.x = tVal
			results.y = s
		case CornerTopRight:
			results.y = s - int32(WI) + 1
		}
		results.increment = int32(HI) - 1
	}
	return results
}

// checkTRDDimension validates dimension + delta. Returns new
// dimension and validity.
func checkTRDDimension(dimension uint32, delta int32) (uint32, bool) {
	res := int64(dimension) + int64(delta)
	if res < 0 || res > 0xFFFFFFFF {
		return 0, false
	}
	return uint32(res), true
}

// checkTRDReferenceDimension validates offset + (dimension >> shift).
// Returns new coord and validity.
func checkTRDReferenceDimension(dimension int32, shift uint32, offset int32) (int32, bool) {
	res := int64(offset) + (int64(dimension) >> shift)
	if res < -2147483648 || res > 2147483647 {
		return 0, false
	}
	return int32(res), true
}

// DecodeHuffman performs Huffman decoding.
func (t *TRDProc) DecodeHuffman(stream *bio.BitStream, grContexts []arith.Ctx) (*page.Image, error) {
	// Required tables for unconditional paths. Refinement tables
	// (SBHUFFRDW/RDH/RDX/RDY/RSIZE) checked at point of use - only
	// hit when per-instance refinement branch fires.
	if t.SBHUFFDT == nil || t.SBHUFFFS == nil || t.SBHUFFDS == nil {
		return nil, errors.New("trd: required SBHUFF* table is nil")
	}
	sbReg := page.NewImage(int32(t.SBW), int32(t.SBH))
	if sbReg == nil {
		return nil, errTRDAllocFailed
	}
	// page.NewImage zero-inits the buffer (= paper). Fill only
	// when SBDEFPIXEL is ink, otherwise the call would rewrite
	// an already-zero buffer.
	if t.SBDEFPIXEL {
		sbReg.Fill(true)
	}
	decoder := huffman.NewDecoder(stream)
	var initialStript int32
	if res := decoder.DecodeAValue(t.SBHUFFDT, &initialStript); res != 0 {
		return nil, errors.New("huffman decode failed for sbhuffdt")
	}
	STRIPT := -int64(initialStript) * int64(t.SBSTRIPS)
	FIRSTS := int64(0)
	NINSTANCES := uint32(0)
	// Total-strip cap: region can't have more strips than
	// instances. Without it, NINSTANCES never advances (every IBI
	// nil per strip) -> outer loop spins. Mirrors arith path.
	stripsBudget := t.SBNUMINSTANCES + 1
	for NINSTANCES < t.SBNUMINSTANCES {
		if stripsBudget == 0 {
			return nil, errors.New("trd: strips iteration cap exceeded")
		}
		stripsBudget--
		var initialDt int32
		if res := decoder.DecodeAValue(t.SBHUFFDT, &initialDt); res != 0 {
			return nil, errors.New("huffman decode failed for sbhuffdt in loop")
		}
		STRIPT += int64(initialDt) * int64(t.SBSTRIPS)
		bFirst := true
		CURS := int64(0)
		// Per-strip iteration cap: a strip can't have more
		// instances than SBNUMINSTANCES. Without it, adversarial
		// input producing nil IBI (refinement dim check fails, or
		// SBSYMS entry nil from zero-pixel referenced symbol)
		// burns bits since NINSTANCES never advances. Mirrors
		// arith path.
		stripBudget := t.SBNUMINSTANCES + 1
		for {
			if stripBudget == 0 {
				return nil, errors.New("trd: strip iteration cap exceeded")
			}
			stripBudget--
			if bFirst {
				var dfs int32
				if res := decoder.DecodeAValue(t.SBHUFFFS, &dfs); res != 0 {
					return nil, errors.New("huffman decode failed for sbhufffs")
				}
				FIRSTS += int64(dfs)
				CURS = FIRSTS
				bFirst = false
			} else {
				// Save bit pos to rewind if decoded IDS is
				// non-OOB past instance cap - matches
				// SDDProc.DecodeHuffman's DW loop pattern.
				savedBitPos := stream.GetBitPos()
				var ids int32
				res := decoder.DecodeAValue(t.SBHUFFDS, &ids)
				if res < 0 {
					return nil, errors.New("huffman decode failed for sbhuffds")
				}
				if res == state.OOB {
					// Encoder emitted OOB - bits legitimate, strip ends.
					break
				}
				if NINSTANCES >= t.SBNUMINSTANCES {
					// Encoder skipped trailing OOB; rewind so
					// next phase starts at right byte. Keeps
					// stream in sync when strip's S values reach
					// declared count without OOB terminator.
					stream.SetBitPos(savedBitPos)
					break
				}
				currDso := int32(t.SBDSOFFSET)
				if currDso >= 16 {
					currDso -= 32
				}
				CURS += int64(ids) + int64(currDso)
			}
			CURT := int32(0)
			if t.SBSTRIPS != 1 {
				nTmp := uint32(1)
				for uint32(1<<nTmp) < t.SBSTRIPS {
					nTmp++
				}
				var val uint32
				val, err := stream.ReadNBits(nTmp)
				if err != nil {
					return nil, errors.New("read nbits failed")
				}
				CURT = int32(val)
			}
			TI := int32(STRIPT + int64(CURT))
			nSafeVal := int32(0)
			nBits := 0
			IDI := uint32(0)
			// Cap symbol-ID bit decode loop same as
			// DecodeSymbolIDHuffmanTable: 32-bit prefix
			// exceeds legitimate JBIG2 symbol-ID codes; past
			// that = adversarial bits, no match in SBSYMCODES.
			const maxIDBits = 32
			idBits := 0
			for {
				if idBits >= maxIDBits {
					return nil, errors.New("trd: symbol-ID bit loop exceeded 32 bits without match")
				}
				idBits++
				var nTmp uint32
				val, err := stream.Read1Bit()
				if err != nil {
					return nil, errors.New("read 1 bit failed")
				}
				nTmp = val
				nSafeVal = (nSafeVal << 1) | int32(nTmp)
				nBits++
				for IDI = 0; IDI < t.SBNUMSYMS; IDI++ {
					if int32(nBits) == t.SBSYMCODES[IDI].Codelen && nSafeVal == t.SBSYMCODES[IDI].Code {
						break
					}
				}
				if IDI < t.SBNUMSYMS {
					break
				}
			}
			var RI uint32 = 0
			if t.SBREFINE {
				val, err := stream.Read1Bit()
				if err != nil {
					return nil, errors.New("read refine bit failed")
				}
				RI = val
			}
			var IBI *page.Image
			if RI == 0 {
				if IDI >= uint32(len(t.SBSYMS)) {
					return nil, fmt.Errorf("idi out of bounds: IDI=%d SBNUMSYMS=%d NINSTANCES=%d", IDI, t.SBNUMSYMS, NINSTANCES)
				}
				IBI = t.SBSYMS[IDI]
			} else {
				if t.SBHUFFRDW == nil || t.SBHUFFRDH == nil || t.SBHUFFRDX == nil || t.SBHUFFRDY == nil || t.SBHUFFRSIZE == nil {
					return nil, errors.New("trd: refinement SBHUFF* table is nil")
				}
				var rdwi, rdhi, rdxi, rdyi, uffrsize int32
				if decoder.DecodeAValue(t.SBHUFFRDW, &rdwi) != 0 ||
					decoder.DecodeAValue(t.SBHUFFRDH, &rdhi) != 0 ||
					decoder.DecodeAValue(t.SBHUFFRDX, &rdxi) != 0 ||
					decoder.DecodeAValue(t.SBHUFFRDY, &rdyi) != 0 ||
					decoder.DecodeAValue(t.SBHUFFRSIZE, &uffrsize) != 0 {
					return nil, errors.New("huffman decode refine values failed")
				}
				stream.AlignByte()
				IBOI := t.SBSYMS[IDI]
				if IBOI == nil {
					return nil, errors.New("failed to get iboi")
				}
				WOI, okW := checkTRDDimension(uint32(IBOI.Width()), rdwi)
				HOI, okH := checkTRDDimension(uint32(IBOI.Height()), rdhi)
				if !okW || !okH {
					return nil, errors.New("dimension check failed")
				}
				// Refinement reference offset = RDX + (RDW >> 1)
				// per T.88. Arith TR path uses shift=1; Huffman
				// path needs same or refined glyphs land off
				// reference for non-zero deltas.
				refDX, okDX := checkTRDReferenceDimension(rdwi, 1, rdxi)
				refDY, okDY := checkTRDReferenceDimension(rdhi, 1, rdyi)
				if !okDX || !okDY {
					return nil, errors.New("ref check failed")
				}
				pGRRD := refinement.NewProc()
				pGRRD.GRW = WOI
				pGRRD.GRH = HOI
				pGRRD.GRTEMPLATE = t.SBRTEMPLATE
				pGRRD.GRREFERENCE = IBOI
				pGRRD.GRREFERENCEDX = refDX
				pGRRD.GRREFERENCEDY = refDY
				pGRRD.TPGRON = false
				pGRRD.GRAT = t.SBRAT
				pArithDecoder := arith.NewDecoder(stream)
				var err error
				IBI, err = pGRRD.Decode(pArithDecoder, grContexts)
				if err != nil {
					return nil, err
				}
				stream.AlignByte()
				stream.AddOffset(2)
				// uffrsize is encoder-declared refinement payload
				// size; spec allows mismatch with actual stream
				// advance under some legal encodings. No parity check.
				_ = uffrsize
			}
			if IBI == nil {
				// Nil IBI = resolved symbol slot empty (dict
				// decoded with zero-pixel glyph at index, or
				// refinement returned no bitmap). Without
				// non-nil IBI, composition skipped, NINSTANCES
				// stuck, only per-strip budget bounds loop.
				// Treat as malformed so cause surfaces.
				return nil, errors.New("trd: resolved symbol IBI is nil")
			}
			WI := uint32(IBI.Width())
			HI := uint32(IBI.Height())
			if !t.TRANSPOSED && (t.REFCORNER == CornerTopRight || t.REFCORNER == CornerBottomRight) {
				CURS += int64(WI) - 1
			} else if t.TRANSPOSED && (t.REFCORNER == CornerBottomLeft || t.REFCORNER == CornerBottomRight) {
				CURS += int64(HI) - 1
			}
			SI := int32(CURS)
			compose := t.GetComposeData(SI, TI, WI, HI)
			IBI.ComposeTo(sbReg, compose.x, compose.y, t.SBCOMBOP)
			CURS += int64(compose.increment)
			NINSTANCES++
		}
	}
	return sbReg, nil
}

// DecodeArith performs arithmetic decoding.
func (t *TRDProc) DecodeArith(arithDecoder *arith.Decoder, grContexts []arith.Ctx, ids *IntDecoderState) (*page.Image, error) {
	var pIADT, pIAFS, pIADS, pIAIT, pIARI, pIARDW, pIARDH, pIARDX, pIARDY *arith.IntDecoder
	var pIAID *arith.IaidDecoder
	if ids != nil {
		pIADT = ids.IADT
		pIAFS = ids.IAFS
		pIADS = ids.IADS
		pIAIT = ids.IAIT
		pIARI = ids.IARI
		pIARDW = ids.IARDW
		pIARDH = ids.IARDH
		pIARDX = ids.IARDX
		pIARDY = ids.IARDY
		pIAID = ids.IAID
	}
	if pIADT == nil {
		pIADT = arith.NewIntDecoder()
	}
	if pIAFS == nil {
		pIAFS = arith.NewIntDecoder()
	}
	if pIADS == nil {
		pIADS = arith.NewIntDecoder()
	}
	if pIAIT == nil {
		pIAIT = arith.NewIntDecoder()
	}
	if pIARI == nil {
		pIARI = arith.NewIntDecoder()
	}
	if pIARDW == nil {
		pIARDW = arith.NewIntDecoder()
	}
	if pIARDH == nil {
		pIARDH = arith.NewIntDecoder()
	}
	if pIARDX == nil {
		pIARDX = arith.NewIntDecoder()
	}
	if pIARDY == nil {
		pIARDY = arith.NewIntDecoder()
	}
	if pIAID == nil {
		pIAID = arith.NewIaidDecoder(t.SBSYMCODELEN)
	}
	sbReg := page.NewImage(int32(t.SBW), int32(t.SBH))
	if sbReg == nil {
		return nil, errTRDAllocFailed
	}
	// page.NewImage zero-inits the buffer (= paper). Fill only
	// when SBDEFPIXEL is ink, otherwise the call would rewrite
	// an already-zero buffer.
	if t.SBDEFPIXEL {
		sbReg.Fill(true)
	}
	var initialStript int32
	if res, ok := pIADT.Decode(arithDecoder); !ok {
		return nil, errors.New("failed to decode initial stript")
	} else {
		initialStript = res
	}
	STRIPT := int64(initialStript) * int64(t.SBSTRIPS)
	STRIPT = -STRIPT
	FIRSTS := int64(0)
	NINSTANCES := uint32(0)
	// Total-strip cap: region can't have more strips than
	// instances. Without it, NINSTANCES stuck (every IBI nil
	// per strip) -> outer loop spins.
	stripsBudget := t.SBNUMINSTANCES + 1
	for NINSTANCES < t.SBNUMINSTANCES {
		if stripsBudget == 0 {
			return nil, errors.New("trd: strips iteration cap exceeded")
		}
		stripsBudget--
		var initialDt int32
		if res, ok := pIADT.Decode(arithDecoder); !ok {
			return nil, errors.New("iadt decode failed")
		} else {
			initialDt = res
		}
		STRIPT += int64(initialDt) * int64(t.SBSTRIPS)
		bFirst := true
		CURS := int64(0)
		// Per-strip iteration cap: strip can't have more
		// instances than SBNUMINSTANCES. Without it, adversarial
		// input producing nil IBI (refinement dim check fails,
		// or SBSYMS entry nil from zero-pixel referenced symbol)
		// burns arith bits since NINSTANCES never advances.
		stripBudget := t.SBNUMINSTANCES + 1
		for {
			if stripBudget == 0 {
				return nil, errors.New("trd: strip iteration cap exceeded")
			}
			stripBudget--
			if bFirst {
				dfs, ok := pIAFS.Decode(arithDecoder)
				if !ok {
					return nil, errors.New("trd: pIAFS.Decode failed (FIRSTS)")
				}
				FIRSTS += int64(dfs)
				CURS = FIRSTS
				bFirst = false
			} else {
				idsVal, ok := pIADS.Decode(arithDecoder)
				if !ok {
					break
				}
				dso := int32(t.SBDSOFFSET)
				if dso >= 16 {
					dso -= 32
				}
				CURS += int64(idsVal) + int64(dso)
			}
			if NINSTANCES >= t.SBNUMINSTANCES {
				break
			}
			CURT := int32(0)
			if t.SBSTRIPS != 1 {
				res, ok := pIAIT.Decode(arithDecoder)
				if !ok {
					return nil, errors.New("trd: pIAIT.Decode failed (CURT)")
				}
				CURT = res
			}
			TI := int32(STRIPT + int64(CURT))
			IDI, err := pIAID.Decode(arithDecoder)
			if err != nil {
				return nil, err
			}
			if IDI >= t.SBNUMSYMS {
				return nil, errors.New("idi out of bounds")
			}
			RI := int32(0)
			if t.SBREFINE {
				res, ok := pIARI.Decode(arithDecoder)
				if !ok {
					return nil, errors.New("trd: pIARI.Decode failed (refinement select)")
				}
				RI = res
			}
			var IBI *page.Image
			if RI == 0 {
				if IDI < uint32(len(t.SBSYMS)) {
					IBI = t.SBSYMS[IDI]
				}
			} else {
				rdwi, okW2 := pIARDW.Decode(arithDecoder)
				rdhi, okH2 := pIARDH.Decode(arithDecoder)
				rdxi, okX2 := pIARDX.Decode(arithDecoder)
				rdyi, okY2 := pIARDY.Decode(arithDecoder)
				if !okW2 || !okH2 || !okX2 || !okY2 {
					return nil, errors.New("trd: pIARD{W,H,X,Y}.Decode failed (refinement geometry)")
				}
				IBOI := t.SBSYMS[IDI]
				if IBOI != nil {
					WOI, okW := checkTRDDimension(uint32(IBOI.Width()), rdwi)
					HOI, okH := checkTRDDimension(uint32(IBOI.Height()), rdhi)
					refDX, okDX := checkTRDReferenceDimension(rdwi, 1, rdxi)
					refDY, okDY := checkTRDReferenceDimension(rdhi, 1, rdyi)
					if okW && okH && okDX && okDY {
						pGRRD := refinement.NewProc()
						pGRRD.GRW = WOI
						pGRRD.GRH = HOI
						pGRRD.GRTEMPLATE = t.SBRTEMPLATE
						pGRRD.GRREFERENCE = IBOI
						pGRRD.GRREFERENCEDX = refDX
						pGRRD.GRREFERENCEDY = refDY
						pGRRD.TPGRON = false
						pGRRD.GRAT = t.SBRAT
						// Propagate refinement decoder error,
						// don't swallow. refinement.Decode
						// wraps ErrResourceBudget/ErrMalformed
						// at leaves; document.go's
						// classifyLeafErr preserves that.
						img, derr := pGRRD.Decode(arithDecoder, grContexts)
						if derr != nil {
							return nil, derr
						}
						IBI = img
					}
				}
			}
			if IBI != nil {
				WI := uint32(IBI.Width())
				HI := uint32(IBI.Height())
				if !t.TRANSPOSED && (t.REFCORNER == CornerTopRight || t.REFCORNER == CornerBottomRight) {
					CURS += int64(WI) - 1
				} else if t.TRANSPOSED && (t.REFCORNER == CornerBottomLeft || t.REFCORNER == CornerBottomRight) {
					CURS += int64(HI) - 1
				}
				SI := int32(CURS)
				compose := t.GetComposeData(SI, TI, WI, HI)
				IBI.ComposeTo(sbReg, compose.x, compose.y, t.SBCOMBOP)
				if compose.increment > 0 {
					CURS += int64(compose.increment)
				}
				NINSTANCES++
			}
		}
	}
	return sbReg, nil
}
