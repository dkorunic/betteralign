package grouped

// Tests per-spec opt-in comments inside a grouped type declaration.
// A betteralign:check comment on an individual TypeSpec should opt in
// only that struct, not every struct in the group.

type (
	// betteralign:check
	OptedIn struct { // want "struct of size 12 could be 8"
		x byte
		y int32
		z byte
	}
	NotOptedIn struct {
		x byte
		y int32
		z byte
	}
)
