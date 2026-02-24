package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"google.golang.org/genai"
)

func main() {
	command := os.Args[1]

	ctx := context.Background()

	switch strings.ToLower(command) {
	case "chunker":
		fs := flag.NewFlagSet("chunker", flag.ExitOnError)

		srcDir := fs.String("src", "./", "Source directory")
		outDir := fs.String("out", "./staging", "Output directory")
		limit := fs.Uint("limit", 1500000, "Max size in bytes")

		fs.Parse(os.Args[2:])

		fmt.Printf("Chunking %q into %q with %d limit\n", *srcDir, *outDir, *limit)

		chunker := NewChunker([]string{
			// from `tokei -l`
			".cc", ".cpp", ".cxx", ".c++", ".pcc", ".tpp", // c++
			".hh", ".hpp", ".hxx", ".inl", ".ipp", // c++ header
			".cppm", ".ixx", ".ccm", ".mpp", ".mxx", ".cxxm", ".hppm", ".hxxm", // c++ module
			".c", ".ec", ".pgc", // c
			".h",    // c header
			".csh",  // c shell
			".java", // java
			".cat",  // Rational Rose
		}, *limit)
		if err := chunker.Chunk(*srcDir, *outDir); err != nil {
			log.Fatalf("chunker.Run(): %s", err)
		}

	case "briefer":
		projectID := getEnvStrOr("PROJECT_ID", "my-project")
		location := getEnvStrOr("LOCATION", "global")

		fs := flag.NewFlagSet("briefer", flag.ExitOnError)

		dir := fs.String("dir", "./staging", "Chunks directory")
		output := fs.String("output", "database.json", "Output file")

		fs.Parse(os.Args[2:])

		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			Project:  projectID,
			Location: location,
			Backend:  genai.BackendVertexAI,
		})
		if err != nil {
			log.Fatalf("genai.NewClient(): %s", err)
		}

		if err := GenerateBriefsFromChunks(ctx, client, *dir, *output); err != nil {
			log.Fatalf("GenerateBriefsFromChunks(%s): %s", *dir, err)
		}

	default:
		log.Fatalf("unknown command: %q\n", command)

	}

}

func getEnvStrOr(name string, fallback string) string {
	envvar := os.Getenv(name)
	if envvar != "" {
		return envvar
	}

	return fallback
}
