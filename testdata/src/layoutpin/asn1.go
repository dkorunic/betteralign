package layoutpin

import "encoding/asn1"

// encoding/asn1 encodes a struct as a SEQUENCE in field order, and a slice of
// structs as SEQUENCE OF, so the call pins the argument and the element type.
type ASNMsg struct { // want `reorder skipped: encoding/asn1 call at .*asn1.go:20:`
	ok    bool
	n     int64
	more  bool
	items []ASNItem
}

type ASNItem struct { // want `reorder skipped: encoding/asn1 call at .*asn1.go:20:`
	ix bool
	iy int64
	iz bool
}

func asnEncode() { _, _ = asn1.Marshal(ASNMsg{}) } // @marshal

type ASNDecoded struct { // want `reorder skipped: encoding/asn1 call at .*asn1.go:28:`
	p bool
	q int64
	r bool
}

func asnDecode(b []byte) { _, _ = asn1.Unmarshal(b, &ASNDecoded{}) } // @unmarshal

// A struct that declares itself ASN.1 — an asn1 tag, or a field of an
// encoding/asn1 type — is left alone even with no encoder call in sight,
// since that call usually lives in another package. No diagnostic.
type ASNTagged struct {
	A bool `asn1:"optional"`
	B int64
	C bool
}

type ASNRawField struct {
	A bool
	B int64
	O asn1.ObjectIdentifier
	C bool
}

// Other tags are not ASN.1.
type JSONTagged struct { // want `struct of size 24 could be 16$`
	A bool  `json:"a"`
	B int64 `json:"b"`
	C bool  `json:"c"`
}

// An ASN.1 struct held by value, or as SEQUENCE OF, makes its holder part of
// the encoding: the holder reaches asn1.Unmarshal through an interface{} in
// another function or package (x/crypto/pkcs12), where no call pins it.
type ASNHolder struct {
	A  bool
	In ASNRawField
	B  bool
}

type ASNSeqHolder struct {
	A  bool
	In []ASNTagged
	B  bool
}

// Behind a pointer the inner struct is its own allocation: still reordered.
type ASNPtrHolder struct { // want `struct of size 24 could be 16$`
	A  bool
	In *ASNTagged
	B  bool
}

// An encoding/asn1 type as an array or slice element counts like a direct
// field of it, also behind a pointer. No diagnostic.
type ASNRawSlice struct {
	A  bool
	In []asn1.RawValue
	B  bool
}

type ASNOIDArray struct {
	A bool
	O [2]asn1.ObjectIdentifier
	B bool
}

type ASNRawPtrSlice struct {
	A  bool
	In []*asn1.RawValue
	B  bool
}
