package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/genai"
)

const model = "google/gemini-3-flash-preview"

const cppPrompt = `
Task: Analyze the following C/C++ component.
The codebase context (including these files) is loaded in your memory.

Rules for Extraction:
1. NAME: Extract the exact identifier (e.g., "CMainFrame"). Keep it strictly under 256 characters.
2. SYNTHESIS: Read the header (.h/.hpp) to identify the entities (classes, structs, functions). Read the implementation (.c/.cpp) to determine the legacy markers (e.g., uses_open_utm, uses_psdb) and dependencies.
3. CONCISENESS: Provide a structural summary. Do not output raw code blocks.

Files making up this component:
%s
`

const lightweightPrompt = `
Task: Analyze the following build script, configuration, or data file.
The codebase context is loaded in your memory.

Rules for Extraction:
1. SUMMARY: Provide a strictly factual, 1-2 sentence explanation of what this file controls, builds, or configures.
2. KEY ELEMENTS: 
   - If this is a Makefile or build script, list the primary execution targets (e.g., "all", "clean", "build").
   - If this is an XML or config file, list the root or most critical configuration nodes/keys.
3. REFERENCES: List only the exact filenames, modules, or scripts explicitly imported, included, or executed by this file.

Files making up this component:
%s
`

type Brief = json.RawMessage

type Chunk struct {
	content []byte
	name    string
	cache   ManagedCache
}

type ManagedCache struct {
	activeFiles atomic.Int32
	cache       *genai.CachedContent
	creationMu  sync.Mutex
}

func (mc *ManagedCache) GetOrCreate(ctx context.Context, client *genai.Client, chunk *Chunk) (*genai.CachedContent, error) {
	if mc.cache != nil {
		return mc.cache, nil
	}

	mc.creationMu.Lock()
	defer mc.creationMu.Unlock()

	if mc.cache != nil {
		return mc.cache, nil
	}

	attempt := 0

	for {
		if attempt > 500 {
			return nil, ErrTooMuchRetries
		}

		cache, err := client.Caches.Create(ctx, model, &genai.CreateCachedContentConfig{
			DisplayName: chunk.name,
			Contents: []*genai.Content{
				{Role: "user", Parts: []*genai.Part{{Text: string(chunk.content)}}},
			},
			TTL: 3600 * time.Second,
		})
		if err == nil {
			mc.cache = cache
			break
		}

		waitWithBackoff(attempt)
		attempt++
	}

	return mc.cache, nil
}

type Briefer int

const (
	WithCache Briefer = iota
	WithoutCache
)

type ComponentType string

const (
	CppComponent          ComponentType = "cpp_component"
	MakefileComponent     ComponentType = "makefile"
	XmlComponent          ComponentType = "config"
	RationalRoseComponent ComponentType = "rational rose"
	ShellComponent        ComponentType = "shell"
	OtherComponent        ComponentType = "other"
)

type LogicalComponent struct {
	BaseName  string
	Dir       string
	FilePaths []string
	Type      ComponentType
}

func GroupFiles(allFiles []string) []*LogicalComponent {
	componentsMap := make(map[string]*LogicalComponent)
	var standalone []*LogicalComponent

	for _, path := range allFiles {
		ext := filepath.Ext(path)
		dir := filepath.Dir(path)
		base := filepath.Base(path)
		nameWithoutExt := strings.TrimSuffix(base, ext)

		if isCppFile(base) {
			output := filepath.Join(dir, nameWithoutExt)
			groupKey := filepath.Join(dir, nameWithoutExt)

			if _, exists := componentsMap[groupKey]; !exists {
				componentsMap[groupKey] = &LogicalComponent{
					BaseName:  output,
					Dir:       dir,
					FilePaths: []string{},
					Type:      CppComponent,
				}
			}
			componentsMap[groupKey].FilePaths = append(componentsMap[groupKey].FilePaths, path)
		} else {
			fileType := OtherComponent
			if isXml(base) {
				fileType = XmlComponent
			}
			if isMakefile(base) {
				fileType = MakefileComponent
			}

			if isRationalRose(base) {
				fileType = RationalRoseComponent
			}
			output := path

			standalone = append(standalone, &LogicalComponent{
				BaseName:  output,
				Dir:       dir,
				FilePaths: []string{path},
				Type:      fileType,
			})
		}
	}

	var results []*LogicalComponent
	for _, comp := range componentsMap {
		sort.Slice(comp.FilePaths, func(i, j int) bool {
			return comp.FilePaths[i] > comp.FilePaths[j]
		})
		results = append(results, comp)
	}

	results = append(results, standalone...)

	sort.Slice(results, func(i, j int) bool {
		return results[i].BaseName < results[j].BaseName
	})

	return results
}

type AnalysisTask struct {
	Component  *LogicalComponent
	Chunk      *Chunk
	Temp       float32
	Attempt    int
	OutputPath string
	Briefer    Briefer
}

type Limiter struct {
	rateLimiter *rate.Limiter
	wg          sync.WaitGroup
	tasks       []*AnalysisTask
}

func NewLimiter(requestsPerMinute int) *Limiter {
	limit := rate.Every(time.Minute / time.Duration(requestsPerMinute))

	return &Limiter{
		rateLimiter: rate.NewLimiter(limit, 3),
	}
}

func (l *Limiter) Plan(ctx context.Context, client *genai.Client, srcDir, outDir string) error {
	chunkPaths := []string{}

	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
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

		chunkPaths = append(chunkPaths, filepath.Join(srcDir, d.Name()))

		return nil
	})
	if err != nil {
		return fmt.Errorf("filepath.WalkDir(%q): %w", srcDir, err)
	}

	fmt.Printf("Found %d chunks to brief\n", len(chunkPaths))

	caches, err := listCaches(ctx, client)
	if err != nil {
		return fmt.Errorf("listCaches(): %w", err)
	}

	fmt.Printf("Found %d caches to reuse\n", len(caches))

	for _, cPath := range chunkPaths {
		chunkData, err := os.ReadFile(cPath)
		if err != nil {
			return fmt.Errorf("os.ReadFile(%q): %w", cPath, err)
		}

		filePaths, err := extractFilePathsFromChunk(chunkData)
		if err != nil {
			return fmt.Errorf("extractFilePathsFromChunk(): %w", err)
		}

		components := GroupFiles(filePaths)

		remainingComponents := []*LogicalComponent{}

		for _, c := range components {
			outputPath := filepath.Join(outDir, c.BaseName+".json")

			if _, err := os.Stat(outputPath); err == nil {
				continue
			}

			remainingComponents = append(remainingComponents, c)
		}

		if len(remainingComponents) == 0 {
			continue
		}

		chunk := Chunk{
			name:    cPath,
			content: chunkData,
			cache:   &ManagedCache{},
		}

		potentialCache := findCacheForChunks(caches, &chunk)
		if potentialCache != nil {
			chunk.cache.cache = potentialCache
		}

		chunk.cache.activeFiles.Store(int32(len(remainingComponents)))

		briefer := WithoutCache
		if len(chunkData) > 4096 {
			briefer = WithCache
		}

		for _, c := range remainingComponents {
			outputPath := filepath.Join(outDir, c.BaseName+".json")

			l.tasks = append(l.tasks, &AnalysisTask{
				Component:  c,
				Chunk:      &chunk,
				Temp:       0,
				OutputPath: outputPath,
				Briefer:    briefer,
			})
		}
	}

	return nil
}

func findCacheForChunks(caches []*genai.CachedContent, chunk *Chunk) *genai.CachedContent {
	for _, c := range caches {
		if c.DisplayName == chunk.name {
			return c
		}
	}

	return nil
}

func (l *Limiter) Execute(ctx context.Context, client *genai.Client) error {
	for _, task := range l.tasks {
		if err := l.rateLimiter.Wait(ctx); err != nil {
			return fmt.Errorf("l.rateLimiter.Wait(): %w", err)
		}

		l.wg.Go(func() {
			defer func() {
				if remaining := task.Chunk.cache.activeFiles.Add(-1); remaining == 0 {
					_ = cleanupChunk(ctx, client, task.Chunk)
				}
				log.Printf("✅ Chunk %q completed\n", task.Chunk.name)
			}()

			start := time.Now()
			brief, err := retryBrief(ctx, client, task)

			if err != nil {
				log.Printf("\t\t❌ cannot brief file %q too many retries. Error: %s", task.Component, err)
				return
			}

			if err := saveBriefOnDisk(brief, task.OutputPath); err != nil {
				log.Printf("\t\t❌ cannot saveBriefOnDisk %q: %s", task.OutputPath, err)
			}

			fmt.Printf("\t\t✔️ %q saved on disk for %q in %s\n", task.OutputPath, task.Chunk.name, time.Since(start))
		})
	}

	l.wg.Wait()

	return nil
}

func GenerateBriefsFromChunks(ctx context.Context, client *genai.Client, src, output string) error {
	start := time.Now()
	limiter := NewLimiter(15)
	if err := limiter.Plan(ctx, client, src, output); err != nil {
		return fmt.Errorf("limiter.Plan(): %w", err)
	}

	fmt.Printf("Planned %d tasks in %s\n", len(limiter.tasks), time.Since(start))

	if err := limiter.Execute(ctx, client); err != nil {
		return fmt.Errorf("limiter.Execute(): %w", err)
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

func retryBrief(ctx context.Context, client *genai.Client, task *AnalysisTask) (Brief, error) {
	var lastError error
	attempt := 0
	rateLimitAttemp := 0

	briefer := BriefChunkWithoutCache
	if task.Briefer == WithCache {
		briefer = BriefChunkWithCache

		cache, err := task.Chunk.cache.GetOrCreate(ctx, client, task.Chunk)
		if err != nil {
			return nil, fmt.Errorf("getOrCreateCache(): %w", err)
		}

		task.Chunk.cache.cache = cache
	}

	for {
		if attempt >= maxRetries {
			return nil, fmt.Errorf("maximum retries reached, last error %w", lastError)
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, 45*time.Second)

		brief, err := briefer(timeoutCtx, client, task.Chunk, task.Component, task.Temp)
		cancel()

		if err == nil {
			if rateLimitAttemp > 10 {
				fmt.Printf("spectacular rateLimitAttempt %d\n", rateLimitAttemp)
			}
			return brief, nil
		}
		lastError = err

		if strings.Contains(err.Error(), "429") {
			rateLimitAttemp++
			lastError = ErrRateLimited
			waitWithBackoff(rateLimitAttemp)
		} else if errors.Is(err, context.DeadlineExceeded) ||
			strings.Contains(err.Error(), "The operation was cancelled") ||
			strings.Contains(err.Error(), "Deadline expired before operation could complete") {
			task.Temp = float32(math.Min(float64(task.Temp+0.3), 1.0))
			lastError = ErrContextExpired
			attempt++
		} else {
			return nil, fmt.Errorf("briefer(%q): %w", task.Component, err)
		}
	}
}

func waitWithBackoff(attempt int) {
	delay := min(time.Duration(math.Pow(2, float64(attempt)))*time.Second, 60*time.Second)

	jitter := time.Duration(rand.Intn(1000)) * time.Millisecond

	time.Sleep(delay + jitter)
}

func cleanupChunk(ctx context.Context, client *genai.Client, chunk *Chunk) error {
	if chunk.cache.cache != nil {
		_, err := client.Caches.Delete(ctx, chunk.cache.cache.Name, nil)
		if err != nil {
			return fmt.Errorf("client.Caches.Delete(%q): %w", chunk.cache.cache.Name, err)
		}
	}

	return nil
}

func listCaches(ctx context.Context, client *genai.Client) ([]*genai.CachedContent, error) {
	caches := []*genai.CachedContent{}
	page, err := client.Caches.List(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("client.Caches.List(): %w", err)
	}

	for {
		caches = append(caches, page.Items...)

		page, err = page.Next(ctx)
		if err != nil {
			if errors.Is(err, genai.ErrPageDone) {
				break
			}

			return nil, fmt.Errorf("client.Caches.List(): %w", err)
		}
	}

	return caches, nil
}

func BriefChunkWithCache(ctx context.Context, client *genai.Client, chunk *Chunk, component *LogicalComponent, temp float32) (Brief, error) {
	resp, err := requestAnalyseWithCache(ctx, client, component, chunk.cache.cache, temp)
	if err != nil {
		return nil, fmt.Errorf("model.GenerateContent(): %w", err)
	}
	content, err := extractContentFromResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("extractContentFromResponse(): %w", err)
	}

	return content, nil
}

func requestAnalyseWithCache(ctx context.Context, client *genai.Client, component *LogicalComponent, cache *genai.CachedContent, temp float32) (*genai.GenerateContentResponse, error) {
	files := strings.Join(component.FilePaths, "\n- ")
	prompt, schema := choosePrompt(component)

	return client.Models.GenerateContent(
		ctx,
		model,
		genai.Text(fmt.Sprintf(prompt, files)),
		&genai.GenerateContentConfig{
			CachedContent:    cache.Name,
			ResponseMIMEType: "application/json",
			ResponseSchema:   schema,
			SafetySettings:   safetySettings(),
			Temperature:      &temp,
			PresencePenalty:  newT(float32(0.1)),
			FrequencyPenalty: newT(float32(0.3)),
		},
	)
}

func BriefChunkWithoutCache(ctx context.Context, client *genai.Client, chunk *Chunk, component *LogicalComponent, temp float32) (Brief, error) {
	resp, err := requestAnalyseWithoutCache(ctx, client, component, string(chunk.content), temp)
	if err != nil {
		return nil, fmt.Errorf("model.GenerateContent(): %w", err)
	}
	content, err := extractContentFromResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("extractContentFromResponse(): %w", err)
	}

	return content, nil
}

func requestAnalyseWithoutCache(ctx context.Context, client *genai.Client, component *LogicalComponent, chunkData string, temp float32) (*genai.GenerateContentResponse, error) {
	files := strings.Join(component.FilePaths, "\n- ")
	prompt, schema := choosePrompt(component)

	return client.Models.GenerateContent(
		ctx,
		model,
		genai.Text(fmt.Sprintf("File: %s\n", chunkData)+fmt.Sprintf(prompt, files)),
		&genai.GenerateContentConfig{
			ResponseMIMEType: "application/json",
			ResponseSchema:   schema,
			SafetySettings:   safetySettings(),
			Temperature:      &temp,
			PresencePenalty:  newT(float32(0.1)),
			FrequencyPenalty: newT(float32(0.3)),
		},
	)
}

func choosePrompt(component *LogicalComponent) (string, *genai.Schema) {
	switch component.Type {
	case CppComponent:
		return cppPrompt, analyseSchema()
	case RationalRoseComponent:
		return rationalRosePrompt, lightweightSchema()
	case MakefileComponent:
		return makefilePrompt, lightweightSchema()
	case XmlComponent:
		return xmlPrompt, lightweightSchema()
	case ShellComponent:
		return shellPrompt, lightweightSchema()
	case OtherComponent:
		fallthrough
	default:
		return otherPrompt, lightweightSchema()
	}
}

func newT[T any](v T) *T {
	return &v
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

func lightweightSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"file_path": {Type: genai.TypeString},
			"type": {
				Type: genai.TypeString,
				Enum: []string{"build_script", "configuration", "deployment", "data", "documentation", "other"},
			},
			"summary": {
				Type:        genai.TypeString,
				Description: "A concise explanation of what this file builds, configures, or executes. Max 2 sentences.",
			},
			"references": {
				Type:        genai.TypeArray,
				Items:       &genai.Schema{Type: genai.TypeString},
				Description: "Other files, directories, or modules explicitly referenced or included by this file.",
			},
			"key_elements": {
				Type:        genai.TypeArray,
				Items:       &genai.Schema{Type: genai.TypeString},
				Description: "The primary Make targets (if a Makefile), root XML nodes (if XML), or main config keys.",
			},
		},
		Required: []string{"file_path", "type", "summary"},
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

var (
	ErrNoPathInChunk = fmt.Errorf("no paths found in <directory_structure> block")
)

func extractFilePathsFromChunk(chunk []byte) ([]string, error) {
	lines := strings.Split(string(chunk), "\n")
	paths := []string{}
	inDirectoryStructure := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if trimmed == "<directory_structure>" {
			inDirectoryStructure = true
			continue
		}
		if trimmed == "</directory_structure>" {
			break
		}

		if inDirectoryStructure && strings.HasPrefix(trimmed, "- ") {
			paths = append(paths, strings.TrimSpace(trimmed[2:]))
		}
	}

	if len(paths) == 0 {
		return nil, ErrNoPathInChunk
	}

	return paths, nil
}

var cppExtensions = []string{
	".cc", ".cpp", ".cxx", ".c++", ".pcc", ".tpp", // c++
	".hh", ".hpp", ".hxx", ".inl", ".ipp", // c++ header
	".cppm", ".ixx", ".ccm", ".mpp", ".mxx", ".cxxm", ".hppm", ".hxxm", // c++ module
	".c", ".ec", ".pgc", // c
	".h", // c header
}

func isCppFile(file string) bool {
	ext := strings.ToLower(filepath.Ext(file))

	return slices.Contains(cppExtensions, ext)
}

func isJava(file string) bool {
	ext := strings.ToLower(filepath.Ext(file))

	return ext == ".java"
}

func isRationalRose(file string) bool {
	ext := strings.ToLower(filepath.Ext(file))

	return ext == ".cat" || ext == ".mdl"
}

func isXml(file string) bool {
	ext := strings.ToLower(filepath.Ext(file))

	return ext == ".xml"
}

func isMakefile(file string) bool {
	return strings.ToLower(file) == "makefile"
}

func isShell(file string) bool {
	ext := strings.ToLower(filepath.Ext(file))

	return ext == ".sh" || ext == ".csh" || ext == ".ksh" || ext == ".bash"
}
