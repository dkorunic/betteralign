package layoutpin

import (
	"encoding/binary"
	"io"
)

// heldElem reaches the wire only as an array element of heldRec's field:
// the array's bytes are part of heldRec's layout, so it is pinned too.
type heldElem struct { // want `reorder skipped: encoding/binary call at .*held.go`
	heldA bool
	heldB int32
	heldC bool
}

// heldTwice is held by two fields; pinning walks it once.
type heldTwice struct { // want `reorder skipped: encoding/binary call at .*held.go`
	heldD bool
	heldE int32
	heldF bool
}

type heldRec struct { // want `reorder skipped: encoding/binary call at .*held.go`
	heldG   bool
	heldArr [2]heldElem
	heldX   heldTwice
	heldY   heldTwice
	heldH   int32
	heldZ   bool
}

func heldWrite(w io.Writer) {
	_ = binary.Write(w, binary.LittleEndian, &heldRec{})
}
