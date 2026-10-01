// Command syncdist replaces the embedded web build with a fresh Vite build.
//
// Usage: go run ./syncdist <vite-dist> <embed-dist>
package main

import (
	"log"
	"os"

	"github.com/0xPiranhaCodes/webpty/internal/webassets"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: syncdist <vite-dist> <embed-dist>")
	}
	if err := webassets.Sync(os.Args[1], os.Args[2]); err != nil {
		log.Fatal(err)
	}
}
