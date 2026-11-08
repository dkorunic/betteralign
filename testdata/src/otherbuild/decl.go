//go:build betteralign_never_set

package otherbuild

// table is declared in this excluded file only, so the package's scope
// doesn't know it.
type table []LocalDecl

var (
	localTab = table{{true, 1, false}}
	genTab   = GenList[GenElem]{{true, 2, false}}
)

var aliasTab = GenAlias[AliasElem]{{true, 3, false}}
