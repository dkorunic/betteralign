// Package ssh stubs what the layoutpin fixture uses of golang.org/x/crypto/ssh;
// only import paths and names matter to the analyzer.
package ssh

// Counter stands in for any package-level variable of an imported package.
var Counter int64

func Marshal(msg any) []byte { return nil }

func Unmarshal(data []byte, out any) error { return nil }
