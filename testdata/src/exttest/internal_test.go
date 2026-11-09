package exttest

// An in-package test file makes the load add the test variant
// exttest [exttest.test], which must pin like the base pass.
var _ = K{A: 1}
