//go:build race

package redact

// raceDetector reports whether the tests run with the race detector, which
// makes reading text many times slower.
const raceDetector = true
