package user

import "xalias/dep"

// dep.N reorders in its own pass, so U must follow it.
type U struct { // want `bytes saved: struct of size \d+ could be \d+$`
	A byte
	P *int
	B byte
}

func conv(u U) dep.N { return dep.N(u) }

func assign(u U) dep.N { return u }
