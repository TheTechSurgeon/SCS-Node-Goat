package local

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"
	"golang.org/x/xerrors"

	"github.com/aquasecurity/trivy/pkg/fanal/analyzer"
	"github.com/aquasecurity/trivy/pkg/fanal/artifact"
	"github.com/aquasecurity/trivy/pkg/fanal/cache"
	"github.com/aquasecurity/trivy/pkg/fanal/handler"
	"github.com/aquasecurity/trivy/pkg/fanal/types"
	"github.com/aquasecurity/trivy/pkg/fanal/walker"
	"github.com/aquasecurity/trivy/pkg/log"
	"github.com/aquasecurity/trivy/pkg/semaphore"
)

type Artifact struct {
	rootPath       string
	cache          cache.ArtifactCache
	walker         walker.FS
	analyzer       analyzer.AnalyzerGroup
	handlerManager handler.Manager

	artifactOption artifact.Option
}

func NewArtifact(rootPath string, c cache.ArtifactCache, opt artifact.Option) (artifact.Artifact, error) {
	handlerManager, err := handler.NewManager(opt)
	if err != nil {
		return nil, xerrors.Errorf("handler initialize error: %w", err)
	}

	a, err := analyzer.NewAnalyzerGroup(opt.AnalyzerOptions())
	if err != nil {
		return nil, xerrors.Errorf("analyzer group error: %w", err)
	}

	return Artifact{
		rootPath: filepath.ToSlash(filepath.Clean(rootPath)),
		cache:    c,
		walker: walker.NewFS(buildPathsToSkip(rootPath, opt.SkipFiles), buildPathsToSkip(rootPath, opt.SkipDirs),
			opt.Parallel, opt.WalkOption.ErrorCallback),
		analyzer:       a,
		handlerManager: handlerManager,

		artifactOption: opt,
	}, nil
}

// buildPathsToSkip builds correct patch for skipDirs and skipFiles
func buildPathsToSkip(base string, paths []string) []string {
	var relativePaths []string
	absBase, err := filepath.Abs(base)
	if err != nil {
		log.Warn("Failed to get an absolute path", log.String("base", base), log.Err(err))
		return nil
	}
	for _, path := range paths {
		// Supports three types of flag specification.
		// All of them are converted into the relative path from the root directory.
		// 1. Relative skip dirs/files from the root directory
		//     The specified dirs and files will be used as is.
		//       e.g. $ trivy fs --skip-dirs bar ./foo
		//     The skip dir from the root directory will be `bar/`.
		// 2. Relative skip dirs/files from the working directory
		//     The specified dirs and files wll be converted to the relative path from the root directory.
		//       e.g. $ trivy fs --skip-dirs ./foo/bar ./foo
		//     The skip dir will be converted to `bar/`.
		// 3. Absolute skip dirs/files
		//     The specified dirs and files wll be converted to the relative path from the root directory.
		//       e.g. $ trivy fs --skip-dirs /bar/foo/baz ./foo
		//     When the working directory is
		//       3.1 /bar: the skip dir will be converted to `baz/`.
		//       3.2 /hoge : the skip dir will be converted to `../../bar/foo/baz/`.

		absSkipPath, err := filepath.Abs(path)
		if err != nil {
			log.Warn("Failed to get an absolute path", log.String("base", base), log.Err(err))
			continue
		}
		rel, err := filepath.Rel(absBase, absSkipPath)
		if err != nil {
			log.Warn("Failed to get an relative path", log.String("base", base), log.Err(err))
			continue
		}

		var relPath string
		switch {
		case !filepath.IsAbs(path) && strings.HasPrefix(rel, ".."):
			// #1: Use the path as is
			relPath = path
		case !filepath.IsAbs(path) && !strings.HasPrefix(rel, ".."):
			// #2: Use the relative path from the root directory
			relPath = rel
		case filepath.IsAbs(path):
			// #3: Use the relative path from the root directory
			relPath = rel
		}
		relPath = filepath.ToSlash(relPath)
		relativePaths = append(relativePaths, relPath)
	}
	return relativePaths
}

func (a Artifact) Inspect(ctx context.Context) (types.ArtifactReference, error) {
	var wg sync.WaitGroup
	result := analyzer.NewAnalysisResult()
	limit := semaphore.New(a.artifactOption.Parallel)
	opts := analyzer.AnalysisOptions{
		Offline:      a.artifactOption.Offline,
		FileChecksum: a.artifactOption.FileChecksum,
	}

	// Prepare filesystem for post analysis
	composite, err := a.analyzer.PostAnalyzerFS()
	if err != nil {
		return types.ArtifactReference{}, xerrors.Errorf("failed to prepare filesystem for post analysis: %w", err)
	}

	err = a.walker.Walk(a.rootPath, func(filePath string, info os.FileInfo, opener analyzer.Opener) error {
		dir := a.rootPath

		// When the directory is the same as the filePath, a file was given
		// instead of a directory, rewrite the file path and directory in this case.
		if filePath == "." {
			dir, filePath = path.Split(a.rootPath)
		}

		if err := a.analyzer.AnalyzeFile(ctx, &wg, limit, result, dir, filePath, info, opener, nil, opts); err != nil {
			return xerrors.Errorf("analyze file (%s): %w", filePath, err)
		}

		// Skip post analysis if the file is not required
		analyzerTypes := a.analyzer.RequiredPostAnalyzers(filePath, info)
		if len(analyzerTypes) == 0 {
			return nil
		}

		// Build filesystem for post analysis
		if err := composite.CreateLink(analyzerTypes, dir, filePath, filepath.Join(dir, filePath)); err != nil {
			return xerrors.Errorf("failed to create link: %w", err)
		}

		return nil
	})
	if err != nil {
		return types.ArtifactReference{}, xerrors.Errorf("walk filesystem: %w", err)
	}

	// Wait for all the goroutine to finish.
	wg.Wait()

	// Post-analysis
	if err = a.analyzer.PostAnalyze(ctx, composite, result, opts); err != nil {
		return types.ArtifactReference{}, xerrors.Errorf("post analysis error: %w", err)
	}

	// Sort the analysis result for consistent results
	result.Sort()

	blobInfo := types.BlobInfo{
		SchemaVersion:     types.BlobJSONSchemaVersion,
		OS:                result.OS,
		Repository:        result.Repository,
		PackageInfos:      result.PackageInfos,
		Applications:      result.Applications,
		Misconfigurations: result.Misconfigurations,
		Secrets:           result.Secrets,
		Licenses:          result.Licenses,
		CustomResources:   result.CustomResources,
	}

	if err = a.handlerManager.PostHandle(ctx, result, &blobInfo); err != nil {
		return types.ArtifactReference{}, xerrors.Errorf("failed to call hooks: %w", err)
	}

	cacheKey, err := a.calcCacheKey(blobInfo)
	if err != nil {
		return types.ArtifactReference{}, xerrors.Errorf("failed to calculate a cache key: %w", err)
	}

	if err = a.cache.PutBlob(cacheKey, blobInfo); err != nil {
		return types.ArtifactReference{}, xerrors.Errorf("failed to store blob (%s) in cache: %w", cacheKey, err)
	}

	// get hostname
	var hostName string
	b, err := os.ReadFile(filepath.Join(a.rootPath, "etc", "hostname"))
	if err == nil && len(b) != 0 {
		hostName = strings.TrimSpace(string(b))
	} else {
		// To slash for Windows
		hostName = filepath.ToSlash(a.rootPath)
	}

	return types.ArtifactReference{
		Name:    hostName,
		Type:    types.ArtifactFilesystem,
		ID:      cacheKey, // use a cache key as pseudo artifact ID
		BlobIDs: []string{cacheKey},
	}, nil
}

func (a Artifact) Clean(reference types.ArtifactReference) error {
	return a.cache.DeleteBlobs(reference.BlobIDs)
}

func (a Artifact) calcCacheKey(blobInfo types.BlobInfo) (string, error) {
	// calculate hash of JSON and use it as pseudo artifactID and blobID
	h := sha256.New()
	if err := json.NewEncoder(h).Encode(blobInfo); err != nil {
		return "", xerrors.Errorf("json error: %w", err)
	}

	d := digest.NewDigest(digest.SHA256, h)
	cacheKey, err := cache.CalcKey(d.String(), a.analyzer.AnalyzerVersions(), a.handlerManager.Versions(), a.artifactOption)
	if err != nil {
		return "", xerrors.Errorf("cache key: %w", err)
	}

	return cacheKey, nil
}
