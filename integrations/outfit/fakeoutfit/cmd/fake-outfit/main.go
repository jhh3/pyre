// Command fake-outfit is the synthetic Outfit CLI for integration tests and
// demos. Set FAKE_OUTFIT_STATE to a fakeoutfit.State JSON file.
package main

import (
	"os"

	"github.com/jhh3/pyre/integrations/outfit/fakeoutfit"
)

func main() {
	os.Exit(fakeoutfit.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}
