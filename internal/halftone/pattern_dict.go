// Package halftone implements halftone region decoding
// (T.88 §6.6, type-22/23 segments) and the pattern dictionary
// it indexes (T.88 §6.7, type-16). HPATS points at HDPATS, so
// shared package.
package halftone

import "github.com/tannevaled/gobig2/internal/page"

// PatternDict is a pattern dictionary.
type PatternDict struct {
	NUMPATS uint32
	HDPATS  []*page.Image
}

// MaxPatternsPerDict caps declared pattern count (HDPATS[]) so
// an adversarial header can't drive multi-GB pointer-slice alloc
// before decode (8 bytes per *Image at uint32 max -> 32 GiB).
// Real fixtures rarely declare > few thousand; 1M well above
// legitimate use. Set to 0 to disable.
var MaxPatternsPerDict uint32 = DefaultMaxPatternsPerDict

// DefaultMaxPatternsPerDict is the codec's stock cap for
// [MaxPatternsPerDict].
const DefaultMaxPatternsPerDict uint32 = 1 << 20

// NewPatternDict creates a pattern dictionary. Returns nil if
// dictSize exceeds MaxPatternsPerDict.
func NewPatternDict(dictSize uint32) *PatternDict {
	if MaxPatternsPerDict > 0 && dictSize > MaxPatternsPerDict {
		return nil
	}
	return &PatternDict{
		NUMPATS: dictSize,
		HDPATS:  make([]*page.Image, dictSize),
	}
}

// DeepCopy returns a deep copy of the dictionary.
func (p *PatternDict) DeepCopy() *PatternDict {
	dst := NewPatternDict(p.NUMPATS)
	for i, img := range p.HDPATS {
		if img != nil {
			dst.HDPATS[i] = img.Duplicate()
		}
	}
	return dst
}
