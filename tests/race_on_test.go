//go:build race

package tests

// raceEnabled reports whether the test binary was built with -race, which
// slows the code down several times and makes timings meaningless.
const raceEnabled = true
