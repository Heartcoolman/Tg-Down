// Package chunked provides chunked download functionality.
package chunked

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		onProgress: func(downloaded, total int64) {
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

	// Create temporary file
	tempPath := filePath + ".tmp"
	file, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer file.Close()

	// Pre-allocate file space
	if err := file.Truncate(size); err != nil {
		cd.logger.Warn("Failed to pre-allocate file space: %v", err)
	}

	// Calculate number of chunks
	numChunks := (size + int64(cd.chunkSize) - 1) / int64(cd.chunkSize)
	cd.logger.Info("Download will use %d chunks with %d workers", numChunks, cd.maxWorkers)

	// Progress tracking
	var downloaded int64
	var mu sync.Mutex
	progressTicker := time.NewTicker(ProgressUpdateInterval)
	defer progressTicker.Stop()

	// Start progress reporting
	progressCtx, progressCancel := context.WithCancel(ctx)
	defer progressCancel()

	go func() {
		for {
			select {
			case <-progressTicker.C:
				mu.Lock()
				current := downloaded
				mu.Unlock()
				cd.onProgress(current, size)
			case <-progressCtx.Done():
				return
			}
		}
	}()

	// Create channels for work distribution
	chunkChan := make(chan chunkJob, cd.maxWorkers*ChunkChannelMultiplier)
	errorChan := make(chan error, cd.maxWorkers)
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < cd.maxWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			cd.worker(ctx, workerID, downloadFunc, file, chunkChan, errorChan, &downloaded, &mu)
		}(i)
	}

	// Send chunk jobs
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

	// Wait for all workers to complete
	go func() {
		wg.Wait()
		close(errorChan)
	}()

	// Check for errors
	for err := range errorChan {
		if err != nil {
			cd.logger.Error("Chunk download error: %v", err)
			if remErr := os.Remove(tempPath); remErr != nil {
				cd.logger.Error("Failed to remove temp file: %v", remErr)
			}
			return err
		}
	}

	// Final progress update
	cd.onProgress(size, size)

	// Rename temp file to final name with retry
	if _, err := os.Stat(tempPath); os.IsNotExist(err) {
		return fmt.Errorf("临时文件不存在: %s", tempPath)
	}
	var renameErr error
	for retry := 0; retry < MaxRenameRetries; retry++ {
		renameErr = os.Rename(tempPath, filePath)
		if renameErr == nil {
			return nil
		}
		cd.logger.Warn("重命名失败 (尝试 %d): %v", retry+1, renameErr)
		time.Sleep(RenameSleepDuration)
	}
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
	workerID int,
	downloadFunc DownloadFunc,
	file *os.File,
	jobs <-chan chunkJob,
	errors chan<- error,
	downloaded *int64,
	mu *sync.Mutex,
) {
	cd.logger.Debug("Worker %d started", workerID)
	defer cd.logger.Debug("Worker %d finished", workerID)

	for job := range jobs {
		select {
		case <-ctx.Done():
			errors <- ctx.Err()
			return
		default:
		}

		cd.logger.Debug("Worker %d downloading chunk at offset %d, size %d", workerID, job.offset, job.size)

		// Download chunk with retry
		var data []byte
		var err error
		for retry := 0; retry < MaxDownloadRetries; retry++ {
			data, err = downloadFunc(job.offset, job.size)
			if err == nil {
				break
			}
			cd.logger.Warn("Worker %d retry %d for chunk at offset %d: %v", workerID, retry+1, job.offset, err)
			time.Sleep(time.Duration(retry+1) * RetryDelayBase)
		}

		if err != nil {
			cd.logger.Error("Worker %d failed to download chunk after retries: %v", workerID, err)
			errors <- fmt.Errorf("failed to download chunk at offset %d: %w", job.offset, err)
			return
		}

		// Write chunk to file
		if _, err := file.WriteAt(data, job.offset); err != nil {
			cd.logger.Error("Worker %d failed to write chunk: %v", workerID, err)
			errors <- fmt.Errorf("failed to write chunk at offset %d: %w", job.offset, err)
			return
		}

		// Update progress
		mu.Lock()
		*downloaded += int64(len(data))
		mu.Unlock()

		cd.logger.Debug("Worker %d completed chunk at offset %d", workerID, job.offset)
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
