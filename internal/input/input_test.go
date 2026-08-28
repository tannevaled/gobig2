package input

import (
	"errors"
	"io"
	"testing"

	"github.com/tannevaled/gobig2/internal/errs"
)

func TestReadBoundedCap(t *testing.T) {
	overCap := int64(MaxBytes) + 1
	r := &countingZeroReader{remaining: overCap}

	_, err := ReadBounded(r)
	if err == nil {
		t.Fatal("ReadBounded accepted over-cap input")
	}
	if !errors.Is(err, errs.ErrResourceBudget) {
		t.Errorf("expected ErrResourceBudget, got: %v", err)
	}
}

func TestCheckGlobalsLen(t *testing.T) {
	if err := CheckGlobalsLen(MaxBytes + 1); err == nil {
		t.Errorf("CheckGlobalsLen accepted MaxBytes+1")
	} else if !errors.Is(err, errs.ErrResourceBudget) {
		t.Errorf("oversize globals: want ErrResourceBudget, got %v", err)
	}
	if err := CheckGlobalsLen(MaxBytes); err != nil {
		t.Errorf("at-cap globals length rejected: %v", err)
	}
	if err := CheckGlobalsLen(0); err != nil {
		t.Errorf("zero-length globals rejected: %v", err)
	}
}

type countingZeroReader struct {
	remaining int64
}

func (r *countingZeroReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := range p[:n] {
		p[i] = 0
	}
	r.remaining -= n
	return int(n), nil
}
