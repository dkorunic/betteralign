package indirect

import (
	"encoding/binary"
	"io"
	"reflect"
	"sync/atomic"
	"unsafe"
)

// One hop of indirection between a struct and the call reading its layout:
// a variable given the operand once, or a function forwarding its parameter
// straight to the call. Each pinned struct has an unpinned control below.

// ViaVar's word reaches the atomic through a variable defined once.
type ViaVar struct { // want `reorder skipped: 64-bit atomic access at .*indirect.go:22:`
	n int64
	p *int
	b byte
}

func (s *ViaVar) Inc() { p := &s.n; atomic.AddInt64(p, 1) }

// Reassigned's variable is reassigned, so its first value is not followed.
type Reassigned struct { // want `struct with 16 pointer bytes could be 8$`
	n int64
	p *int
	b byte
}

func (s *Reassigned) Inc(other *int64) {
	p := &s.n
	p = other
	atomic.AddInt64(p, 1)
}

// ViaHelper's word goes through a function passing its parameter on.
type ViaHelper struct { // want `reorder skipped: 64-bit atomic access at .*indirect.go:44:`
	n int64
	p *int
	b byte
}

func (s *ViaHelper) Inc() { add(&s.n) }

func add(p *int64) { atomic.AddInt64(p, 1) }

// ViaFuncVal calls the atomic through a variable holding it.
type ViaFuncVal struct { // want `reorder skipped: 64-bit atomic access at`
	n int64
	p *int
	b byte
}

var addf = atomic.AddInt64

func (s *ViaFuncVal) Inc() { addf(&s.n, 1) }

// Hdr is decoded through a forwarder taking any.
type Hdr struct { // want `reorder skipped: encoding/binary call at .*indirect.go:67:`
	A uint8
	B uint32
	C uint8
	D uint16
}

func ParseHdr(r io.Reader) (h Hdr, err error) { err = readLE(r, &h); return }

func readLE(r io.Reader, v any) error { return binary.Read(r, binary.LittleEndian, v) }

// Iface is decoded through an interface variable.
type Iface struct { // want `reorder skipped: encoding/binary call at`
	A uint8
	B uint32
	C uint8
	D uint16
}

func ParseIface(r io.Reader) (h Iface, err error) {
	var v any = &h
	err = binary.Read(r, binary.LittleEndian, v)
	return
}

// Gen is encoded through a generic forwarder.
type Gen struct { // want `reorder skipped: encoding/binary call at`
	A uint8
	B uint32
	C uint8
	D uint16
}

func enc[T any](v T) ([]byte, error) { return binary.Append(nil, binary.LittleEndian, v) }

func EncGen() ([]byte, error) { return enc(Gen{}) }

// Punned's bytes are read through a generic forwarder.
type Punned struct { // want `reorder skipped: unsafe.Pointer conversion at`
	A byte
	B *int
	C byte
}

func bytesOf[T any](p *T) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(p)), unsafe.Sizeof(*p))
}

func PunnedBytes(p *Punned) []byte { return bytesOf(p) }

// ReadOnly's forwarder only reads its parameter: no pin.
type ReadOnly struct { // want `struct with 16 pointer bytes could be 8$`
	n int64
	p *int
	b byte
}

func peek(v any) bool { return v != nil }

func Peek() bool { return peek(&ReadOnly{}) }

// Arith reads a field by pointer arithmetic from another field's address.
type Arith struct { // want `reorder skipped: unsafe.Pointer conversion at`
	A byte
	B *int
	C byte
}

func ArithC(p *Arith) *byte { return (*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(&p.A)) + 8)) }

// ArithAdd does the same through unsafe.Add.
type ArithAdd struct { // want `reorder skipped: unsafe.Pointer conversion at`
	A byte
	B *int
	C byte
}

func ArithAddC(p *ArithAdd) *byte { return (*byte)(unsafe.Add(unsafe.Pointer(&p.A), 8)) }

// FieldBytes views one field's own bytes, whatever its offset: reordered.
type FieldBytes struct { // want `struct of size 24 could be 16$`
	A byte
	B *int
	C uint32
}

func FieldBytesC(p *FieldBytes) *[4]byte { return (*[4]byte)(unsafe.Pointer(&p.C)) }

// R1, R2 and R3 are read by constant field index.
type R1 struct { // want `reorder skipped: reflect field index at`
	A byte
	B *int
	C byte
}

type R2 struct { // want `reorder skipped: reflect field index at`
	A byte
	B *int
	C byte
}

type R3 struct { // want `reorder skipped: reflect field index at`
	A byte
	B *int
	C byte
}

// ByName is read by field name, which survives a reorder: no pin.
type ByName struct { // want `struct of size 24 could be 16$`
	A byte
	B *int
	C byte
}

func Reflect(r2 *R2) []string {
	return []string{
		reflect.TypeOf(R1{}).Field(0).Name,
		reflect.ValueOf(r2).Elem().Field(1).Type().Name(),
		reflect.TypeFor[R3]().Field(2).Name,
		reflect.ValueOf(ByName{}).FieldByName("A").Type().Name(),
	}
}

// ViaElemPtr's words are reached through a pointer to an element of its
// array field (internal/bisect.dedup): recent's offset places them.
type ViaElemPtr struct { // want `reorder skipped: 64-bit atomic access at`
	recent [4][2]uint64
	mu     int32
	m      map[uint64]bool
}

func (d *ViaElemPtr) Seen(h uint64) bool {
	cache := &d.recent[h%4]
	return atomic.LoadUint64(&cache[0]) == h
}

// ViaRowPtr's word is two array levels below the pointer's target.
type ViaRowPtr struct { // want `reorder skipped: 64-bit atomic access at`
	b    byte
	grid [2][2][2]uint64
	p    *int
}

func (s *ViaRowPtr) Inc(i, j, k int) { row := &s.grid[i]; atomic.AddUint64(&row[j][k], 1) }

// ViaFieldPtr's word is a field of a struct field reached by pointer: t's
// offset places it (an inline struct, which holdsAtomic does not see).
type ViaFieldPtr struct { // want `reorder skipped: 64-bit atomic access at`
	b byte
	t struct{ n int64 }
	p *int
}

func (s *ViaFieldPtr) Inc() { t := &s.t; atomic.AddInt64(&t.n, 1) }

// ViaSlicePtr's pointer is into a slice's elements, their own allocation:
// no pin.
type ViaSlicePtr struct { // want `struct of size 40 could be 32$`
	b    byte
	rows [][2]uint64
	c    byte
}

func (s *ViaSlicePtr) Inc(i, j int) { row := &s.rows[i]; atomic.AddUint64(&row[j], 1) }
