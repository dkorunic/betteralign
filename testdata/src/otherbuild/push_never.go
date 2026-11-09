//go:build betteralign_never_set

package otherbuild

import _ "errors"

// Pushed into a package this file imports, which can't import this one back
// to name the type and declares a mirror: pinned although exported.
//
//go:linkname pushImported errors.pushImported
func pushImported(p *PushImported) int { return 0 }

// Pushed into a package neither the build nor this file imports, which may
// import this one and name the type itself: not pinned.
//
//go:linkname pushFree otherbuildx.pushFree
func pushFree(p *PushFree) int { return 0 }
