package dbus

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

func TestPrimitiveWireAlignment(t *testing.T) {
	// Independently laid out wire vector: BYTE, padded UINT32, UINT64,
	// STRING, then VARIANT(INT64) aligned to eight.
	want := []byte{7, 0, 0, 0, 0x78, 0x56, 0x34, 0x12, 8, 7, 6, 5, 4, 3, 2, 1, 1, 0, 0, 0, 'x', 0, 1, 'x', 0, 0, 0, 0, 0, 0, 0, 0, 0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	values := []any{byte(7), uint32(0x12345678), uint64(0x0102030405060708), "x", Variant{"x", int64(-2)}}
	wire, err := encodeValues("yutsv", values)
	if err != nil || !bytes.Equal(wire, want) {
		t.Fatalf("wire %x, want %x (%v)", wire, want, err)
	}
	got, err := decodeValues("yutsv", want, binary.LittleEndian)
	if err != nil || !reflect.DeepEqual(got, values) {
		t.Fatalf("decoded %v (%v)", got, err)
	}
	big := []byte{0xff, 0xff, 0xff, 0xfe, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}
	got, err = decodeValues("it", big, binary.BigEndian)
	if err != nil || !reflect.DeepEqual(got, []any{int32(-2), uint64(0x0102030405060708)}) {
		t.Fatalf("big endian %v (%v)", got, err)
	}
}

func TestBigEndianMessage(t *testing.T) {
	// Fixed header, reply-serial field, signature field, one padding byte,
	// and a signed INT32 body. This vector does not use the encoder.
	wire := []byte{'B', 2, 0, 1, 0, 0, 0, 4, 0, 0, 0, 7, 0, 0, 0, 15, 5, 1, 'u', 0, 0, 0, 0, 1, 8, 1, 'g', 0, 1, 'i', 0, 0, 0xff, 0xff, 0xff, 0xfe}
	m, e := readMessage(bytes.NewReader(wire))
	if e != nil || m.serial != 7 || m.reply != 1 || m.signature != "i" || m.values[0] != int32(-2) {
		t.Fatalf("big endian message %+v (%v)", m, e)
	}
}

func TestMalformedPrimitiveBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, sig string
		b         []byte
	}{
		{"nonzero alignment", "yu", []byte{1, 1, 0, 0, 1, 0, 0, 0}},
		{"truncated alignment", "yt", []byte{1, 0}},
		{"string length bomb", "s", []byte{0xff, 0xff, 0xff, 0xff}},
		{"missing nul", "s", []byte{1, 0, 0, 0, 'a', 1}},
		{"embedded nul", "s", []byte{2, 0, 0, 0, 0, 'a', 0}},
		{"invalid utf8", "s", []byte{1, 0, 0, 0, 0xff, 0}},
		{"object path", "o", []byte{3, 0, 0, 0, '/', 'a', '/', 0}},
		{"bad signature", "g", []byte{1, '(', 0}},
		{"multiple variant types", "v", []byte{2, 'i', 'i', 0, 0, 0, 0, 0}},
		{"empty variant", "v", []byte{0, 0}},
		{"nested variant unsupported", "v", []byte{1, 'v', 0}},
		{"truncated variant alignment", "v", []byte{1, 'x', 0, 0}},
		{"trailing bytes", "u", []byte{1, 0, 0, 0, 0}},
		{"unsupported containers", "as", []byte{0, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeValues(tc.sig, tc.b, binary.LittleEndian); err == nil {
				t.Fatal("accepted malformed data")
			}
		})
	}
}

func reply(t *testing.T, kind byte, serial, replySerial uint32, sig string, v ...any) []byte {
	t.Helper()
	fields := []field{{5, "u", replySerial}}
	if kind == 3 {
		fields = append(fields, field{4, "s", "org.freedesktop.DBus.Error.AccessDenied"})
	}
	b, e := marshal(kind, serial, fields, sig, v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestMessageValidation(t *testing.T) {
	good := reply(t, 2, 7, 1, "v", Variant{"i", int32(20)})
	m, e := readMessage(bytes.NewReader(good))
	if e != nil || m.serial != 7 || m.reply != 1 || m.values[0].(Variant).Value != int32(20) {
		t.Fatalf("message %+v %v", m, e)
	}
	for n := 0; n < len(good); n++ {
		if _, e := readMessage(bytes.NewReader(good[:n])); e == nil {
			t.Fatalf("accepted truncated message of %d bytes", n)
		}
	}
	for _, tc := range []struct {
		name string
		edit func([]byte)
	}{
		{"byte order", func(b []byte) { b[0] = 'z' }},
		{"protocol", func(b []byte) { b[3] = 2 }},
		{"type", func(b []byte) { b[1] = 0 }},
		{"serial zero", func(b []byte) { binary.LittleEndian.PutUint32(b[8:12], 0) }},
		{"reply zero", func(b []byte) { binary.LittleEndian.PutUint32(b[20:24], 0) }},
		{"body allocation bomb", func(b []byte) { binary.LittleEndian.PutUint32(b[4:8], ^uint32(0)) }},
		{"header allocation bomb", func(b []byte) { binary.LittleEndian.PutUint32(b[12:16], ^uint32(0)) }},
		{"wrong field type", func(b []byte) { b[18] = 'i' }},
		{"invalid signature", func(b []byte) { b[28] = '(' }},
		{"header padding", func(b []byte) { b[31] = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := bytes.Clone(good)
			tc.edit(b)
			if _, e := readMessage(bytes.NewReader(b)); e == nil {
				t.Fatal("accepted malformed message")
			}
		})
	}
	for _, fields := range [][]field{
		{{5, "u", uint32(1)}, {5, "u", uint32(1)}},
		{{9, "u", uint32(1)}, {5, "u", uint32(1)}},
		{{4, "s", "org.test.Error"}},
	} {
		b, e := marshal(2, 1, fields, "", nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e := readMessage(bytes.NewReader(b)); e == nil {
			t.Fatal("accepted invalid reply headers")
		}
	}
	if _, e := marshal(1, 0, nil, "", nil); !errors.Is(e, errProtocol) {
		t.Fatal("accepted zero outgoing serial")
	}
}

func FuzzReadMessage(f *testing.F) {
	b, _ := marshal(2, 1, []field{{5, "u", uint32(1)}}, "v", []any{Variant{"x", int64(200000)}})
	f.Add(b)
	f.Add([]byte("l"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > maxMessage {
			return
		}
		_, _ = readMessage(bytes.NewReader(b))
	})
}
