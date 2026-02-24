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

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}

		if !(strings.HasPrefix(d.Name(), "chunk_") && strings.HasSuffix(d.Name(), ".txt")) {
			return nil
		}

		base := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
		outputPath := filepath.Join(output, base+".json")
		if _, err := os.Stat(outputPath); err == nil {
			fmt.Printf("Ignore %q, already done\n", outputPath)

			return nil
		}

		briefs, err := BriefChunkWithCache(ctx, client, filepath.Join(dir, d.Name()))
		if err != nil {
			return fmt.Errorf("BriefChunkWithCache(%q): %w", d.Name(), err)
		}

		save, err := json.Marshal(briefs)
		if err != nil {
			os.WriteFile("failed.json", fmt.Appendf(nil, "%s", briefs), os.ModePerm)

			return fmt.Errorf("json.Marshal(): %w", err)
		}

		return os.WriteFile(outputPath, save, os.ModePerm)
	})

	if err != nil {
		return fmt.Errorf("filepath.WalkDir(%q): %w", dir, err)
	}

	return nil
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

	if len(chunkData) < 1024 {
		return BriefChunkWithoutCache(ctx, client, string(chunkData), filePaths)
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
		resp, err := requestAnalyseWithCache(ctx, client, path, cache)
		if err != nil {
			if strings.Contains(err.Error(), "429") {
				time.Sleep(10 * time.Second)
				resp, err = requestAnalyseWithCache(ctx, client, path, cache)
			} else {
				return nil, fmt.Errorf("model.GenerateContent(): %w", err)
			}
		}
		content, err := extractContentFromResponse(resp)
		if err != nil {
			return nil, fmt.Errorf("extractContentFromResponse(): %w", err)
		}

		briefs = append(briefs, content)

		fmt.Printf("Brief done for %q in %s (%d/%d)\n", path, time.Since(start), i+1, len(filePaths))
	}

	return briefs, nil
}

func requestAnalyseWithCache(ctx context.Context, client *genai.Client, path string, cache *genai.CachedContent) (*genai.GenerateContentResponse, error) {
	return client.Models.GenerateContent(
		ctx,
		model,
		genai.Text(fmt.Sprintf(prompt, path)),
		&genai.GenerateContentConfig{
			CachedContent:    cache.Name,
			ResponseMIMEType: "application/json",
			ResponseSchema:   analyseSchema(),
		},
	)
}

func BriefChunkWithoutCache(ctx context.Context, client *genai.Client, chunkData string, files []string) ([]Brief, error) {
	briefs := []Brief{}

	for i, path := range files {
		if i > 10 {
			break
		}

		start := time.Now()
		resp, err := requestAnalyseWithoutCache(ctx, client, path, chunkData)
		if err != nil {
			if strings.Contains(err.Error(), "429") {
				time.Sleep(10 * time.Second)
				resp, err = requestAnalyseWithoutCache(ctx, client, path, chunkData)
			} else {
				return nil, fmt.Errorf("model.GenerateContent(): %w", err)
			}
		}
		content, err := extractContentFromResponse(resp)
		if err != nil {
			return nil, fmt.Errorf("extractContentFromResponse(): %w", err)
		}

		briefs = append(briefs, content)

		fmt.Printf("Brief done for %q in %s (%d/%d)\n", path, time.Since(start), i+1, len(files))
	}

	return briefs, nil
}

func requestAnalyseWithoutCache(ctx context.Context, client *genai.Client, path string, chunkData string) (*genai.GenerateContentResponse, error) {
	return client.Models.GenerateContent(
		ctx,
		model,
		genai.Text(fmt.Sprintf("File: %s\n", chunkData)+fmt.Sprintf(prompt, path)),
		&genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
			ResponseSchema:   analyseSchema(),
		},
	)
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
						"name":        {Type: genai.TypeString},
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
