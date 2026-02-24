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
		cfg := &Config{}

		flag.StringVar(&cfg.SrcDir, "src", "./", "Source directory of the codebase")
		flag.StringVar(&cfg.OutDir, "out", "./stark_chunks", "Output directory for chunks")
		flag.IntVar(&cfg.MaxChunkSize, "limit", 1500000, "Max size of each chunk in bytes (~500k tokens)")
		flag.BoolVar(&cfg.StripComments, "strip", true, "Whether to strip C-style comments")
		flag.Parse()

		proc := NewProcessor(cfg)
		if err := proc.Run(ctx); err != nil {
			log.Fatalf("proc.Run(): %s", err)
		}
		fmt.Printf("\n✅ Ingestion complete. Chunks generated in %s\n", cfg.OutDir)

	case "briefer":
		projectID := getEnvStrOr("PROJECT_ID", "my-project")
		location := getEnvStrOr("LOCATION", "global")
		dir := ""
		output := ""
		flag.StringVar(&dir, "dir", "./stark_chunks", "Chunks directory")
		flag.StringVar(&output, "output", "database.json", "Where briefs are written")
		flag.Parse()

		client, err := genai.NewClient(ctx, &genai.ClientConfig{
			Project:  projectID,
			Location: location,
			Backend:  genai.BackendVertexAI,
		})
		if err != nil {
			log.Fatalf("genai.NewClient(): %s", err)
		}

		if err := GenerateBriefsFromChunks(ctx, client, dir, output); err != nil {
			log.Fatalf("GenerateBriefsFromChunks(%q): %s", dir, err)
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
