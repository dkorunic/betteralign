package layoutpin

import (
	"encoding/asn1"
	"encoding/binary"
	"io"
)

// A defined type can be its own element; the codec walk must still stop.
type (
	cycleSlice []cycleSlice
	cyclePtr   *cyclePtr
)

type cycleHolder struct {
	S cycleSlice
}

type cycleMisaligned struct { // want `struct of size 12 could be 8`
	cycleA bool
	cycleB int32
	cycleC bool
}

func cycles(w io.Writer) {
	_ = binary.Size(cycleSlice(nil))
	var p cyclePtr
	_ = binary.Write(w, binary.LittleEndian, p)
	_, _ = asn1.Marshal(cycleHolder{})
}
