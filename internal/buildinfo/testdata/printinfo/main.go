// Command printinfo prints the build metadata linked into it.
package main

import (
	"fmt"

	"github.com/0xPiranhaCodes/webpty/internal/buildinfo"
)

func main() { i := buildinfo.Get(); fmt.Print(i.Version, " ", i.Commit, " ", i.Date) }
