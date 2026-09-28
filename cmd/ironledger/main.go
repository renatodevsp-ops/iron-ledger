// Command ironledger runs the distributed betting ledger API.
//
// It is a single binary that serves the REST API described in
// specs/001-distributed-betting-ledger/contracts/openapi.yaml. The SQS consumer
// runs from the same binary once the FIFO channel is wired; until then this
// process is API-only and says so at start-up.
package main

import (
	"fmt"
	"os"

	"github.com/ironledger/ironledger/internal/platform/fxapp"
)

// version is set at build time with -ldflags "-X main.version=...". It is
// logged at start-up so a running pod can always be matched to a commit; a
// process whose version is unknown cannot be reasoned about after an incident.
var version = "dev"

func main() {
	if err := fxapp.Run(version); err != nil {
		// The failure is reported on stderr as well as through the logger,
		// because a start-up error may be the only thing this process ever
		// produces and a supervisor may only capture the exit path.
		fmt.Fprintf(os.Stderr, "ironledger: %v\n", err)
		os.Exit(1)
	}
}
