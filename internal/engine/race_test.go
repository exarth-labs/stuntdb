//go:build race

package engine

// raceEnabled shrinks the slowest tests: the race detector makes them
// many times slower.
const raceEnabled = true
