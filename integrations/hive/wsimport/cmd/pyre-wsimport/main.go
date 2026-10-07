// Command pyre-wsimport inventories a copy of a legacy ws mail tree and
// prints the deterministic Pyre import plan (dry run). Sending (--apply)
// requires a hub client and runs through `pyre migrate ws-mail --apply`,
// which calls wsimport.Main with its authenticated MessageAPI.
package main

import (
	"context"
	"os"

	"github.com/jhh3/pyre/integrations/hive/wsimport"
)

func main() {
	os.Exit(wsimport.Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr, os.Getenv, nil))
}
