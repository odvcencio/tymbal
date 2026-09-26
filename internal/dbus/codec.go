// Package dbus implements the small, bounded subset of D-Bus used by realtime
// priority negotiation. It intentionally rejects unsupported container types and
// FD passing rather than providing a general-purpose D-Bus implementation.
package dbus

import (
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const maxMessage = 64 << 10
const maxHeader = 16 << 10

var errProtocol = errors.New("dbus: malformed or unsupported message")

// Variant holds one supported primitive property value.
type Variant struct {
	Signature string
	Value     any
}

type decoder struct {
	b     []byte
	pos   int
	order binary.ByteOrder
}

func (d *decoder) take(n int) ([]byte, error) {
	if n < 0 || n > len(d.b)-d.pos {
		return nil, errProtocol
	}
	b := d.b[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}
func (d *decoder) align(n int) error {
	b, err := d.take((-d.pos) & (n - 1))
	if err != nil {
		return err
	}
	for _, v := range b {
		if v != 0 {
			return errProtocol
		}
	}
	return nil
}
func supported(sig string) bool {
	if len(sig) > 255 {
		return false
	}
	for _, c := range sig {
		if !strings.ContainsRune("yiuxtsogv", c) {
			return false
		}
	}
	return true
}
func validPath(s string) bool {
	if s == "/" {
		return true
	}
	if !strings.HasPrefix(s, "/") {
		return false
	}
	for _, part := range strings.Split(s[1:], "/") {
		if part == "" {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				return false
			}
		}
	}
	return true
}
func validElement(s string, digitsFirst, hyphen bool) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= '0' && c <= '9' && (i > 0 || digitsFirst) || hyphen && c == '-') {
			return false
		}
	}
	return true
}
func validInterface(s string) bool {
	if len(s) > 255 {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !validElement(part, false, false) {
			return false
		}
	}
	return true
}
func validMember(s string) bool { return len(s) <= 255 && validElement(s, false, false) }
func validBusName(s string) bool {
	if len(s) > 255 || s == "" {
		return false
	}
	unique := s[0] == ':'
	if unique {
		s = s[1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !validElement(part, unique, true) {
			return false
		}
	}
	return true
}

func (d *decoder) value(t byte, depth int) (any, error) {
	if depth > 8 {
		return nil, errProtocol
	}
	switch t {
	case 'y':
		b, e := d.take(1)
		if e != nil {
			return nil, e
		}
		return b[0], nil
	case 'i', 'u':
		if e := d.align(4); e != nil {
			return nil, e
		}
		b, e := d.take(4)
		if e != nil {
			return nil, e
		}
		v := d.order.Uint32(b)
		if t == 'i' {
			return int32(v), nil
		}
		return v, nil
	case 'x', 't':
		if e := d.align(8); e != nil {
			return nil, e
		}
		b, e := d.take(8)
		if e != nil {
			return nil, e
		}
		v := d.order.Uint64(b)
		if t == 'x' {
			return int64(v), nil
		}
		return v, nil
	case 's', 'o', 'g':
		var n uint32
		if t == 'g' {
			b, e := d.take(1)
			if e != nil {
				return nil, e
			}
			n = uint32(b[0])
		} else {
			if e := d.align(4); e != nil {
				return nil, e
			}
			b, e := d.take(4)
			if e != nil {
				return nil, e
			}
			n = d.order.Uint32(b)
		}
		if uint64(n)+1 > uint64(len(d.b)-d.pos) {
			return nil, errProtocol
		}
		b, e := d.take(int(n) + 1)
		if e != nil {
			return nil, e
		}
		s := string(b[:n])
		if b[n] != 0 || strings.IndexByte(s, 0) >= 0 || !utf8.ValidString(s) {
			return nil, errProtocol
		}
		if t == 'g' && !supported(s) || t == 'o' && !validPath(s) {
			return nil, errProtocol
		}
		return s, nil
	case 'v':
		v, e := d.value('g', depth+1)
		if e != nil {
			return nil, e
		}
		sig := v.(string)
		if len(sig) != 1 || sig == "v" {
			return nil, errProtocol
		}
		v, e = d.value(sig[0], depth+1)
		if e != nil {
			return nil, e
		}
		return Variant{sig, v}, nil
	}
	return nil, errProtocol
}

type encoder struct{ b []byte }

func (e *encoder) align(n int) { e.b = append(e.b, make([]byte, (-len(e.b))&(n-1))...) }
func (e *encoder) value(t byte, v any) error {
	switch t {
	case 'y':
		x, ok := v.(byte)
		if !ok {
			return errProtocol
		}
		e.b = append(e.b, x)
	case 'i', 'u':
		var x uint32
		if t == 'i' {
			v, ok := v.(int32)
			if !ok {
				return errProtocol
			}
			x = uint32(v)
		} else {
			v, ok := v.(uint32)
			if !ok {
				return errProtocol
			}
			x = v
		}
		e.align(4)
		e.b = binary.LittleEndian.AppendUint32(e.b, x)
	case 'x', 't':
		var x uint64
		if t == 'x' {
			v, ok := v.(int64)
			if !ok {
				return errProtocol
			}
			x = uint64(v)
		} else {
			v, ok := v.(uint64)
			if !ok {
				return errProtocol
			}
			x = v
		}
		e.align(8)
		e.b = binary.LittleEndian.AppendUint64(e.b, x)
	case 's', 'o', 'g':
		s, ok := v.(string)
		if !ok || !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 || len(s) > maxMessage {
			return errProtocol
		}
		if t == 'g' {
			if !supported(s) {
				return errProtocol
			}
			e.b = append(e.b, byte(len(s)))
		} else {
			if t == 'o' && !validPath(s) {
				return errProtocol
			}
			e.align(4)
			e.b = binary.LittleEndian.AppendUint32(e.b, uint32(len(s)))
		}
		e.b = append(e.b, s...)
		e.b = append(e.b, 0)
	case 'v':
		x, ok := v.(Variant)
		if !ok || len(x.Signature) != 1 || x.Signature == "v" {
			return errProtocol
		}
		if err := e.value('g', x.Signature); err != nil {
			return err
		}
		return e.value(x.Signature[0], x.Value)
	default:
		return errProtocol
	}
	if len(e.b) > maxMessage {
		return errProtocol
	}
	return nil
}
func encodeValues(sig string, values []any) ([]byte, error) {
	if !supported(sig) || len(sig) != len(values) {
		return nil, errProtocol
	}
	e := encoder{}
	for i := range sig {
		if err := e.value(sig[i], values[i]); err != nil {
			return nil, err
		}
	}
	return e.b, nil
}
func decodeValues(sig string, b []byte, order binary.ByteOrder) ([]any, error) {
	if !supported(sig) {
		return nil, errProtocol
	}
	d := decoder{b: b, order: order}
	v := make([]any, 0, len(sig))
	for i := range sig {
		x, e := d.value(sig[i], 0)
		if e != nil {
			return nil, e
		}
		v = append(v, x)
	}
	if d.pos != len(b) {
		return nil, errProtocol
	}
	return v, nil
}

type message struct {
	kind                 byte
	serial, reply        uint32
	signature, errorName string
	values               []any
}
type field struct {
	code      byte
	signature string
	value     any
}

func marshal(kind byte, serial uint32, fields []field, sig string, values []any) ([]byte, error) {
	if serial == 0 || kind < 1 || kind > 4 {
		return nil, errProtocol
	}
	body, err := encodeValues(sig, values)
	if err != nil {
		return nil, err
	}
	h := encoder{b: make([]byte, 16)}
	if sig != "" {
		fields = append(fields, field{8, "g", sig})
	}
	for _, f := range fields {
		h.align(8)
		h.b = append(h.b, f.code)
		if err := h.value('v', Variant{f.signature, f.value}); err != nil {
			return nil, err
		}
	}
	if len(h.b)-16 > maxHeader {
		return nil, errProtocol
	}
	headerLen := len(h.b) - 16
	h.align(8)
	h.b = append(h.b, body...)
	if len(h.b) > maxMessage {
		return nil, errProtocol
	}
	h.b[0] = 'l'
	h.b[1] = kind
	if kind == 1 {
		h.b[2] = 2
	} // NO_AUTO_START: never start a host service implicitly.
	h.b[3] = 1
	binary.LittleEndian.PutUint32(h.b[4:8], uint32(len(body)))
	binary.LittleEndian.PutUint32(h.b[8:12], serial)
	binary.LittleEndian.PutUint32(h.b[12:16], uint32(headerLen))
	return h.b, nil
}
func readMessage(r io.Reader) (message, error) {
	var fixed [16]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return message{}, err
	}
	var order binary.ByteOrder
	switch fixed[0] {
	case 'l':
		order = binary.LittleEndian
	case 'B':
		order = binary.BigEndian
	default:
		return message{}, errProtocol
	}
	m := message{kind: fixed[1], serial: order.Uint32(fixed[8:12])}
	if fixed[3] != 1 || m.kind < 1 || m.kind > 4 || m.serial == 0 {
		return message{}, errProtocol
	}
	bodyLen := uint64(order.Uint32(fixed[4:8]))
	headerLen := uint64(order.Uint32(fixed[12:16]))
	padded := (headerLen + 7) &^ 7
	if headerLen > maxHeader || 16+padded+bodyLen > maxMessage {
		return message{}, errProtocol
	}
	b := make([]byte, int(padded+bodyLen))
	if _, err := io.ReadFull(r, b); err != nil {
		return message{}, err
	}
	d := decoder{b: b[:headerLen], order: order}
	seen := [256]bool{}
	for d.pos < len(d.b) {
		if err := d.align(8); err != nil || d.pos == len(d.b) {
			return message{}, errProtocol
		}
		code, e := d.value('y', 0)
		if e != nil {
			return message{}, e
		}
		c := code.(byte)
		if c == 0 || seen[c] {
			return message{}, errProtocol
		}
		seen[c] = true
		v, e := d.value('v', 0)
		if e != nil {
			return message{}, e
		}
		x := v.(Variant)
		if c <= 9 {
			want := []string{"", "o", "s", "s", "s", "u", "s", "s", "g", "u"}[c]
			if x.Signature != want {
				return message{}, errProtocol
			}
		}
		switch c {
		case 2:
			if !validInterface(x.Value.(string)) {
				return message{}, errProtocol
			}
		case 3:
			if !validMember(x.Value.(string)) {
				return message{}, errProtocol
			}
		case 6, 7:
			if !validBusName(x.Value.(string)) {
				return message{}, errProtocol
			}
		case 4:
			if !validInterface(x.Value.(string)) {
				return message{}, errProtocol
			}
			m.errorName = x.Value.(string)
		case 5:
			m.reply = x.Value.(uint32)
			if m.reply == 0 {
				return message{}, errProtocol
			}
		case 8:
			m.signature = x.Value.(string)
		case 9:
			if x.Value.(uint32) != 0 {
				return message{}, errProtocol
			}
		}
	}
	for _, v := range b[headerLen:padded] {
		if v != 0 {
			return message{}, errProtocol
		}
	}
	switch m.kind {
	case 1:
		if !seen[1] || !seen[3] {
			return message{}, errProtocol
		}
	case 2, 3:
		if !seen[5] || m.kind == 3 && (!seen[4] || m.errorName == "") {
			return message{}, errProtocol
		}
	case 4:
		if !seen[1] || !seen[2] || !seen[3] {
			return message{}, errProtocol
		}
	}
	values, e := decodeValues(m.signature, b[padded:], order)
	m.values = values
	return m, e
}
