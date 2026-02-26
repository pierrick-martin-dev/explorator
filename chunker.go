package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// WorkUnit represents a collection of file paths to be bundled into one chunk.
type WorkUnit []string

type Chunker struct {
	limitChunk uint
}

func NewChunker(limitChunk uint) *Chunker {
	return &Chunker{
		limitChunk: limitChunk,
	}
}

// Node represents a directory or file metadata
type Node struct {
	Name     string
	Path     string
	IsDir    bool
	Size     int64
	Children []Node
}

func (c *Chunker) Chunk(src string, dest string) error {
	tree, err := c.harvestStatTree(src)
	if err != nil {
		return fmt.Errorf("harvest failed: %w", err)
	}

	works, err := c.partitionWork(*tree)
	if err != nil {
		return fmt.Errorf("partition failed: %w", err)
	}

	if err := os.MkdirAll(dest, fs.ModePerm); err != nil {
		return err
	}

	if err := c.Validate(src, works); err != nil {
		return fmt.Errorf("c.Validate(): %w", err)
	}

	for i, w := range works {
		if err := c.processChunkWork(w, src, dest, i+1); err != nil {
			return fmt.Errorf("processing failed for chunk %d: %w", i, err)
		}
	}

	fmt.Printf("📦 Processed %d chunks\n", len(works))

	return nil
}

func (c *Chunker) harvestStatTree(path string) (*Node, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	node := &Node{
		Name:  info.Name(),
		Path:  path,
		IsDir: info.IsDir(),
	}

	if !info.IsDir() {
		base := filepath.Base(path)
		if shouldKeepFile(base) {
			node.Size = info.Size()
			return node, nil
		}

		return nil, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		child, err := c.harvestStatTree(filepath.Join(path, entry.Name()))
		if err != nil {
			return nil, err
		}

		if child == nil {
			continue
		}

		node.Children = append(node.Children, *child)
		node.Size += child.Size
	}

	return node, nil
}

func shouldKeepFile(base string) bool {
	return isCppFile(base) || isMakefile(base) || isRationalRose(base) || isXml(base) || isJava(base) || isShell(base)
}

func (c *Chunker) partitionWork(tree Node) ([]WorkUnit, error) {
	var works []WorkUnit

	if uint(tree.Size) <= c.limitChunk {
		files := c.collectFiles(tree)

		if len(files) > 0 {
			return []WorkUnit{files}, nil
		}
		return nil, nil
	}

	var localFiles []Node
	for _, child := range tree.Children {
		if child.IsDir {
			childWorks, _ := c.partitionWork(child)

			if len(childWorks) > 0 {
				works = append(works, childWorks...)
			}
		} else {

			if child.Path != "" {
				localFiles = append(localFiles, child)
			}
		}
	}

	if len(localFiles) > 0 {
		residuals := c.splitLinearly(localFiles)
		for _, r := range residuals {
			if len(r) > 0 {
				works = append(works, r)
			}
		}
	}

	return works, nil
}

func (c *Chunker) collectFiles(n Node) WorkUnit {
	if !n.IsDir {
		return WorkUnit{n.Path}
	}
	var res WorkUnit
	for _, child := range n.Children {
		res = append(res, c.collectFiles(child)...)
	}
	return res
}

func (c *Chunker) splitLinearly(nodes []Node) []WorkUnit {
	var results []WorkUnit
	var current WorkUnit
	var currentSize uint

	for _, n := range nodes {
		size := uint(n.Size)

		if currentSize+size > c.limitChunk && len(current) > 0 {
			results = append(results, current)
			current = WorkUnit{}
			currentSize = 0
		}
		current = append(current, n.Path)
		currentSize += size
	}
	if len(current) > 0 {
		results = append(results, current)
	}
	return results
}

func (c *Chunker) Validate(src string, works []WorkUnit) error {
	expectedFiles := make(map[string]bool)
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if shouldKeepFile(filepath.Base(path)) {
			expectedFiles[path] = true
		}
		return nil
	})
	if err != nil {
		return err
	}

	actualFiles := make(map[string]int)
	for _, unit := range works {
		for _, path := range unit {
			actualFiles[path]++
		}
	}

	fmt.Printf("\n--- 🛡️ Validation Report ---\n")
	fmt.Printf("Files on disk (matching exts): %d\n", len(expectedFiles))
	fmt.Printf("Files assigned to chunks:    %d\n", len(actualFiles))

	missing := 0
	for path := range expectedFiles {
		if actualFiles[path] == 0 {
			fmt.Printf("❌ MISSING: %s\n", path)
			missing++
		}
	}

	duplicates := 0
	for path, count := range actualFiles {
		if count > 1 {
			fmt.Printf("⚠️ DUPLICATE (%d times): %s\n", count, path)
			duplicates++
		}
	}

	if missing == 0 && duplicates == 0 {
		fmt.Println("✅ 100% integrity: Every file is accounted for exactly once.")
		return nil
	}

	return fmt.Errorf("integrity check failed: %d missing, %d duplicates", missing, duplicates)
}

func (c *Chunker) processChunkWork(work WorkUnit, srcDir, destDir string, chunkID int) error {
	if len(work) == 0 {
		return nil
	}

	chunkPath := filepath.Join(destDir, fmt.Sprintf("chunk_%d.txt", chunkID))

	var chunkContent strings.Builder

	chunkContent.WriteString("<directory_structure>\n")
	for _, p := range work {
		cleanedPath, err := filepath.Rel(srcDir, p)
		if err != nil {
			return fmt.Errorf("filepath.Rel(%q): %w", p, err)
		}

		fmt.Fprintf(&chunkContent, "- %s\n", cleanedPath)
	}
	chunkContent.WriteString("</directory_structure>\n\n")

	chunkContent.WriteString("<files>\n")
	for _, p := range work {
		content, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %q: %w", p, err)
		}

		cleaned := cleanContent(string(content))
		cleanedPath, err := filepath.Rel(srcDir, p)
		if err != nil {
			return fmt.Errorf("filepath.Rel(%q): %w", p, err)
		}

		chunkContent.WriteString(fmt.Sprintf("<file path=%q>\n", cleanedPath))
		chunkContent.WriteString(cleaned)
		chunkContent.WriteString("\n</file>\n\n")
	}
	chunkContent.WriteString("</files>")

	return os.WriteFile(chunkPath, []byte(chunkContent.String()), 0644)
}

var (
	singleLineRE = regexp.MustCompile(`//.*`)
	multiLineRE  = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

func cleanContent(content string) string {
	content = multiLineRE.ReplaceAllString(content, "")

	content = singleLineRE.ReplaceAllString(content, "")

	lines := strings.Split(content, "\n")
	var cleaned []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			continue
		}

		cleaned = append(cleaned, trimmed)
	}

	return strings.Join(cleaned, "\n")
}
