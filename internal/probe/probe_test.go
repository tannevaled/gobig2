package probe

import (
	"errors"
	"testing"

	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/segment"
)

func TestValidSegmentTypeRespectsCanonicalTable(t *testing.T) {
	defined := map[byte]bool{
		0: true, 4: true, 6: true, 7: true,
		16: true,
		20: true, 22: true, 23: true,
		36: true, 38: true, 39: true,
		40: true, 42: true, 43: true,
		48: true, 49: true, 50: true, 51: true, 52: true, 53: true,
		62: true,
	}
	for code := 0; code < 64; code++ {
		got := ValidSegmentType(byte(code))
		want := defined[byte(code)]
		if got != want {
			t.Errorf("ValidSegmentType(%d) = %v, want %v", code, got, want)
		}
	}
	for code := 0; code < 64; code++ {
		kind, _ := segment.TypeInfo(byte(code))
		regOK := kind != segment.TypeKindReserved
		if regOK != defined[byte(code)] {
			t.Errorf("segment.TypeInfo(%d).kind reserved? = %v, want defined %v",
				code, !regOK, defined[byte(code)])
		}
	}
}

func TestRejectUnsupportedOrg(t *testing.T) {
	cases := []struct {
		name string
		ra   bool
		org  int
		want error
	}{
		{"sequential", false, 0, nil},
		{"grouped", false, 1, nil},
		{"random-access grouped", true, 1, nil},
		{"random-access sequential", true, 0, errs.ErrUnsupported},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := RejectUnsupportedOrg(c.ra, c.org)
			if c.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want errors.Is(err, %v)", err, c.want)
			}
		})
	}
}

func TestSniffEmbeddedJBIG2LittleEndian(t *testing.T) {
	header := []byte{
		0x01, 0x00, 0x00, 0x00, // segNum (LE 1, primes DetectEmbeddedEndianness)
		0x30,                   // flags: type 48 = page info
		0x00,                   // refByte (0 refs)
		0x01,                   // page-assoc (1 byte)
		0x13, 0x00, 0x00, 0x00, // dataLen = 19, little-endian
	}
	body := make([]byte, 19)
	full := make([]byte, 0, len(header)+len(body))
	full = append(full, header...)
	full = append(full, body...)
	if err := SniffEmbeddedJBIG2(full); err != nil {
		t.Fatalf("LE-encoded plausible dataLen rejected: %v", err)
	}

	hostile := make([]byte, len(header))
	copy(hostile, header)
	hostile[7], hostile[8], hostile[9], hostile[10] = 0xFE, 0xFF, 0xFF, 0xFF
	hostile = append(hostile, body...)
	if err := SniffEmbeddedJBIG2(hostile); err == nil {
		t.Error("hostile LE dataLen not rejected by sniff")
	}
}
