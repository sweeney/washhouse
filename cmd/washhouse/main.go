// Command washhouse will turn tanktemp's probe and meter messages into hot
// water state and draw events (see PLAN.md). Until phase 5 it only reports
// its version.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// version is stamped at build time with -ldflags "-X main.version=<commit>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("washhouse", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "washhouse %s\n", version)
		return 0
	}
	fmt.Fprintln(stderr, "washhouse: the service is not implemented yet (see PLAN.md)")
	return 2
}
