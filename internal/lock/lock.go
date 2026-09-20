// Package lock serializes applied Swarmfolio runs without storing decisions.
package lock

import "os"

type Lock struct {
	file *os.File
}
