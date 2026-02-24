package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config holds the application settings
type Config struct {
	SrcDir        string
	OutDir        string
	MaxChunkSize  int
	StripComments bool
}

// Processor handles the state of the current ingestion run
type Processor struct {
	cfg          *Config
	chunkID      int
	currentFiles []FileData
	currentSize  int
	singleLineRE *regexp.Regexp
	multiLineRE  *regexp.Regexp
}

type FileData struct {
	Path    string
	Content string
}

func NewProcessor(cfg *Config) *Processor {
	return &Processor{
		cfg:          cfg,
		chunkID:      1,
		singleLineRE: regexp.MustCompile(`//.*`),
		multiLineRE:  regexp.MustCompile(`(?s)/\*.*?\*/`),
	}
}

func (p *Processor) stripCode(content string) string {
	if !p.cfg.StripComments {
		return content
	}
	content = p.multiLineRE.ReplaceAllString(content, "")
	content = p.singleLineRE.ReplaceAllString(content, "")

	lines := strings.Split(content, "\n")
	var cleanLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			cleanLines = append(cleanLines, trimmed)
		}
	}
	return strings.Join(cleanLines, "\n")
}

func (p *Processor) writeChunk() error {
	fileName := filepath.Join(p.cfg.OutDir, fmt.Sprintf("chunk_%d.txt", p.chunkID))
	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer f.Close()

	writer := bufio.NewWriter(f)

	writer.WriteString("### PROJECT HIERARCHY (CHUNK CONTEXT) ###\n")
	for _, file := range p.currentFiles {
		writer.WriteString(fmt.Sprintf("- %s\n", file.Path))
	}
	writer.WriteString("\n" + strings.Repeat("=", 64) + "\n\n")

	for _, file := range p.currentFiles {
		writer.WriteString(fmt.Sprintf("--- SOURCE: %s ---\n", file.Path))
		writer.WriteString(file.Content)
		writer.WriteString("\n\n")
	}

	return writer.Flush()
}

func (p *Processor) Run(_ context.Context) error {
	fmt.Printf("🚀 Starting Auto-Ingest\nSource: %s\nLimit: %d bytes\n\n", p.cfg.SrcDir, p.cfg.MaxChunkSize)

	if err := os.MkdirAll(p.cfg.OutDir, 0755); err != nil {
		return fmt.Errorf("could not create output directory: %w", err)
	}

	err := filepath.Walk(p.cfg.SrcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		ext := strings.ToLower(filepath.Ext(path))
		if info.IsDir() || !(ext == ".cpp" || ext == ".cc" || ext == ".h" || ext == ".pc" || ext == ".ec" || ext == ".cat") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		processed := p.stripCode(string(data))
		entrySize := len(processed) + len(path) + 100

		if p.currentSize+entrySize > p.cfg.MaxChunkSize && len(p.currentFiles) > 0 {
			fmt.Printf("📦 Chunk %d...\n", p.chunkID)
			if err := p.writeChunk(); err != nil {
				return err
			}
			p.chunkID++
			p.currentFiles = nil
			p.currentSize = 0
		}

		p.currentFiles = append(p.currentFiles, FileData{Path: path, Content: processed})
		p.currentSize += entrySize
		return nil
	})

	if err != nil {
		return err
	}

	if len(p.currentFiles) > 0 {
		return p.writeChunk()
	}

	return nil
}
