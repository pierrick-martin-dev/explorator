package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"
)

const model = "google/gemini-3-flash-preview"

const prompt = `
Task: Analyze the provided C++ source file.
Output MUST be a PURE JSON ARRAY. No markdown, no preamble.

STRICT RULES:
1. NAME FIELD: Identifier ONLY (e.g., "CMainFrame"). If you add ANY explanation, line numbers, or meta-talk in this field, the output is considered a failure.
2. NO REPETITION: Do not repeat phrases. If you find yourself stuck in a loop, terminate the JSON immediately.
3. CHARACTER LIMIT: 'name' fields > 256 chars will be truncated and rejected.
4. SCHEMA ADHERENCE: Use null for missing values. Do not invent keys.
5. BRAIN-DEAD MODE: Do not philosophize about the code. Just extract the structural metadata.

This is proprietary internal code for architectural analysis only. Summarize the structure without quoting large blocks of code verbatim.

File to analyze: %s
`

type Brief = json.RawMessage

type Chunk struct {
	content []byte
	files   []string
	name    string
	temp    float32
	cache   *genai.CachedContent
}

type Briefer = func(ctx context.Context, client *genai.Client, chunk *Chunk, filepath string) (Brief, error)

func GenerateBriefsFromChunks(ctx context.Context, client *genai.Client, dir string, output string) error {
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠️ Error accessing path %q: %v\n", path, err)
			return nil
		}

		if d.IsDir() {
			return nil
		}

		if !(strings.HasPrefix(d.Name(), "chunk_") && strings.HasSuffix(d.Name(), ".txt")) {
			return nil
		}

		start := time.Now()
		err = BriefChunk(ctx, client, path, output)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ FAILED: BriefChunkWithCache(%q): %v\n", d.Name(), err)

			return nil
		}

		fmt.Printf("✅ Processed %q in %s\n", d.Name(), time.Since(start))

		return nil
	})

	if err != nil {
		return fmt.Errorf("filepath.WalkDir(%q): %w", dir, err)
	}

	return nil
}

func BriefChunk(ctx context.Context, client *genai.Client, chunkPath string, outputDir string) error {
	chunkData, err := os.ReadFile(chunkPath)
	if err != nil {
		return fmt.Errorf("os.ReadFile(%q): %w", chunkPath, err)
	}

	filePaths, err := extractFilePathsFromChunk(chunkData)
	if err != nil {
		return fmt.Errorf("extractFilePathsFromChunk(): %w", err)
	}

	remainingFiles := []string{}

	for _, f := range filePaths {
		outputPath := filepath.Join(outputDir, f+".json")

		if _, err := os.Stat(outputPath); err == nil {
			continue
		}

		remainingFiles = append(remainingFiles, f)
	}

	if len(remainingFiles) == 0 {
		return nil
	}

	fmt.Printf("%d remaining files to brief for %q\n", len(remainingFiles), chunkPath)

	chunk := Chunk{
		name:    chunkPath,
		content: chunkData,
		files:   remainingFiles,
		temp:    0,
	}
	briefer := Briefer(BriefChunkWithoutCache)

	if len(chunkData) > 2048 {
		cache, err := client.Caches.Create(ctx, model, &genai.CreateCachedContentConfig{
			Contents: []*genai.Content{
				{Role: "user", Parts: []*genai.Part{{Text: string(chunk.content)}}},
			},
			TTL: 3600 * time.Second,
		})
		if err != nil {
			if !strings.Contains(err.Error(), "The minimum token count to start caching is 1024.") {
				return fmt.Errorf("client.Caches.Create(): %w", err)
			}

			// safely ignore this error and process the chunk without cache
		} else {

			chunk.cache = cache
			briefer = BriefChunkWithCache
			defer cleanupChunk(ctx, client, &chunk)
		}
	}

	for _, f := range chunk.files {
		outputPath := filepath.Join(outputDir, f+".json")

		brief, err := retryBrief(ctx, client, briefer, chunk, f)
		if err != nil {
			log.Printf("\t\tcannot brief file %q too many retries. Error: %s", f, err)
			continue
		}

		if err := saveBriefOnDisk(brief, outputPath); err != nil {
			return fmt.Errorf("saveBriefOnDisk(): %w", err)
		}

		fmt.Printf("\t\t✔️ %q written on disk\n", outputPath)
	}

	return nil
}

func saveBriefOnDisk(brief Brief, outputPath string) error {
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return fmt.Errorf("os.MkdirAll(%q): %w", dir, err)
	}

	save, err := json.MarshalIndent(brief, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ FAILED: json.Marshal(%q): %v\n", outputPath, err)
		_ = os.WriteFile(filepath.Join(outputPath+".error.log"), fmt.Appendf(nil, "%s", brief), 0644)
		return nil
	}

	if err := os.WriteFile(outputPath, save, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "❌ FAILED: os.WriteFile(%q): %v\n", outputPath, err)
		return nil
	}

	return nil
}

const maxRetries = 3

var (
	ErrRateLimited    = fmt.Errorf("reached rate limit")
	ErrContextExpired = fmt.Errorf("context expired. Too long to respond")
)

func retryBrief(ctx context.Context, client *genai.Client, briefer Briefer, chunk Chunk, path string) (Brief, error) {
	var lastError error

	for range maxRetries {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)

		brief, err := briefer(ctx, client, &chunk, path)
		cancel()

		if err == nil {
			return brief, nil
		}

		if strings.Contains(err.Error(), "429") {
			time.Sleep(10 * time.Second)
			lastError = ErrRateLimited
		}

		if errors.Is(err, context.DeadlineExceeded) {
			chunk.temp += 0.3
			lastError = ErrContextExpired
		}

		lastError = err
	}

	return nil, fmt.Errorf("maximum retries reached, last error %w", lastError)
}

func cleanupChunk(ctx context.Context, client *genai.Client, chunk *Chunk) error {
	if chunk.cache != nil {
		_, err := client.Caches.Delete(ctx, chunk.cache.Name, nil)
		if err != nil {
			return fmt.Errorf("client.Caches.Delete(%q): %w", chunk.cache.Name, err)
		}
	}

	return nil
}

func BriefChunkWithCache(ctx context.Context, client *genai.Client, chunk *Chunk, path string) (Brief, error) {
	resp, err := requestAnalyseWithCache(ctx, client, path, chunk.cache, chunk.temp)
	if err != nil {
		return nil, fmt.Errorf("model.GenerateContent(): %w", err)
	}
	content, err := extractContentFromResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("extractContentFromResponse(): %w", err)
	}

	return content, nil
}

func requestAnalyseWithCache(ctx context.Context, client *genai.Client, path string, cache *genai.CachedContent, temp float32) (*genai.GenerateContentResponse, error) {
	return client.Models.GenerateContent(
		ctx,
		model,
		genai.Text(fmt.Sprintf(prompt, path)),
		&genai.GenerateContentConfig{
			CachedContent:    cache.Name,
			ResponseMIMEType: "application/json",
			ResponseSchema:   analyseSchema(),
			SafetySettings:   safetySettings(),
			Temperature:      &temp,
		},
	)
}

func BriefChunkWithoutCache(ctx context.Context, client *genai.Client, chunk *Chunk, path string) (Brief, error) {
	resp, err := requestAnalyseWithoutCache(ctx, client, path, string(chunk.content), chunk.temp)
	if err != nil {
		return nil, fmt.Errorf("model.GenerateContent(): %w", err)
	}
	content, err := extractContentFromResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("extractContentFromResponse(): %w", err)
	}

	return content, nil
}

func requestAnalyseWithoutCache(ctx context.Context, client *genai.Client, path string, chunkData string, temp float32) (*genai.GenerateContentResponse, error) {
	return client.Models.GenerateContent(
		ctx,
		model,
		genai.Text(fmt.Sprintf("File: %s\n", chunkData)+fmt.Sprintf(prompt, path)),
		&genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
			ResponseSchema:   analyseSchema(),
			SafetySettings:   safetySettings(),
			Temperature:      &temp,
		},
	)
}

func safetySettings() []*genai.SafetySetting {
	return []*genai.SafetySetting{
		{Category: genai.HarmCategoryHateSpeech, Threshold: genai.HarmBlockThresholdOff},
		{Category: genai.HarmCategoryDangerousContent, Threshold: genai.HarmBlockThresholdOff},
		{Category: genai.HarmCategoryHarassment, Threshold: genai.HarmBlockThresholdOff},
		{Category: genai.HarmCategorySexuallyExplicit, Threshold: genai.HarmBlockThresholdOff},
		{Category: genai.HarmCategoryJailbreak, Threshold: genai.HarmBlockThresholdOff},
		{Category: genai.HarmCategoryUnspecified, Threshold: genai.HarmBlockThresholdOff},
	}
}

func analyseSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"file_path": {Type: genai.TypeString},
			"type":      {Type: genai.TypeString, Enum: []string{"header", "implementation", "resource", "other"}},
			"summary":   {Type: genai.TypeString},
			"entities": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"name":        {Type: genai.TypeString, Description: "The exact identifier from the code. NO EXPLANATIONS."},
						"kind":        {Type: genai.TypeString, Enum: []string{"class", "struct", "function"}},
						"description": {Type: genai.TypeString},
					},
				},
			},
			"dependencies": {
				Type:  genai.TypeArray,
				Items: &genai.Schema{Type: genai.TypeString},
			},
			"legacy_markers": {
				Type: genai.TypeObject,
				Properties: map[string]*genai.Schema{
					"uses_open_utm": {Type: genai.TypeBoolean},
					"uses_psdb":     {Type: genai.TypeBoolean},
					"frameworks": {
						Type:  genai.TypeArray,
						Items: &genai.Schema{Type: genai.TypeString},
					},
				},
			},
		},
		Required: []string{"file_path", "type", "summary", "entities", "legacy_markers"},
	}
}

var (
	ErrNoCandidate         = fmt.Errorf("no candidate")
	ErrNoPartsForCandidate = fmt.Errorf("no parts for first candidate")
	ErrNoContent           = fmt.Errorf("no content for first candidate")
	ErrNilPartForCandidate = fmt.Errorf("first part is nil for first candidate")
)

func extractContentFromResponse(resp *genai.GenerateContentResponse) ([]byte, error) {
	if len(resp.Candidates) == 0 {
		return nil, ErrNoCandidate
	}

	if resp.Candidates[0].Content == nil {
		return nil, ErrNoContent
	}

	if len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, ErrNoPartsForCandidate
	}

	if resp.Candidates[0].Content.Parts[0] == nil {
		return nil, ErrNilPartForCandidate
	}

	return []byte(resp.Candidates[0].Content.Parts[0].Text), nil
}

func extractFilePathsFromChunk(chunk []byte) ([]string, error) {
	buf := bytes.NewBuffer(chunk)

	_, err := buf.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("buf.ReadString(\\n): %w", err)
	}

	paths := []string{}
	for {
		line, err := buf.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("buf.ReadString(\\n): %w", err)
		}

		if strings.HasPrefix(line, "- ") {
			paths = append(paths, strings.TrimSpace(line[2:]))
		} else {
			break
		}
	}

	return paths, nil
}
