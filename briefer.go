package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"
)

const model = "google/gemini-3-flash-preview"

const prompt = `
Task: Analyze the provided file.
Return a JSON object following this EXACT structure:
{
"file_path": "string",
"type": "header|implementation|resource",
"summary": "2-sentence high-level purpose",
"entities": [
{"name": "string", "kind": "class|struct|function", "description": "string"}
],
"dependencies": ["string"],
"legacy_markers": {
"uses_open_utm": bool,
"uses_psdb": bool,
"frameworks": ["MFC", "Objective Grid", "etc"]
}
}
Rule: If a field is not applicable, return an empty array or null. Do not add extra keys.
File to analyze: %s
`

type Brief = json.RawMessage

func GenerateBriefsFromChunks(ctx context.Context, client *genai.Client, dir string, output string) error {
	briefs := []Brief{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}

		if d.Name() != "chunk_1.txt" {
			return nil
		}

		if !(strings.HasPrefix(d.Name(), "chunk_") && strings.HasSuffix(d.Name(), ".txt")) {
			return nil
		}

		bs, err := BriefChunkWithCache(ctx, client, filepath.Join(dir, d.Name()))
		if err != nil {
			return fmt.Errorf("BriefChunkWithCache(%q): %w", d.Name(), err)
		}

		briefs = append(briefs, bs...)

		return nil
	})

	if err != nil {
		return fmt.Errorf("filepath.WalkDir(%q): %w", dir, err)
	}

	save, err := json.Marshal(briefs)
	if err != nil {
		os.WriteFile("failed.json", []byte(fmt.Sprintf("%s", briefs)), os.ModePerm)

		return fmt.Errorf("json.Marshal(): %w", err)
	}

	return os.WriteFile(output, save, os.ModePerm)
}

func BriefChunkWithCache(ctx context.Context, client *genai.Client, chunkPath string) ([]Brief, error) {
	briefs := []Brief{}
	chunkData, err := os.ReadFile(chunkPath)
	if err != nil {
		return nil, fmt.Errorf("os.ReadFile(%q): %w", chunkPath, err)
	}

	filePaths, err := extractFilePathsFromChunk(chunkData)
	if err != nil {
		return nil, fmt.Errorf("extractFilePathsFromChunk(): %w", err)
	}

	cache, err := client.Caches.Create(ctx, model, &genai.CreateCachedContentConfig{
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{{Text: string(chunkData)}}},
		},
		TTL: 3600 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("client.Caches.Create(): %w", err)
	}

	for i, path := range filePaths {
		if i > 10 {
			break
		}

		start := time.Now()
		resp, err := client.Models.GenerateContent(
			ctx,
			model,
			genai.Text(fmt.Sprintf(prompt, path)),
			&genai.GenerateContentConfig{CachedContent: cache.Name, ResponseMIMEType: "application/json"},
		)
		if err != nil {
			if strings.Contains(err.Error(), "429") {
				time.Sleep(10 * time.Second)
				resp, err = client.Models.GenerateContent(ctx, model, genai.Text(fmt.Sprintf(
					"Based on the cached code, give me a JSON summary for ONLY the file: %s, object MUST be { summary: string}", path,
				)), &genai.GenerateContentConfig{CachedContent: cache.Name})
			} else {
				return nil, fmt.Errorf("model.GenerateContent(): %w", err)
			}
		}

		briefs = append(briefs, []byte(resp.Candidates[0].Content.Parts[0].Text))

		fmt.Printf("Brief done for %q in %s (%d/%d)\n", path, time.Since(start), i+1, len(filePaths))
	}

	return briefs, nil
}

func extractContentFromResponse(resp *genai.GenerateContentResponse) (string, error) {
	return "", nil
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
