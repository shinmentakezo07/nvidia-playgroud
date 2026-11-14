package hcaptcha

import "testing"

// TestDecodeTruncatedBinExt checks that truncation inside bin/ext payloads is a
// decode error, not a slice-bounds panic: a malformed getcaptcha response must
// fail the extraction, not terminate the pool worker.
func TestDecodeTruncatedBinExt(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"bin8 payload missing", []byte{0xc4, 0x02, 0x01}},
		{"bin8 length only", []byte{0xc4, 0x02}},
		{"bin16 payload missing", []byte{0xc5, 0x00, 0x02, 0x01}},
		{"bin32 payload missing", []byte{0xc6, 0x00, 0x00, 0x00, 0x02, 0x01}},
		{"bin32 length overflow", []byte{0xc6, 0xff, 0xff, 0xff, 0xff, 0x01}},
		{"ext8 type only", []byte{0xc7, 0x02, 0x12}},
		{"ext8 payload missing", []byte{0xc7, 0x02, 0x12, 0x01}},
		{"ext16 payload missing", []byte{0xc8, 0x00, 0x02, 0x12, 0x01}},
		{"ext32 payload missing", []byte{0xc9, 0x00, 0x00, 0x00, 0x02, 0x12, 0x01}},
		{"ext32 length overflow", []byte{0xc9, 0xff, 0xff, 0xff, 0xff, 0x12, 0x01}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panic on %#v: %v", tc.in, r)
					}
				}()
				_, _, err = msgpackDecode(tc.in)
			}()
			if err == nil {
				t.Fatalf("msgpackDecode(%#v) succeeded, want truncation error", tc.in)
			}
		})
	}
}

// TestDecodeCompleteBinExt keeps the accepted shapes working: a full payload
// decodes and leaves the trailing bytes for the caller.
func TestDecodeCompleteBinExt(t *testing.T) {
	v, rest, err := msgpackDecode([]byte{0xc4, 0x02, 0x01, 0x02, 0xaa})
	if err != nil {
		t.Fatalf("bin: %v", err)
	}
	if b, ok := v.([]byte); !ok || len(b) != 2 || b[0] != 1 || b[1] != 2 {
		t.Fatalf("bin = %#v", v)
	}
	if len(rest) != 1 || rest[0] != 0xaa {
		t.Fatalf("bin rest = %#v", rest)
	}

	v, rest, err = msgpackDecode([]byte{0xc7, 0x02, 0x12, 0x01, 0x02, 0xaa})
	if err != nil {
		t.Fatalf("ext: %v", err)
	}
	if b, ok := v.([]byte); !ok || len(b) != 2 || b[0] != 1 || b[1] != 2 {
		t.Fatalf("ext = %#v", v)
	}
	if len(rest) != 1 || rest[0] != 0xaa {
		t.Fatalf("ext rest = %#v", rest)
	}
}

// TestDecodeHugeContainerLength checks that a length prefix larger than the
// remaining input is rejected before it is used to preallocate. array32/map32
// can declare ~4e9 elements in 4 bytes; allocating on that count turns one
// crafted getcaptcha payload into an OOM kill.
func TestDecodeHugeContainerLength(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"array32 enormous", []byte{0xdd, 0xff, 0xff, 0xff, 0xff}},
		{"array32 oversized", []byte{0xdd, 0x00, 0x00, 0x10, 0x00}},
		{"map32 enormous", []byte{0xdf, 0xff, 0xff, 0xff, 0xff}},
		{"map32 oversized", []byte{0xdf, 0x00, 0x00, 0x10, 0x00}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panic on %#v: %v", tc.in, r)
					}
				}()
				_, _, err = msgpackDecode(tc.in)
			}()
			if err == nil {
				t.Fatalf("msgpackDecode(%#v) succeeded, want a length error", tc.in)
			}
		})
	}
}
