package arith

import (
	"testing"

	"github.com/tannevaled/gobig2/internal/bio"
)

// FuzzDecoder feeds random bytes to MQ decoder and integer/IAID
// adapters. Contract: no panic, including streams driving Decode
// past supplied bytes (coder is IsComplete()-aware not
// length-aware).
//
// Cap decode-step count per input; adversarial state can loop on
// stale buffer bytes arbitrarily.
func FuzzDecoder(f *testing.F) {
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte{0x97, 0x4A, 0x42, 0x32, 0x0D, 0x0A, 0x1A, 0x0A})
	f.Add(make([]byte, 64))
	f.Fuzz(func(t *testing.T, data []byte) {
		const maxSteps = 4096

		// Plain MQ decode against a single context.
		stream := bio.NewBitStream(data, 0)
		dec := NewDecoder(stream)
		var cx Ctx
		for i := 0; i < maxSteps; i++ {
			if dec.IsComplete() {
				break
			}
			_ = dec.Decode(&cx)
		}

		// Integer decoder over a fresh stream.
		stream2 := bio.NewBitStream(data, 0)
		dec2 := NewDecoder(stream2)
		intDec := NewIntDecoder()
		for i := 0; i < 32; i++ {
			if dec2.IsComplete() {
				break
			}
			_, _ = intDec.Decode(dec2)
		}

		// IAID decoder. Small/mid widths only - MaxIaidCodeLen=30
		// would allocate ~2 GiB Ctx per input via NewIaidDecoder.
		// Clamping math + decode bounds check covered by unit
		// tests in arith_test.go that bypass allocation.
		for _, codeLen := range []uint8{1, 4, 8, 12} {
			stream3 := bio.NewBitStream(data, 0)
			dec3 := NewDecoder(stream3)
			iaid := NewIaidDecoder(codeLen)
			for i := 0; i < 16; i++ {
				if dec3.IsComplete() {
					break
				}
				_, _ = iaid.Decode(dec3)
			}
		}
	})
}
