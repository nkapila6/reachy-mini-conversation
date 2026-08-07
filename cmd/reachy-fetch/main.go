// reachy-fetch fetches live web data from context.dev and prints it.
// Useful for piping web context into an LLM prompt or for debugging.
//
// Usage:
//
//	reachy-fetch -url https://example.com
//	reachy-fetch -search "latest AI news"
//	reachy-fetch -url https://example.com -max 2000
//
// Build:
//
//	CGO_ENABLED=0 go build -o reachy-fetch ./cmd/reachy-fetch
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/nkapila6/reachy-utils/pkg/context"
	"github.com/nkapila6/reachy-utils/pkg/env"
)

func main() {
	env.Load()

	url := flag.String("url", "", "URL to fetch content from")
	search := flag.String("search", "", "search query for web results")
	maxChars := flag.Int("max", 0, "max characters in output (0 = no limit)")
	flag.Parse()

	if *url == "" && *search == "" {
		fmt.Fprintln(os.Stderr, "either -url or -search is required")
		os.Exit(2)
	}

	client, err := context.New("")
	if err != nil {
		log.Fatalf("context client: %v", err)
	}

	if *url != "" {
		result, err := client.FetchAndSummarize(*url, *maxChars)
		if err != nil {
			log.Fatalf("fetch: %v", err)
		}
		fmt.Println(result)
		return
	}

	if *search != "" {
		result, err := client.SearchAndSummarize(*search, 5)
		if err != nil {
			log.Fatalf("search: %v", err)
		}
		fmt.Println(result)
		return
	}
}
