package layoutpin

import (
	"sync/atomic"

	"golang.org/x/crypto/ssh"
)

// A package-level variable sits in no struct, so there is nothing to pin.
func atomicPackageVar() { atomic.AddInt64(&ssh.Counter, 1) }
