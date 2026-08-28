// Package refinement implements JBIG2 generic refinement region
// decoding (T.88 §6.3). Re-renders a reference bitmap via delta
// context coding. Used by symbol dictionaries with
// REFAGGNINST > 1 and stand-alone refinement region segments.
package refinement

import (
	"errors"
	"fmt"

	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/state"
)

// Proc is the generic-refinement-region decoding procedure.
type Proc struct {
	GRTEMPLATE    bool
	TPGRON        bool
	GRW           uint32
	GRH           uint32
	GRREFERENCEDX int32
	GRREFERENCEDY int32
	GRREFERENCE   *page.Image
	GRAT          [4]int8
}

// NewProc creates a new generic-refinement-region decoder.
func NewProc() *Proc { return &Proc{} }

// Decode decodes a refinement region.
func (g *Proc) Decode(arithDecoder *arith.Decoder, grContexts []arith.Ctx) (*page.Image, error) {
	if g.GRW > state.MaxImageSize || g.GRH > state.MaxImageSize {
		// Adversarial dims past state.MaxImageSize. Fresh-but-
		// undecoded image would silently yield all-zero bitmap
		// (or nil on int32 overflow). Fail loud so segment
		// parser surfaces ErrResourceBudget via wrap.
		return nil, fmt.Errorf("refinement: GRW/GRH exceeds MaxImageSize: %w", errs.ErrResourceBudget)
	}
	if g.GRREFERENCE == nil {
		// Adversarial header names referred-to segment not
		// filled in (or non-existent). Error vs panic on
		// .Width() below.
		return nil, errors.New("refinement: GRREFERENCE is nil")
	}
	if !g.GRTEMPLATE {
		if g.GRAT[0] == -1 && g.GRAT[1] == -1 && g.GRAT[2] == -1 && g.GRAT[3] == -1 &&
			g.GRREFERENCEDX == 0 && int32(g.GRW) == g.GRREFERENCE.Width() {
			return g.decodeTemplate0Opt(arithDecoder, grContexts)
		}
		return g.decodeTemplate0Unopt(arithDecoder, grContexts)
	}
	if g.GRREFERENCEDX == 0 && int32(g.GRW) == g.GRREFERENCE.Width() {
		return g.decodeTemplate1Opt(arithDecoder, grContexts)
	}
	return g.decodeTemplate1Unopt(arithDecoder, grContexts)
}

func (g *Proc) decodeTemplate0Opt(decoder *arith.Decoder, contexts []arith.Ctx) (*page.Image, error) {
	return g.decodeTemplate0Unopt(decoder, contexts)
}

func (g *Proc) decodeTemplate1Opt(decoder *arith.Decoder, contexts []arith.Ctx) (*page.Image, error) {
	return g.decodeTemplate1Unopt(decoder, contexts)
}

func (g *Proc) decodeTemplate0Unopt(decoder *arith.Decoder, contexts []arith.Ctx) (*page.Image, error) {
	grReg := page.NewImage(int32(g.GRW), int32(g.GRH))
	if grReg == nil {
		// page.NewImage nil only on MaxImagePixels fire or int32
		// overflow - both resource-budget, not stream corruption.
		return nil, fmt.Errorf("refinement: failed to create image: %w", errs.ErrResourceBudget)
	}
	// page.NewImage zero-inits; no Fill(false) needed.
	ltp := 0
	lines := make([]uint32, 5)
	for h := int32(0); h < int32(g.GRH); h++ {
		if g.TPGRON {
			if decoder.IsComplete() {
				return nil, errors.New("decoder complete prematurely")
			}
			bit := decoder.Decode(&contexts[0x0010])
			if bit != 0 {
				ltp ^= 1
			}
		}
		lines[0] = uint32(grReg.GetPixel(1, h-1))
		lines[0] |= uint32(grReg.GetPixel(0, h-1)) << 1
		lines[1] = 0
		lines[2] = uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX+1, h-g.GRREFERENCEDY-1))
		lines[2] |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX, h-g.GRREFERENCEDY-1)) << 1
		lines[3] = uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX+1, h-g.GRREFERENCEDY))
		lines[3] |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX, h-g.GRREFERENCEDY)) << 1
		lines[3] |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX-1, h-g.GRREFERENCEDY)) << 2
		lines[4] = uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX+1, h-g.GRREFERENCEDY+1))
		lines[4] |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX, h-g.GRREFERENCEDY+1)) << 1
		lines[4] |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX-1, h-g.GRREFERENCEDY+1)) << 2
		if ltp == 0 {
			for w := int32(0); w < int32(g.GRW); w++ {
				CONTEXT := g.calculateContext0(grReg, lines, w, h)
				if decoder.IsComplete() {
					return nil, errors.New("decoder complete prematurely")
				}
				bVal := decoder.Decode(&contexts[CONTEXT])
				g.setPixel0(grReg, lines, w, h, bVal)
			}
		} else {
			for w := int32(0); w < int32(g.GRW); w++ {
				bVal := g.GRREFERENCE.GetPixel(w, h)
				needDecode := true
				if g.TPGRON {
					if bVal == g.GRREFERENCE.GetPixel(w-1, h-1) &&
						bVal == g.GRREFERENCE.GetPixel(w, h-1) &&
						bVal == g.GRREFERENCE.GetPixel(w+1, h-1) &&
						bVal == g.GRREFERENCE.GetPixel(w-1, h) &&
						bVal == g.GRREFERENCE.GetPixel(w+1, h) &&
						bVal == g.GRREFERENCE.GetPixel(w-1, h+1) &&
						bVal == g.GRREFERENCE.GetPixel(w, h+1) &&
						bVal == g.GRREFERENCE.GetPixel(w+1, h+1) {
						needDecode = false
					}
				}
				if needDecode {
					CONTEXT := g.calculateContext0(grReg, lines, w, h)
					if decoder.IsComplete() {
						return nil, errors.New("decoder complete prematurely")
					}
					bVal = decoder.Decode(&contexts[CONTEXT])
				}
				g.setPixel0(grReg, lines, w, h, bVal)
			}
		}
	}
	return grReg, nil
}

func (g *Proc) calculateContext0(grReg *page.Image, lines []uint32, w, h int32) uint32 {
	CONTEXT := lines[4]
	CONTEXT |= lines[3] << 3
	CONTEXT |= lines[2] << 6
	CONTEXT |= uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+int32(g.GRAT[2]), h-g.GRREFERENCEDY+int32(g.GRAT[3]))) << 8
	CONTEXT |= lines[1] << 9
	CONTEXT |= lines[0] << 10
	CONTEXT |= uint32(grReg.GetPixel(w+int32(g.GRAT[0]), h+int32(g.GRAT[1]))) << 12
	return CONTEXT
}

func (g *Proc) setPixel0(grReg *page.Image, lines []uint32, w, h int32, bVal int) {
	grReg.SetPixel(w, h, bVal)
	lines[0] = ((lines[0] << 1) | uint32(grReg.GetPixel(w+2, h-1))) & 0x03
	lines[1] = ((lines[1] << 1) | uint32(bVal)) & 0x01
	lines[2] = ((lines[2] << 1) | uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+2, h-g.GRREFERENCEDY-1))) & 0x03
	lines[3] = ((lines[3] << 1) | uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+2, h-g.GRREFERENCEDY))) & 0x07
	lines[4] = ((lines[4] << 1) | uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+2, h-g.GRREFERENCEDY+1))) & 0x07
}

func (g *Proc) decodeTemplate1Unopt(decoder *arith.Decoder, contexts []arith.Ctx) (*page.Image, error) {
	grReg := page.NewImage(int32(g.GRW), int32(g.GRH))
	if grReg == nil {
		// page.NewImage nil only on MaxImagePixels fire or int32
		// overflow - both resource-budget, not stream corruption.
		return nil, fmt.Errorf("refinement: failed to create image: %w", errs.ErrResourceBudget)
	}
	// page.NewImage zero-inits; no Fill(false) needed.
	ltp := 0
	for h := int32(0); h < int32(g.GRH); h++ {
		if g.TPGRON {
			if decoder.IsComplete() {
				return nil, errors.New("decoder complete prematurely")
			}
			bit := decoder.Decode(&contexts[0x0008])
			if bit != 0 {
				ltp ^= 1
			}
		}
		if ltp == 0 {
			line1 := uint32(grReg.GetPixel(1, h-1))
			line1 |= uint32(grReg.GetPixel(0, h-1)) << 1
			line1 |= uint32(grReg.GetPixel(-1, h-1)) << 2
			line2 := uint32(0)
			line3 := uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX, h-g.GRREFERENCEDY-1))
			line4 := uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX+1, h-g.GRREFERENCEDY))
			line4 |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX, h-g.GRREFERENCEDY)) << 1
			line4 |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX-1, h-g.GRREFERENCEDY)) << 2
			line5 := uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX+1, h-g.GRREFERENCEDY+1))
			line5 |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX, h-g.GRREFERENCEDY+1)) << 1
			for w := int32(0); w < int32(g.GRW); w++ {
				CONTEXT := line5
				CONTEXT |= line4 << 2
				CONTEXT |= line3 << 5
				CONTEXT |= line2 << 6
				CONTEXT |= line1 << 7
				if decoder.IsComplete() {
					return nil, errors.New("decoder complete prematurely")
				}
				bVal := decoder.Decode(&contexts[CONTEXT])
				grReg.SetPixel(w, h, bVal)
				line1 = ((line1 << 1) | uint32(grReg.GetPixel(w+2, h-1))) & 0x07
				line2 = ((line2 << 1) | uint32(bVal)) & 0x01
				line3 = ((line3 << 1) | uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+1, h-g.GRREFERENCEDY-1))) & 0x01
				line4 = ((line4 << 1) | uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+2, h-g.GRREFERENCEDY))) & 0x07
				line5 = ((line5 << 1) | uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+2, h-g.GRREFERENCEDY+1))) & 0x03
			}
		} else {
			line1 := uint32(grReg.GetPixel(1, h-1))
			line1 |= uint32(grReg.GetPixel(0, h-1)) << 1
			line1 |= uint32(grReg.GetPixel(-1, h-1)) << 2
			line2 := uint32(0)
			line3 := uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX, h-g.GRREFERENCEDY-1))
			line4 := uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX+1, h-g.GRREFERENCEDY))
			line4 |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX, h-g.GRREFERENCEDY)) << 1
			line4 |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX-1, h-g.GRREFERENCEDY)) << 2
			line5 := uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX+1, h-g.GRREFERENCEDY+1))
			line5 |= uint32(g.GRREFERENCE.GetPixel(-g.GRREFERENCEDX, h-g.GRREFERENCEDY+1)) << 1
			for w := int32(0); w < int32(g.GRW); w++ {
				bVal := g.GRREFERENCE.GetPixel(w, h)
				needDecode := true
				if g.TPGRON {
					if bVal == g.GRREFERENCE.GetPixel(w-1, h-1) &&
						bVal == g.GRREFERENCE.GetPixel(w, h-1) &&
						bVal == g.GRREFERENCE.GetPixel(w+1, h-1) &&
						bVal == g.GRREFERENCE.GetPixel(w-1, h) &&
						bVal == g.GRREFERENCE.GetPixel(w+1, h) &&
						bVal == g.GRREFERENCE.GetPixel(w-1, h+1) &&
						bVal == g.GRREFERENCE.GetPixel(w, h+1) &&
						bVal == g.GRREFERENCE.GetPixel(w+1, h+1) {
						needDecode = false
					}
				}
				if needDecode {
					CONTEXT := line5
					CONTEXT |= line4 << 2
					CONTEXT |= line3 << 5
					CONTEXT |= line2 << 6
					CONTEXT |= line1 << 7
					if decoder.IsComplete() {
						return nil, errors.New("decoder complete prematurely")
					}
					bVal = decoder.Decode(&contexts[CONTEXT])
				}
				grReg.SetPixel(w, h, bVal)
				line1 = ((line1 << 1) | uint32(grReg.GetPixel(w+2, h-1))) & 0x07
				line2 = ((line2 << 1) | uint32(bVal)) & 0x01
				line3 = ((line3 << 1) | uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+1, h-g.GRREFERENCEDY-1))) & 0x01
				line4 = ((line4 << 1) | uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+2, h-g.GRREFERENCEDY))) & 0x07
				line5 = ((line5 << 1) | uint32(g.GRREFERENCE.GetPixel(w-g.GRREFERENCEDX+2, h-g.GRREFERENCEDY+1))) & 0x03
			}
		}
	}
	return grReg, nil
}
