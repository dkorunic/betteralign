package nested

// oneLine is alone in its file: a file the run rewrites is reprinted through
// gofmt, which would spread the struct over several lines for the next run.
type oneLine struct{ p *int; a bool; q *int } // want `struct with 24 pointer bytes could be 16`
