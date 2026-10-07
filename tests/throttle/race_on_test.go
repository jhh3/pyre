//go:build race

package throttle

// raceEnabled: built with -race, which slows scheduling and synchronisation
// several-fold (CI runs every package this way, concurrently).
const raceEnabled = true
