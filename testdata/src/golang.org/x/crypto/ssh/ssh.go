// Package ssh stubs the two golang.org/x/crypto/ssh functions the layoutpin
// fixture calls; only their import path and names matter to the analyzer.
package ssh

func Marshal(msg any) []byte { return nil }

func Unmarshal(data []byte, out any) error { return nil }
