package input

import (
	"fmt"
	"io"

	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/errs"
)

// MaxBytes caps the physical bytes a single JBIG2 input can
// occupy. Aliased from [bio.MaxInputBytes] so callers here
// don't need to import bio for the limit.
const MaxBytes = bio.MaxInputBytes

// ReadBounded slurps the constructor's io.Reader with a hard
// cap from [MaxBytes]. Reads up to MaxBytes+1 to detect
// over-cap, then fails fast with [errs.ErrResourceBudget].
// Without this, over-cap input later surfaces as ErrMalformed
// via bio.NewBitStream nilling the data, hiding the real cause.
func ReadBounded(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	// LimitReader caps at MaxBytes+1. Reading that many bytes
	// means original was MaxBytes+1 or larger - either way reject.
	limited := &io.LimitedReader{R: r, N: int64(MaxBytes) + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > int64(MaxBytes) {
		return nil, fmt.Errorf(
			"jbig2: input exceeds bio.MaxInputBytes (%d): %w",
			MaxBytes, errs.ErrResourceBudget,
		)
	}
	return data, nil
}

// CheckGlobals applies the [MaxBytes] cap to a pre-buffered
// globals slice. [ReadBounded] covers main stream; globals
// arrive as []byte (typically PDF /JBIG2Globals object) so
// they need explicit length check to surface as
// ErrResourceBudget instead of the malformed-stream error
// bio.NewBitStream produces by nilling data.
func CheckGlobals(globals []byte) error {
	return CheckGlobalsLen(len(globals))
}

// CheckGlobalsLen is the length-only predicate CheckGlobals
// delegates to. Split out so cap behavior can be tested
// without allocating the MaxBytes+1 (256 MiB) backing slice -
// allocation would make tests fragile under race/parallel
// runs or low-memory CI.
func CheckGlobalsLen(n int) error {
	if n > MaxBytes {
		return fmt.Errorf(
			"jbig2: globals exceed MaxInputBytes (%d): %w",
			MaxBytes, errs.ErrResourceBudget,
		)
	}
	return nil
}
