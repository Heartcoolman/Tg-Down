// Package chunked provides chunked download functionality.
package chunked

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tg-down/internal/logger"
)

const (
	// DefaultChunkSize is the default chunk size for downloads
	DefaultChunkSize = 512 * 1024 // 512KB
	// DefaultMaxWorkers is the default number of concurrent workers
	DefaultMaxWorkers = 4
	// ProgressUpdateInterval is how often to update progress
	ProgressUpdateInterval = time.Second
	// DirectoryPermission is the permission for creating directories
	DirectoryPermission = 0750
	// ChunkChannelMultiplier is the multiplier for chunk channel buffer size
	ChunkChannelMultiplier = 2
	// MaxRenameRetries is the maximum number of rename retries
	MaxRenameRetries = 5
	// RenameSleepDuration is the sleep duration between rename retries
	RenameSleepDuration = 500 * time.Millisecond
	// MaxDownloadRetries is the maximum number of download retries per chunk
	MaxDownloadRetries = 3
	// RetryDelayBase is the base delay for retry calculations
	RetryDelayBase = time.Second
)

// DownloadFunc represents a function that downloads a chunk of data
type DownloadFunc func(offset int64, limit int) ([]byte, error)

// ProgressCallback represents a progress callback function
type ProgressCallback func(downloaded, total int64)

// ChunkDownloader handles chunked downloads
type ChunkDownloader struct {
	chunkSize  int
	maxWorkers int
	logger     *logger.Logger
	onProgress ProgressCallback
}

// New creates a new ChunkDownloader with default settings
func New(logger *logger.Logger) *ChunkDownloader {
	return &ChunkDownloader{
		chunkSize:  DefaultChunkSize,
		maxWorkers: DefaultMaxWorkers,
		logger:     logger,
		onProgress: func(_, _ int64) {
			// Default no-op progress callback
		},
	}
}

// WithChunkSize sets the chunk size
func (cd *ChunkDownloader) WithChunkSize(size int) *ChunkDownloader {
	cd.chunkSize = size
	return cd
}

// WithMaxWorkers sets the maximum number of workers
func (cd *ChunkDownloader) WithMaxWorkers(workers int) *ChunkDownloader {
	cd.maxWorkers = workers
	return cd
}

// WithProgressCallback sets the progress callback
func (cd *ChunkDownloader) WithProgressCallback(callback ProgressCallback) *ChunkDownloader {
	cd.onProgress = callback
	return cd
}

// DownloadToFile downloads a file using chunked download
func (cd *ChunkDownloader) DownloadToFile(
	ctx context.Context,
	downloadFunc DownloadFunc,
	size int64,
	filePath string,
) error {
	cd.logger.Info("Starting chunked download: %s (size: %d bytes)", filePath, size)

	// Create directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(filePath), DirectoryPermission); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Create and prepare temporary file
	tempPath := filePath + ".tmp"
	file, err := cd.createTempFile(tempPath, size)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			cd.logger.Error("Failed to close temp file: %v", closeErr)
		}
	}()

	// Perform the download
	if err := cd.performChunkedDownload(ctx, downloadFunc, file, size); err != nil {
		if remErr := os.Remove(tempPath); remErr != nil {
			cd.logger.Error("Failed to remove temp file: %v", remErr)
		}
		return err
	}

	// Finalize the download
	return cd.finalizeDownload(tempPath, filePath)
}

// createTempFile creates and prepares the temporary file
func (cd *ChunkDownloader) createTempFile(tempPath string, size int64) (*os.File, error) {
	// Validate the temp path to prevent directory traversal attacks
	cleanPath := filepath.Clean(tempPath)
	if cleanPath != tempPath {
		return nil, fmt.Errorf("invalid temp path: %s", tempPath)
	}

	// Ensure the path doesn't contain directory traversal patterns
	if strings.Contains(tempPath, "..") {
		return nil, fmt.Errorf("path contains directory traversal: %s", tempPath)
	}

	file, err := os.Create(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}

	// Pre-allocate file space
	if err := file.Truncate(size); err != nil {
		cd.logger.Warn("Failed to pre-allocate file space: %v", err)
	}

	return file, nil
}

// performChunkedDownload handles the actual chunked download process
func (cd *ChunkDownloader) performChunkedDownload(
	ctx context.Context,
	downloadFunc DownloadFunc,
	file *os.File,
	size int64,
) error {
	// Calculate number of chunks
	numChunks := (size + int64(cd.chunkSize) - 1) / int64(cd.chunkSize)
	cd.logger.Info("Download will use %d chunks with %d workers", numChunks, cd.maxWorkers)

	// Setup progress tracking
	var downloaded int64
	var mu sync.Mutex
	progressCtx, progressCancel := context.WithCancel(ctx)
	defer progressCancel()

	cd.startProgressReporting(progressCtx, &downloaded, &mu, size)

	// Setup work distribution
	chunkChan := make(chan chunkJob, cd.maxWorkers*ChunkChannelMultiplier)
	errorChan := make(chan error, cd.maxWorkers)
	var wg sync.WaitGroup

	// Start workers
	cd.startWorkers(ctx, downloadFunc, file, chunkChan, errorChan, &downloaded, &mu, &wg)

	// Send chunk jobs
	cd.sendChunkJobs(ctx, chunkChan, numChunks, size)

	// Wait and check for errors
	return cd.waitForCompletion(&wg, errorChan, size)
}

// startProgressReporting starts the progress reporting goroutine
func (cd *ChunkDownloader) startProgressReporting(
	ctx context.Context,
	downloaded *int64,
	mu *sync.Mutex,
	size int64,
) {
	progressTicker := time.NewTicker(ProgressUpdateInterval)
	go func() {
		defer progressTicker.Stop()
		for {
			select {
			case <-progressTicker.C:
				mu.Lock()
				current := *downloaded
				mu.Unlock()
				cd.onProgress(current, size)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// startWorkers starts the download workers
func (cd *ChunkDownloader) startWorkers(
	ctx context.Context,
	downloadFunc DownloadFunc,
	file *os.File,
	chunkChan <-chan chunkJob,
	errorChan chan<- error,
	downloaded *int64,
	mu *sync.Mutex,
	wg *sync.WaitGroup,
) {
	for i := 0; i < cd.maxWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			cd.worker(ctx, workerID, downloadFunc, file, chunkChan, errorChan, downloaded, mu)
		}(i)
	}
}

// sendChunkJobs sends chunk jobs to workers
func (cd *ChunkDownloader) sendChunkJobs(
	ctx context.Context,
	chunkChan chan<- chunkJob,
	numChunks int64,
	size int64,
) {
	go func() {
		defer close(chunkChan)
		for i := int64(0); i < numChunks; i++ {
			offset := i * int64(cd.chunkSize)
			chunkSize := cd.chunkSize
			if offset+int64(chunkSize) > size {
				chunkSize = int(size - offset)
			}

			select {
			case chunkChan <- chunkJob{
				offset: offset,
				size:   chunkSize,
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
}

// waitForCompletion waits for all workers to complete and checks for errors
func (cd *ChunkDownloader) waitForCompletion(
	wg *sync.WaitGroup,
	errorChan chan error,
	size int64,
) error {
	// Wait for all workers to complete
	go func() {
		wg.Wait()
		close(errorChan)
	}()

	// Check for errors
	for err := range errorChan {
		if err != nil {
			cd.logger.Error("Chunk download error: %v", err)
			return err
		}
	}

	// Final progress update
	cd.onProgress(size, size)
	return nil
}

// finalizeDownload renames the temporary file to the final name
func (cd *ChunkDownloader) finalizeDownload(tempPath, filePath string) error {
	// Check if temp file exists
	if _, err := os.Stat(tempPath); os.IsNotExist(err) {
		return fmt.Errorf("临时文件不存在: %s", tempPath)
	}

	// Rename temp file to final name with retry
	var renameErr error
	for retry := 0; retry < MaxRenameRetries; retry++ {
		renameErr = os.Rename(tempPath, filePath)
		if renameErr == nil {
			return nil
		}
		cd.logger.Warn("重命名失败 (尝试 %d): %v", retry+1, renameErr)
		time.Sleep(RenameSleepDuration)
	}

	// Clean up temp file if rename failed
	if remErr := os.Remove(tempPath); remErr != nil {
		cd.logger.Error("Failed to remove temp file: %v", remErr)
	}
	return fmt.Errorf("failed to rename temp file after retries: %w", renameErr)
}

// chunkJob represents a single chunk download job
type chunkJob struct {
	offset int64
	size   int
}

// worker processes chunk download jobs
func (cd *ChunkDownloader) worker(
	ctx context.Context,
	_ int, // workerID is kept for interface compatibility but not used
	downloadFunc DownloadFunc,
	file *os.File,
	jobs <-chan chunkJob,
	errors chan<- error,
	downloaded *int64,
	mu *sync.Mutex,
) {
	for job := range jobs {
		select {
		case <-ctx.Done():
			errors <- ctx.Err()
			return
		default:
		}

		cd.logger.Debug("Downloading chunk at offset %d, size %d", job.offset, job.size)

		// Download chunk with retry
		var data []byte
		var err error
		for retry := 0; retry < MaxDownloadRetries; retry++ {
			data, err = downloadFunc(job.offset, job.size)
			if err == nil {
				break
			}
			cd.logger.Warn("Retry %d for chunk at offset %d: %v", retry+1, job.offset, err)
			time.Sleep(time.Duration(retry+1) * RetryDelayBase)
		}

		if err != nil {
			cd.logger.Error("Failed to download chunk after retries: %v", err)
			errors <- fmt.Errorf("failed to download chunk at offset %d: %w", job.offset, err)
			return
		}

		// Write chunk to file
		if _, err := file.WriteAt(data, job.offset); err != nil {
			cd.logger.Error("Failed to write chunk: %v", err)
			errors <- fmt.Errorf("failed to write chunk at offset %d: %w", job.offset, err)
			return
		}

		// Update progress
		mu.Lock()
		*downloaded += int64(len(data))
		mu.Unlock()

		cd.logger.Debug("Completed chunk at offset %d", job.offset)
	}

	errors <- nil // Signal successful completion
}

// DownloadToWriter downloads a file using chunked download to an io.Writer
func (cd *ChunkDownloader) DownloadToWriter(
	ctx context.Context,
	downloadFunc DownloadFunc,
	size int64,
	writer io.Writer,
) error {
	cd.logger.Info("Starting chunked download to writer (size: %d bytes)", size)

	// For writer, we need to download sequentially to maintain order
	var downloaded int64
	progressTicker := time.NewTicker(ProgressUpdateInterval)
	defer progressTicker.Stop()

	// Start progress reporting
	progressCtx, progressCancel := context.WithCancel(ctx)
	defer progressCancel()

	go func() {
		for {
			select {
			case <-progressTicker.C:
				cd.onProgress(downloaded, size)
			case <-progressCtx.Done():
				return
			}
		}
	}()

	// Download in chunks sequentially
	for offset := int64(0); offset < size; {
		chunkSize := cd.chunkSize
		if offset+int64(chunkSize) > size {
			chunkSize = int(size - offset)
		}

		data, err := downloadFunc(offset, chunkSize)
		if err != nil {
			return fmt.Errorf("failed to download chunk at offset %d: %w", offset, err)
		}

		if _, err := writer.Write(data); err != nil {
			return fmt.Errorf("failed to write chunk: %w", err)
		}

		downloaded += int64(len(data))
		offset += int64(len(data))
	}

	cd.onProgress(size, size)
	cd.logger.Info("Chunked download to writer completed")
	return nil
}
