package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
)

// Configuration constants
const (
	// Maximum number of entries in processedObjects cache before pruning
	maxProcessedObjectsCache = 10000
	// TTL for processed objects cache entries
	processedObjectsCacheTTL = 24 * time.Hour
	// Valid pre-signed URL hosts for cloud storage
	validPresignedURLHosts = `^(.*\.s3\..*\.amazonaws\.com|.*\.s3\.amazonaws\.com|storage\.googleapis\.com|.*\.blob\.core\.windows\.net)$`
	// Rate limit: maximum API requests per minute
	apiRateLimitPerMinute = 60
	// Maximum request body size (10MB)
	maxRequestBodySize = 10 * 1024 * 1024
	// Minimum sync interval in seconds
	minSyncIntervalSeconds = 60
	// Maximum sync interval in seconds (24 hours)
	maxSyncIntervalSeconds = 86400
)

// processedObjectEntry stores cache entry with timestamp for TTL-based pruning
type processedObjectEntry struct {
	processedAt time.Time
}

var (
	logger           *logrus.Logger
	storageClient    StorageClient
	storageProvider  string
	storageLocation  string
	storagePrefix    string
	wonderfulAPIURL  string
	wonderfulTenant  string
	wonderfulEnv     string
	wonderfulRAGID   string
	wonderfulAPIKey  string
	internalAPIKey   string // API key for internal endpoints
	syncInterval     time.Duration
	maxFileSize      int64 // Maximum file size in bytes (0 = no limit)

	// Processed objects cache with TTL support
	processedObjects   map[string]processedObjectEntry
	processedObjectsMu sync.RWMutex

	// Sync mutex to prevent concurrent sync operations
	syncMu       sync.Mutex
	syncRunning  bool
	syncRunningMu sync.RWMutex

	// Rate limiting for API endpoints
	apiRequestCounts   map[string][]time.Time
	apiRequestCountsMu sync.Mutex

	// Pre-compiled regex for URL validation
	presignedURLHostRegex *regexp.Regexp

	// Cancellation context for graceful shutdown
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	httpClient *http.Client
)

var (
	syncRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wonderful_rag_sync_runs_total",
			Help: "Total number of sync runs.",
		},
		[]string{"provider"},
	)
	syncErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wonderful_rag_sync_errors_total",
			Help: "Total number of sync errors not tied to a specific file.",
		},
		[]string{"provider"},
	)
	filesProcessedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wonderful_rag_files_processed_total",
			Help: "Total number of files processed successfully.",
		},
		[]string{"provider"},
	)
	filesFailedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wonderful_rag_files_failed_total",
			Help: "Total number of files that failed to process.",
		},
		[]string{"provider"},
	)
	filesSkippedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "wonderful_rag_files_skipped_total",
			Help: "Total number of files skipped (archived or too large).",
		},
		[]string{"provider"},
	)
	syncDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "wonderful_rag_sync_duration_seconds",
			Help:    "Duration of sync runs in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"provider"},
	)
	syncInProgress = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wonderful_rag_sync_in_progress",
			Help: "Whether a sync is currently in progress (1/0).",
		},
		[]string{"provider"},
	)
	lastSyncTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wonderful_rag_last_sync_timestamp",
			Help: "Unix timestamp of the last completed sync.",
		},
		[]string{"provider"},
	)
	lastSyncSuccess = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wonderful_rag_last_sync_success",
			Help: "Whether the last sync completed without errors (1/0).",
		},
		[]string{"provider"},
	)
	lastSyncFilesFound = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wonderful_rag_last_sync_files_found",
			Help: "Number of files found in the last sync.",
		},
		[]string{"provider"},
	)
	lastSyncFilesProcessed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wonderful_rag_last_sync_files_processed",
			Help: "Number of files processed in the last sync.",
		},
		[]string{"provider"},
	)
	lastSyncFilesFailed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wonderful_rag_last_sync_files_failed",
			Help: "Number of files failed in the last sync.",
		},
		[]string{"provider"},
	)
	lastSyncFilesSkipped = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "wonderful_rag_last_sync_files_skipped",
			Help: "Number of files skipped in the last sync.",
		},
		[]string{"provider"},
	)
)

type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type StorageClient interface {
	Provider() string
	ListObjects(ctx context.Context) ([]ObjectInfo, error)
	DownloadObject(ctx context.Context, key string) ([]byte, error)
	MoveObject(ctx context.Context, srcKey, destKey string) error
	Location() string
	Prefix() string
}

type S3Storage struct {
	bucket     string
	prefix     string
	client     *s3.Client
	downloader *manager.Downloader
}

func (s *S3Storage) Provider() string { return "s3" }
func (s *S3Storage) Location() string { return s.bucket }
func (s *S3Storage) Prefix() string   { return s.prefix }

func (s *S3Storage) ListObjects(ctx context.Context) ([]ObjectInfo, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: &s.bucket,
	}
	if s.prefix != "" {
		input.Prefix = &s.prefix
	}

	var objects []ObjectInfo
	paginator := s3.NewListObjectsV2Paginator(s.client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects from S3 bucket %s: %w", s.bucket, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			lastModified := time.Time{}
			if obj.LastModified != nil {
				lastModified = *obj.LastModified
			}
			objects = append(objects, ObjectInfo{
				Key:          *obj.Key,
				Size:         *obj.Size,
				LastModified: lastModified,
			})
		}
	}
	return objects, nil
}

func (s *S3Storage) DownloadObject(ctx context.Context, key string) ([]byte, error) {
	buf := manager.NewWriteAtBuffer([]byte{})
	_, err := s.downloader.Download(ctx, buf, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download object %s from S3 bucket %s: %w", key, s.bucket, err)
	}
	return buf.Bytes(), nil
}

func (s *S3Storage) MoveObject(ctx context.Context, srcKey, destKey string) error {
	if srcKey == destKey {
		return nil
	}
	copySource := url.PathEscape(s.bucket + "/" + srcKey)
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     &s.bucket,
		Key:        &destKey,
		CopySource: &copySource,
	})
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    &srcKey,
	})
	return err
}

type GCSStorage struct {
	bucket string
	prefix string
	client *storage.Client
}

func (g *GCSStorage) Provider() string { return "gcs" }
func (g *GCSStorage) Location() string { return g.bucket }
func (g *GCSStorage) Prefix() string   { return g.prefix }

func (g *GCSStorage) ListObjects(ctx context.Context) ([]ObjectInfo, error) {
	bucket := g.client.Bucket(g.bucket)
	query := &storage.Query{}
	if g.prefix != "" {
		query.Prefix = g.prefix
	}
	it := bucket.Objects(ctx, query)
	objects := []ObjectInfo{}
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list objects from GCS bucket %s: %w", g.bucket, err)
		}
		objects = append(objects, ObjectInfo{
			Key:          attrs.Name,
			Size:         attrs.Size,
			LastModified: attrs.Updated,
		})
	}
	return objects, nil
}

func (g *GCSStorage) DownloadObject(ctx context.Context, key string) ([]byte, error) {
	reader, err := g.client.Bucket(g.bucket).Object(key).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create reader for GCS object %s in bucket %s: %w", key, g.bucket, err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read GCS object %s from bucket %s: %w", key, g.bucket, err)
	}
	return data, nil
}

func (g *GCSStorage) MoveObject(ctx context.Context, srcKey, destKey string) error {
	if srcKey == destKey {
		return nil
	}
	bucket := g.client.Bucket(g.bucket)
	copier := bucket.Object(destKey).CopierFrom(bucket.Object(srcKey))
	if _, err := copier.Run(ctx); err != nil {
		return err
	}
	return bucket.Object(srcKey).Delete(ctx)
}

type AzureStorage struct {
	container string
	prefix    string
	client    *azblob.Client
}

func (a *AzureStorage) Provider() string { return "azure" }
func (a *AzureStorage) Location() string { return a.container }
func (a *AzureStorage) Prefix() string   { return a.prefix }

func (a *AzureStorage) ListObjects(ctx context.Context) ([]ObjectInfo, error) {
	options := &azblob.ListBlobsFlatOptions{}
	if a.prefix != "" {
		options.Prefix = to.Ptr(a.prefix)
	}
	pager := a.client.NewListBlobsFlatPager(a.container, options)
	objects := []ObjectInfo{}
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list blobs from Azure container %s: %w", a.container, err)
		}
		for _, item := range resp.Segment.BlobItems {
			if item.Name == nil {
				continue
			}
			size := int64(0)
			lastModified := time.Time{}
			if item.Properties != nil {
				if item.Properties.ContentLength != nil {
					size = *item.Properties.ContentLength
				}
				if item.Properties.LastModified != nil {
					lastModified = *item.Properties.LastModified
				}
			}
			objects = append(objects, ObjectInfo{
				Key:          *item.Name,
				Size:         size,
				LastModified: lastModified,
			})
		}
	}
	return objects, nil
}

func (a *AzureStorage) DownloadObject(ctx context.Context, key string) ([]byte, error) {
	resp, err := a.client.DownloadStream(ctx, a.container, key, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to download blob %s from Azure container %s: %w", key, a.container, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read blob %s from Azure container %s: %w", key, a.container, err)
	}
	return data, nil
}

func (a *AzureStorage) MoveObject(ctx context.Context, srcKey, destKey string) error {
	if srcKey == destKey {
		return nil
	}

	// Use server-side copy instead of download/upload to save bandwidth and time
	// Get the source blob URL
	srcBlobURL := fmt.Sprintf("%s/%s", a.client.URL(), url.PathEscape(a.container+"/"+srcKey))

	// Get a blob client for the destination
	destBlobClient := a.client.ServiceClient().NewContainerClient(a.container).NewBlobClient(destKey)

	// Start the copy operation (server-side copy)
	_, err := destBlobClient.StartCopyFromURL(ctx, srcBlobURL, nil)
	if err != nil {
		// Fallback to download/upload if server-side copy fails
		logger.Warnf("Server-side copy failed, falling back to download/upload: %v", err)
		data, err := a.DownloadObject(ctx, srcKey)
		if err != nil {
			return fmt.Errorf("failed to download source blob: %w", err)
		}
		_, err = a.client.UploadBuffer(ctx, a.container, destKey, data, &azblob.UploadBufferOptions{
			HTTPHeaders: &blob.HTTPHeaders{
				BlobContentType: to.Ptr("application/octet-stream"),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to upload to destination: %w", err)
		}
	}

	// Delete the source blob
	_, err = a.client.DeleteBlob(ctx, a.container, srcKey, nil)
	if err != nil {
		return fmt.Errorf("failed to delete source blob after copy: %w", err)
	}
	return nil
}

func init() {
	logger = logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetLevel(logrus.InfoLevel) // Production-safe default log level

	// Initialize processed objects cache with TTL support
	processedObjects = make(map[string]processedObjectEntry)

	// Initialize rate limiting map
	apiRequestCounts = make(map[string][]time.Time)

	// Compile pre-signed URL validation regex
	presignedURLHostRegex = regexp.MustCompile(validPresignedURLHosts)

	// Initialize shutdown context
	shutdownCtx, shutdownCancel = context.WithCancel(context.Background())

	// Configure HTTP client with explicit TLS settings for security
	httpClient = &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			// Explicit TLS configuration - never allow insecure connections
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: false, // SECURITY: Always verify TLS certificates
			},
		},
	}

	prometheus.MustRegister(
		syncRunsTotal,
		syncErrorsTotal,
		filesProcessedTotal,
		filesFailedTotal,
		filesSkippedTotal,
		syncDurationSeconds,
		syncInProgress,
		lastSyncTimestamp,
		lastSyncSuccess,
		lastSyncFilesFound,
		lastSyncFilesProcessed,
		lastSyncFilesFailed,
		lastSyncFilesSkipped,
	)
}

func main() {
	// Load configuration
	wonderfulTenant = getEnv("WONDERFUL_TENANT", "swiss-german")
	wonderfulEnv = getEnv("WONDERFUL_ENV", "sb")
	if !isValidWonderfulEnv(wonderfulEnv) {
		logger.Fatalf("Invalid WONDERFUL_ENV: %s (allowed: dev, demo, sb, prod)", wonderfulEnv)
	}
	wonderfulAPIURL = buildWonderfulAPIURL(wonderfulTenant, wonderfulEnv)
	wonderfulRAGID = getEnv("WONDERFUL_RAG_ID", "")
	wonderfulAPIKey = getEnv("WONDERFUL_API_KEY", "")
	internalAPIKey = getEnv("INTERNAL_API_KEY", "") // Optional: for securing internal endpoints
	intervalSeconds := getEnv("SYNC_INTERVAL_SECONDS", "")
	intervalMinutes := getEnv("SYNC_INTERVAL_MINUTES", "") // Fallback for backward compatibility
	maxFileSizeMB := getEnv("MAX_FILE_SIZE_MB", "0") // 0 = no limit
	port := getEnv("PORT", "8080")

	logger.Info("=== Wonderful RAG Storage Sync Service Starting ===")
	logger.Debugf("Wonderful API URL: %s", wonderfulAPIURL)
	logger.Debugf("Wonderful tenant: %s, env: %s", wonderfulTenant, wonderfulEnv)
	logger.Debugf("Wonderful RAG ID: %s", wonderfulRAGID)

	if wonderfulRAGID == "" {
		logger.Fatal("WONDERFUL_RAG_ID is required")
	}
	if wonderfulAPIKey == "" {
		logger.Fatal("WONDERFUL_API_KEY is required")
	}

	// Parse sync interval (prefer seconds, fallback to minutes) with validation
	var interval time.Duration
	var err error
	if intervalSeconds != "" {
		interval, err = time.ParseDuration(intervalSeconds + "s")
		if err != nil {
			logger.Fatalf("Invalid SYNC_INTERVAL_SECONDS: %v", err)
		}
		logger.Infof("Sync interval set to %v (from SYNC_INTERVAL_SECONDS)", interval)
	} else if intervalMinutes != "" {
		interval, err = time.ParseDuration(intervalMinutes + "m")
		if err != nil {
			logger.Fatalf("Invalid SYNC_INTERVAL_MINUTES: %v", err)
		}
		logger.Infof("Sync interval set to %v (from SYNC_INTERVAL_MINUTES)", interval)
	} else {
		interval = 30 * time.Minute
		logger.Warnf("No sync interval specified, using default: %v", interval)
	}

	// Validate sync interval bounds
	if interval.Seconds() < float64(minSyncIntervalSeconds) {
		logger.Warnf("Sync interval %v is below minimum (%ds), using minimum", interval, minSyncIntervalSeconds)
		interval = time.Duration(minSyncIntervalSeconds) * time.Second
	}
	if interval.Seconds() > float64(maxSyncIntervalSeconds) {
		logger.Warnf("Sync interval %v exceeds maximum (%ds), using maximum", interval, maxSyncIntervalSeconds)
		interval = time.Duration(maxSyncIntervalSeconds) * time.Second
	}
	syncInterval = interval

	// Parse max file size
	if maxFileSizeMB != "0" && maxFileSizeMB != "" {
		var maxMB int64
		_, err := fmt.Sscanf(maxFileSizeMB, "%d", &maxMB)
		if err == nil && maxMB > 0 {
			maxFileSize = maxMB * 1024 * 1024 // Convert MB to bytes
			logger.Infof("Maximum file size limit: %d MB (%s)", maxMB, formatFileSize(maxFileSize))
		} else {
			logger.Warnf("Invalid MAX_FILE_SIZE_MB value '%s', ignoring limit", maxFileSizeMB)
			maxFileSize = 0
		}
	} else {
		maxFileSize = 0
		logger.Info("No file size limit configured (MAX_FILE_SIZE_MB not set or 0)")
	}

	ctx := context.Background()
	storageClient, err = initStorageClient(ctx)
	if err != nil {
		logger.Fatalf("Failed to initialize storage client: %v", err)
	}
	storageProvider = storageClient.Provider()
	storageLocation = storageClient.Location()
	storagePrefix = storageClient.Prefix()
	if storagePrefix == "" {
		logger.Debugf("Storage prefix is empty - will list from root (%s)", storageProvider)
	} else {
		logger.Debugf("Storage prefix set to '%s' (%s)", storagePrefix, storageProvider)
	}
	logger.Infof("Configuration loaded - Provider: %s, Location: %s, Prefix: %s", storageProvider, storageLocation, storagePrefix)

	// Start background sync job with graceful shutdown support
	logger.Info("Starting background sync job...")
	go func() {
		// Initial sync
		logger.Info("Performing initial sync...")
		syncMu.Lock()
		syncStorageToWonderful()
		syncMu.Unlock()

		// Periodic sync with shutdown context
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()

		logger.Infof("Starting periodic sync (interval: %v)", syncInterval)
		for {
			select {
			case <-shutdownCtx.Done():
				logger.Info("Background sync job shutting down...")
				return
			case <-ticker.C:
				// Try to acquire lock, skip if already running
				if syncMu.TryLock() {
					logger.Debugf("Sync interval reached (%v), triggering sync...", syncInterval)
					syncStorageToWonderful()
					syncMu.Unlock()
				} else {
					logger.Debug("Skipping scheduled sync - previous sync still running")
				}
			}
		}
	}()

	// Setup HTTP server
	router := setupRouter()

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: router,
	}

	// Start server in goroutine
	go func() {
		logger.Infof("Starting server on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")

	// Signal background jobs to stop
	shutdownCancel()

	// Wait for any running sync to complete (with timeout)
	syncWaitCtx, syncWaitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer syncWaitCancel()

	// Check periodically if sync has stopped
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

waitLoop:
	for {
		select {
		case <-syncWaitCtx.Done():
			logger.Warn("Timeout waiting for sync to complete, forcing shutdown...")
			break waitLoop
		case <-ticker.C:
			syncRunningMu.RLock()
			isRunning := syncRunning
			syncRunningMu.RUnlock()
			if !isRunning {
				logger.Info("Background sync completed, proceeding with shutdown")
				break waitLoop
			}
			logger.Debug("Waiting for background sync to complete...")
		}
	}

	// Shutdown HTTP server
	httpShutdownCtx, httpShutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer httpShutdownCancel()

	if err := srv.Shutdown(httpShutdownCtx); err != nil {
		logger.Fatalf("Server forced to shutdown: %v", err)
	}

	logger.Info("Server exited")
}

func initStorageClient(ctx context.Context) (StorageClient, error) {
	provider := strings.ToLower(getEnv("STORAGE_PROVIDER", "s3"))

	switch provider {
	case "s3":
		awsRegion := getEnv("AWS_REGION", "us-east-1")
		awsAccessKeyID := getEnv("AWS_ACCESS_KEY_ID", "")
		awsSecretAccessKey := getEnv("AWS_SECRET_ACCESS_KEY", "")
		bucket := getEnv("S3_BUCKET", "")
		prefix := normalizePrefix(getEnv("S3_PREFIX", ""))

		if bucket == "" {
			return nil, fmt.Errorf("S3_BUCKET is required for s3 provider")
		}

		var opts []func(*awsconfig.LoadOptions) error
		opts = append(opts, awsconfig.WithRegion(awsRegion))

		if awsAccessKeyID != "" && awsSecretAccessKey != "" {
			logger.Info("Using AWS Access Key credentials for authentication")
			// SECURITY: Do not log any credential information, even masked
			opts = append(opts, awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(awsAccessKeyID, awsSecretAccessKey, ""),
			))
		} else {
			logger.Info("No AWS credentials provided, using IAM role or default credentials chain")
		}

		cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config: %w", err)
		}

		s3Client := s3.NewFromConfig(cfg)
		downloader := manager.NewDownloader(s3Client)
		storage := &S3Storage{
			bucket:     bucket,
			prefix:     prefix,
			client:     s3Client,
			downloader: downloader,
		}

		// Test S3 connection
		if _, err := s3Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket}); err != nil {
			logger.Warnf("S3 bucket head check failed (may be expected): %v", err)
		} else {
			logger.Infof("✓ Successfully connected to S3 bucket: %s", bucket)
		}

		return storage, nil

	case "gcs", "gcp", "google":
		bucket := getEnv("GCS_BUCKET", "")
		prefix := normalizePrefix(getEnv("GCS_PREFIX", ""))
		if bucket == "" {
			return nil, fmt.Errorf("GCS_BUCKET is required for gcs provider")
		}

		client, err := storage.NewClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create GCS client: %w", err)
		}
		gcsStorage := &GCSStorage{
			bucket: bucket,
			prefix: prefix,
			client: client,
		}

		if _, err := client.Bucket(bucket).Attrs(ctx); err != nil {
			logger.Warnf("GCS bucket attribute check failed (may be expected): %v", err)
		} else {
			logger.Infof("✓ Successfully connected to GCS bucket: %s", bucket)
		}

		return gcsStorage, nil

	case "azure":
		container := getEnv("AZURE_STORAGE_CONTAINER", "")
		prefix := normalizePrefix(getEnv("AZURE_STORAGE_PREFIX", ""))
		if container == "" {
			return nil, fmt.Errorf("AZURE_STORAGE_CONTAINER is required for azure provider")
		}

		connectionString := getEnv("AZURE_STORAGE_CONNECTION_STRING", "")
		var client *azblob.Client
		var err error
		if connectionString != "" {
			client, err = azblob.NewClientFromConnectionString(connectionString, nil)
			if err != nil {
				return nil, fmt.Errorf("failed to create Azure client from connection string: %w", err)
			}
		} else {
			account := getEnv("AZURE_STORAGE_ACCOUNT", "")
			key := getEnv("AZURE_STORAGE_KEY", "")
			if account == "" || key == "" {
				return nil, fmt.Errorf("AZURE_STORAGE_ACCOUNT and AZURE_STORAGE_KEY are required unless AZURE_STORAGE_CONNECTION_STRING is set")
			}
			cred, err := azblob.NewSharedKeyCredential(account, key)
			if err != nil {
				return nil, fmt.Errorf("failed to create Azure shared key credential: %w", err)
			}
			serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", account)
			client, err = azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
			if err != nil {
				return nil, fmt.Errorf("failed to create Azure client: %w", err)
			}
		}

		azureStorage := &AzureStorage{
			container: container,
			prefix:    prefix,
			client:    client,
		}

		options := &azblob.ListBlobsFlatOptions{
			MaxResults: to.Ptr(int32(1)),
		}
		pager := client.NewListBlobsFlatPager(container, options)
		if pager.More() {
			if _, err := pager.NextPage(ctx); err != nil {
				logger.Warnf("Azure container list check failed (may be expected): %v", err)
			} else {
				logger.Infof("✓ Successfully connected to Azure container: %s", container)
			}
		} else {
			logger.Infof("✓ Successfully connected to Azure container: %s", container)
		}

		return azureStorage, nil

	default:
		return nil, fmt.Errorf("unsupported STORAGE_PROVIDER: %s", provider)
	}
}

func normalizePrefix(prefix string) string {
	return strings.Trim(prefix, "/")
}

func maskCredential(value string) string {
	if len(value) > 8 {
		return value[:4] + "..." + value[len(value)-4:]
	}
	if len(value) > 0 {
		return "***"
	}
	return ""
}

func archivePrefixes(prefix string) (string, string) {
	base := strings.Trim(prefix, "/")
	if base == "" {
		return "processed", "error"
	}
	return base + "/processed", base + "/error"
}

func isInArchive(key, archivePrefix string) bool {
	return key == archivePrefix || strings.HasPrefix(key, archivePrefix+"/")
}

func relativeKey(key, prefix string) string {
	base := strings.Trim(prefix, "/")
	if base == "" {
		return key
	}
	prefixWithSlash := base + "/"
	if strings.HasPrefix(key, prefixWithSlash) {
		return strings.TrimPrefix(key, prefixWithSlash)
	}
	return key
}

func buildArchiveKey(archivePrefix, relative string) string {
	return strings.TrimSuffix(archivePrefix, "/") + "/" + relative
}

func boolToFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// pruneProcessedObjectsCache removes old entries from the processed objects cache
// IMPORTANT: Caller must hold processedObjectsMu lock
func pruneProcessedObjectsCache() {
	now := time.Now()
	pruned := 0

	for key, entry := range processedObjects {
		// Remove entries older than TTL
		if now.Sub(entry.processedAt) > processedObjectsCacheTTL {
			delete(processedObjects, key)
			pruned++
		}
	}

	// If still too many entries after TTL pruning, remove oldest entries
	if len(processedObjects) > maxProcessedObjectsCache {
		// Find and remove the oldest entries
		type keyAge struct {
			key string
			age time.Duration
		}
		var entries []keyAge
		for key, entry := range processedObjects {
			entries = append(entries, keyAge{key: key, age: now.Sub(entry.processedAt)})
		}
		// Sort by age (oldest first) - simple bubble sort for small sets
		for i := 0; i < len(entries)-1; i++ {
			for j := i + 1; j < len(entries); j++ {
				if entries[i].age < entries[j].age {
					entries[i], entries[j] = entries[j], entries[i]
				}
			}
		}
		// Remove oldest entries until under limit
		toRemove := len(processedObjects) - maxProcessedObjectsCache + (maxProcessedObjectsCache / 10) // Remove extra 10% for headroom
		for i := 0; i < toRemove && i < len(entries); i++ {
			delete(processedObjects, entries[i].key)
			pruned++
		}
	}

	if pruned > 0 {
		logger.Debugf("Pruned %d entries from processed objects cache (current size: %d)", pruned, len(processedObjects))
	}
}

func isValidWonderfulEnv(env string) bool {
	switch env {
	case "dev", "demo", "sb", "prod":
		return true
	default:
		return false
	}
}

func buildWonderfulAPIURL(tenant, env string) string {
	return fmt.Sprintf("https://%s.api.%s.wonderful.ai", tenant, env)
}

// validatePresignedURL validates that the pre-signed URL is safe to use
// It checks that:
// 1. The URL is well-formed
// 2. The scheme is HTTPS (secure)
// 3. The host matches known cloud storage providers
func validatePresignedURL(rawURL string) error {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL format: %w", err)
	}

	// Ensure HTTPS scheme
	if parsedURL.Scheme != "https" {
		return fmt.Errorf("insecure URL scheme: expected 'https', got '%s'", parsedURL.Scheme)
	}

	// Validate host against known cloud storage providers
	host := parsedURL.Host
	if !presignedURLHostRegex.MatchString(host) {
		return fmt.Errorf("untrusted host: '%s' does not match known cloud storage providers", host)
	}

	// Additional checks for suspicious patterns
	if strings.Contains(rawURL, "..") {
		return fmt.Errorf("suspicious URL: contains path traversal pattern")
	}

	return nil
}

// sanitizeFileName extracts and validates a filename from an object key
// SECURITY: Prevents path traversal attacks and validates filename characters
func sanitizeFileName(key string) (string, error) {
	// Extract base filename
	fileName := filepath.Base(key)

	// Check for empty filename
	if fileName == "" || fileName == "." || fileName == ".." {
		return "", fmt.Errorf("invalid filename: empty or relative path component")
	}

	// Check for path traversal patterns
	if strings.Contains(fileName, "..") {
		return "", fmt.Errorf("path traversal detected in filename")
	}

	// Check for null bytes (could cause issues in C-based systems)
	if strings.Contains(fileName, "\x00") {
		return "", fmt.Errorf("null byte detected in filename")
	}

	// Check for suspicious characters that shouldn't be in filenames
	// Allow: alphanumeric, dots, hyphens, underscores, spaces
	for _, r := range fileName {
		if !isAllowedFileNameChar(r) {
			return "", fmt.Errorf("invalid character in filename: %q", r)
		}
	}

	// Limit filename length
	if len(fileName) > 255 {
		return "", fmt.Errorf("filename too long: %d characters (max 255)", len(fileName))
	}

	return fileName, nil
}

// isAllowedFileNameChar checks if a rune is allowed in filenames
func isAllowedFileNameChar(r rune) bool {
	// Allow: letters, digits, dots, hyphens, underscores, spaces, and common unicode letters
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '.', '-', '_', ' ', '(', ')', '[', ']':
		return true
	}
	// Allow unicode letters for international filenames
	if r > 127 {
		return true
	}
	return false
}

// newTraceID generates a cryptographically secure trace ID
func newTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp if crypto/rand fails (should never happen)
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func doRequestWithRetry(req *http.Request, maxAttempts int, retryableStatus func(int) bool, label string) (*http.Response, error) {
	var resp *http.Response
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err = httpClient.Do(req)
		if err == nil && resp != nil && !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		if attempt == maxAttempts {
			break
		}
		sleepWithJitter(attempt)
	}
	if err == nil && resp != nil {
		return resp, nil
	}
	return nil, fmt.Errorf("%s failed after %d attempts: %w", label, maxAttempts, err)
}

func isRetryableStatus(status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status >= 500 && status <= 599 {
		return true
	}
	return false
}

func sleepWithJitter(attempt int) {
	backoff := time.Duration(200*attempt) * time.Millisecond
	jitter := time.Duration(mathrand.Int63n(200)) * time.Millisecond
	time.Sleep(backoff + jitter)
}

func setupRouter() *gin.Engine {
	router := gin.Default()

	// Set maximum request body size to prevent DoS
	router.MaxMultipartMemory = maxRequestBodySize

	router.Use(gin.Logger(), gin.Recovery())

	// Health check (public) - limited information disclosure
	router.GET("/health", healthHandler)

	// Metrics endpoint (public - typically restricted by network policy or service mesh)
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// API routes - protected by API key authentication and rate limiting
	api := router.Group("/api/v1")
	api.Use(rateLimitMiddleware(), apiKeyAuthMiddleware())
	{
		api.POST("/sync", triggerSync)
		api.GET("/stats", getStats)
		api.GET("/processed-files", getProcessedFiles)
	}

	return router
}

// apiKeyAuthMiddleware validates API key for internal endpoints
// SECURITY: This middleware implements fail-closed authentication
func apiKeyAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// SECURITY: Fail-closed - if INTERNAL_API_KEY is not set, deny access
		if internalAPIKey == "" {
			logger.Error("INTERNAL_API_KEY not set - denying access to protected endpoint (fail-closed security)")
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Service not properly configured. API authentication is required but not configured.",
			})
			c.Abort()
			return
		}

		// SECURITY: Only accept API key via header, not query parameter
		// Query parameters are logged in access logs and visible in browser history
		providedKey := c.GetHeader("x-api-key")

		if providedKey == "" {
			logger.Warnf("API request without authentication attempted from %s", c.ClientIP())
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Missing API key. Provide 'x-api-key' header.",
			})
			c.Abort()
			return
		}

		// Constant-time comparison to prevent timing attacks
		if !secureCompare(providedKey, internalAPIKey) {
			logger.Warnf("API request with invalid API key attempted from %s", c.ClientIP())
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Invalid API key",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// rateLimitMiddleware implements basic rate limiting for API endpoints
func rateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		clientIP := c.ClientIP()
		now := time.Now()
		windowStart := now.Add(-time.Minute)

		apiRequestCountsMu.Lock()

		// Clean up old entries
		if times, exists := apiRequestCounts[clientIP]; exists {
			var validTimes []time.Time
			for _, t := range times {
				if t.After(windowStart) {
					validTimes = append(validTimes, t)
				}
			}
			apiRequestCounts[clientIP] = validTimes
		}

		// Check rate limit
		currentCount := len(apiRequestCounts[clientIP])
		if currentCount >= apiRateLimitPerMinute {
			apiRequestCountsMu.Unlock()
			logger.Warnf("Rate limit exceeded for client %s: %d requests in last minute", clientIP, currentCount)
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       "Rate limit exceeded",
				"retry_after": "60",
			})
			c.Abort()
			return
		}

		// Record this request
		apiRequestCounts[clientIP] = append(apiRequestCounts[clientIP], now)
		apiRequestCountsMu.Unlock()

		c.Next()
	}
}

// secureCompare performs constant-time string comparison using crypto/subtle
// to prevent timing attacks. This is the recommended approach for comparing secrets.
func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func healthHandler(c *gin.Context) {
	// SECURITY: Limit information disclosed in health endpoint
	// Detailed config info should only be exposed to authenticated users
	c.JSON(http.StatusOK, gin.H{
		"status":  "healthy",
		"service": "wonderful-rag-sync",
	})
}

func triggerSync(c *gin.Context) {
	// Check if sync is already running
	syncRunningMu.RLock()
	isRunning := syncRunning
	syncRunningMu.RUnlock()

	if isRunning {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "Sync already in progress",
			"status":  "running",
			"message": "Please wait for the current sync to complete before triggering another",
		})
		return
	}

	// Try to acquire the sync lock
	if !syncMu.TryLock() {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "Sync already in progress",
			"status":  "running",
			"message": "Another sync operation is starting",
		})
		return
	}

	// Start sync in background
	go func() {
		defer syncMu.Unlock()
		syncStorageToWonderful()
	}()

	c.JSON(http.StatusOK, gin.H{
		"message": "Sync triggered",
		"status":  "started",
	})
}

func getStats(c *gin.Context) {
	processedObjectsMu.RLock()
	count := len(processedObjects)
	processedObjectsMu.RUnlock()

	c.JSON(http.StatusOK, gin.H{
		"processed_files":       count,
		"storage_provider":      storageProvider,
		"storage_location":      storageLocation,
		"storage_prefix":        storagePrefix,
		"sync_interval_seconds": syncInterval.Seconds(),
	})
}

func getProcessedFiles(c *gin.Context) {
	processedObjectsMu.RLock()
	files := make([]string, 0, len(processedObjects))
	for file := range processedObjects {
		files = append(files, file)
	}
	processedObjectsMu.RUnlock()

	c.JSON(http.StatusOK, gin.H{
		"files": files,
		"count": len(files),
	})
}

func syncStorageToWonderful() {
	// Mark sync as running
	syncRunningMu.Lock()
	syncRunning = true
	syncRunningMu.Unlock()
	defer func() {
		syncRunningMu.Lock()
		syncRunning = false
		syncRunningMu.Unlock()
	}()

	logger.Info("=== Starting Storage to Wonderful API Sync ===")
	syncStartTime := time.Now()
	providerLabel := storageProvider
	syncRunsTotal.WithLabelValues(providerLabel).Inc()
	syncInProgress.WithLabelValues(providerLabel).Set(1)
	defer syncInProgress.WithLabelValues(providerLabel).Set(0)

	// Use shutdown context for graceful cancellation
	ctx, cancel := context.WithCancel(shutdownCtx)
	defer cancel()
	logger.Debugf("Preparing to list objects from %s (%s)", storageProvider, storageLocation)

	objects, err := storageClient.ListObjects(ctx)
	if err != nil {
		logger.Errorf("✗ Error listing objects: %v", err)
		syncErrorsTotal.WithLabelValues(providerLabel).Inc()
		return
	}

	successCount := 0
	errorCount := 0
	skippedCount := 0
	totalFilesFound := 0
	errors := []string{}
	processedPrefix, errorPrefix := archivePrefixes(storagePrefix)
	logger.Infof("Archive prefixes - processed: %s, error: %s", processedPrefix, errorPrefix)

	logger.Infof("Scanning %s for files...", storageProvider)

	for _, obj := range objects {
		key := obj.Key
		if isInArchive(key, processedPrefix) || isInArchive(key, errorPrefix) {
			logger.Debugf("Skipping archived file: %s", key)
			filesSkippedTotal.WithLabelValues(providerLabel).Inc()
			continue
		}
		size := obj.Size
		totalFilesFound++

		// Format file size for logging
		sizeStr := formatFileSize(size)
		logger.Debugf("Found file: %s (size: %s, modified: %v)", key, sizeStr, obj.LastModified)

		// Check file size limit
		if maxFileSize > 0 && size > maxFileSize {
			logger.Warnf("⚠️  Skipping file %s: size %s exceeds maximum allowed size %s", key, sizeStr, formatFileSize(maxFileSize))
			errorCount++
			errors = append(errors, fmt.Sprintf("%s: file too large (%s > %s)", key, sizeStr, formatFileSize(maxFileSize)))
			skippedCount++
			filesSkippedTotal.WithLabelValues(providerLabel).Inc()
			continue
		}

		logger.Infof("📄 Processing file: %s (size: %s)", key, sizeStr)

		// Download file from storage provider
		logger.Debugf("  → Downloading from %s: %s", storageProvider, key)
		downloadStart := time.Now()
		fileContent, err := storageClient.DownloadObject(ctx, key)
		downloadDuration := time.Since(downloadStart)

		if err != nil {
			logger.Errorf("  ✗ Failed to download %s: %v", key, err)
			errorCount++
			errors = append(errors, fmt.Sprintf("%s: download failed - %v", key, err))
			filesFailedTotal.WithLabelValues(providerLabel).Inc()
			continue
		}
		logger.Debugf("  ✓ Downloaded %d bytes in %v", len(fileContent), downloadDuration)

		// Upload to Wonderful API
		// SECURITY: Extract and sanitize filename to prevent path traversal
		fileName, err := sanitizeFileName(key)
		if err != nil {
			logger.Errorf("  ✗ Invalid filename in key %s: %v", key, err)
			errorCount++
			errors = append(errors, fmt.Sprintf("%s: invalid filename - %v", key, err))
			filesFailedTotal.WithLabelValues(providerLabel).Inc()
			continue
		}

		logger.Infof("  → Starting upload and attach process for: %s", key)
		uploadStart := time.Now()

		// uploadToWonderful performs both:
		// 1. Upload request (get pre-signed URL and upload to S3)
		// 2. Attach request (attach uploaded file to RAG)
		fileID, err := uploadToWonderful(ctx, fileName, fileContent, key)
		uploadDuration := time.Since(uploadStart)

		if err != nil {
			logger.Errorf("  ✗ Failed to process %s (upload or attach failed): %v", key, err)
			errorCount++
			errors = append(errors, fmt.Sprintf("%s: processing failed - %v", key, err))
			filesFailedTotal.WithLabelValues(providerLabel).Inc()
			archiveKey := buildArchiveKey(errorPrefix, relativeKey(key, storagePrefix))
			if moveErr := storageClient.MoveObject(ctx, key, archiveKey); moveErr != nil {
				logger.Warnf("  ⚠ Failed to move %s to error folder: %v", key, moveErr)
				syncErrorsTotal.WithLabelValues(providerLabel).Inc()
			} else {
				logger.Infof("  ↪ Moved failed file to %s", archiveKey)
			}
			continue
		}

		// Only move after BOTH upload and attach succeeded
		archiveKey := buildArchiveKey(processedPrefix, relativeKey(key, storagePrefix))
		if moveErr := storageClient.MoveObject(ctx, key, archiveKey); moveErr != nil {
			logger.Warnf("  ⚠ Failed to move %s to processed folder: %v", key, moveErr)
			syncErrorsTotal.WithLabelValues(providerLabel).Inc()
		} else {
			logger.Infof("  ↪ Moved processed file to %s", archiveKey)
		}
		// Track processed object with timestamp for TTL-based pruning
		processedObjectsMu.Lock()
		processedObjects[key] = processedObjectEntry{processedAt: time.Now()}
		// Prune old entries if cache is getting too large
		if len(processedObjects) > maxProcessedObjectsCache {
			pruneProcessedObjectsCache()
		}
		processedObjectsMu.Unlock()

		logger.Infof("  ✓ Successfully processed %s (uploaded and attached to RAG, file ID: %s, took %v)", key, fileID, uploadDuration)
		successCount++
		filesProcessedTotal.WithLabelValues(providerLabel).Inc()
	}

	syncDuration := time.Since(syncStartTime)
	syncDurationSeconds.WithLabelValues(providerLabel).Observe(syncDuration.Seconds())
	lastSyncTimestamp.WithLabelValues(providerLabel).Set(float64(time.Now().Unix()))
	lastSyncSuccess.WithLabelValues(providerLabel).Set(boolToFloat(errorCount == 0))
	lastSyncFilesFound.WithLabelValues(providerLabel).Set(float64(totalFilesFound))
	lastSyncFilesProcessed.WithLabelValues(providerLabel).Set(float64(successCount))
	lastSyncFilesFailed.WithLabelValues(providerLabel).Set(float64(errorCount))
	lastSyncFilesSkipped.WithLabelValues(providerLabel).Set(float64(skippedCount))
	logger.Info("=== Sync Completed ===")
	logger.Infof("Summary:")
	logger.Infof("  Total files found: %d", totalFilesFound)
	logger.Infof("  Successfully processed: %d", successCount)
	logger.Infof("  Skipped (already processed): %d", skippedCount)
	logger.Infof("  Failed: %d", errorCount)
	logger.Infof("  Duration: %v", syncDuration)

	if len(errors) > 0 {
		logger.Warnf("Errors encountered:")
		for i, errMsg := range errors {
			logger.Warnf("  %d. %s", i+1, errMsg)
		}
	}
}

func uploadToWonderful(ctx context.Context, fileName string, fileContent []byte, s3Key string) (string, error) {
	logger.Debugf("    Preparing upload to Wonderful API...")
	logger.Debugf("    File name: %s, Size: %d bytes", fileName, len(fileContent))
	traceID := newTraceID()

	// Step 1: Get pre-signed S3 URL from Wonderful API
	storageURL := fmt.Sprintf("%s/api/v1/storage", wonderfulAPIURL)
	logger.Debugf("    Step 1: Requesting pre-signed S3 URL from: %s", storageURL)

	// Determine content type from file extension
	contentType := "application/octet-stream"
	if strings.HasSuffix(strings.ToLower(fileName), ".json") {
		contentType = "application/json"
	} else if strings.HasSuffix(strings.ToLower(fileName), ".jpg") || strings.HasSuffix(strings.ToLower(fileName), ".jpeg") {
		contentType = "image/jpeg"
	} else if strings.HasSuffix(strings.ToLower(fileName), ".png") {
		contentType = "image/png"
	} else if strings.HasSuffix(strings.ToLower(fileName), ".pdf") {
		contentType = "application/pdf"
	} else if strings.HasSuffix(strings.ToLower(fileName), ".txt") {
		contentType = "text/plain"
	}

	// Create JSON payload for storage request
	storagePayload := map[string]interface{}{
		"contentType": contentType,
		"filename":    fileName,
	}

	storageBody, err := json.Marshal(storagePayload)
	if err != nil {
		logger.Errorf("    ✗ Failed to marshal storage request: %v", err)
		return "", fmt.Errorf("failed to marshal storage request: %w", err)
	}

	logger.Debugf("    ✓ Storage request prepared for file: %s", fileName)

	// Try POST method first (as per API spec)
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", storageURL, bytes.NewBuffer(storageBody))
	if err != nil {
		logger.Errorf("    ✗ Failed to create storage request: %v", err)
		return "", fmt.Errorf("failed to create storage request: %w", err)
	}

	req.Header.Set("x-api-key", wonderfulAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "s3-to-wonderful-rag-sync/1.0")
	req.Header.Set("X-Request-Id", traceID)

	resp, err := doRequestWithRetry(req, 3, isRetryableStatus, "storage request")
	if err != nil {
		logger.Errorf("    ✗ Storage request failed: %v", err)
		return "", fmt.Errorf("failed to get pre-signed URL: %w", err)
	}
	defer resp.Body.Close()

	logger.Debugf("    ✓ Received storage response: Status %d %s", resp.StatusCode, resp.Status)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Errorf("    ✗ Failed to read storage response: %v", err)
		return "", fmt.Errorf("failed to read storage response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		logger.Errorf("    ✗ Storage API returned error status %d: %s", resp.StatusCode, string(respBody))
		return "", fmt.Errorf("storage API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response to get pre-signed URL and file_id
	// Response format: {"data": {"id": "...", "url": "..."}, "status": 200}
	var storageResult map[string]interface{}
	if err := json.Unmarshal(respBody, &storageResult); err != nil {
		logger.Errorf("    ✗ Failed to parse storage response: %v", err)
		return "", fmt.Errorf("failed to parse storage response: %w", err)
	}

	// Extract data object
	dataObj, ok := storageResult["data"].(map[string]interface{})
	if !ok {
		logger.Errorf("    ✗ No 'data' field in storage response: %s", string(respBody))
		return "", fmt.Errorf("no 'data' field in storage response")
	}

	// Get pre-signed URL from data.url
	presignedURL, ok := dataObj["url"].(string)
	if !ok || presignedURL == "" {
		logger.Errorf("    ✗ No 'url' in data object: %s", string(respBody))
		return "", fmt.Errorf("no 'url' in storage response data")
	}

	// SECURITY: Validate the pre-signed URL before using it
	if err := validatePresignedURL(presignedURL); err != nil {
		logger.Errorf("    ✗ Pre-signed URL validation failed: %v", err)
		return "", fmt.Errorf("pre-signed URL validation failed: %w", err)
	}

	// Get file ID from data.id
	fileID, ok := dataObj["id"].(string)
	if !ok || fileID == "" {
		logger.Errorf("    ✗ No 'id' in data object: %s", string(respBody))
		return "", fmt.Errorf("no 'id' in storage response data")
	}

	logger.Debugf("    ✓ Got file_id: %s", fileID)
	logger.Debugf("    ✓ Got pre-signed URL (length: %d)", len(presignedURL))

	// Step 2: Upload file directly to S3 using pre-signed URL
	logger.Infof("    Step 2: Uploading file to S3 using pre-signed URL...")

	uploadCtx, uploadCancel := context.WithTimeout(ctx, 60*time.Second)
	defer uploadCancel()
	uploadReq, err := http.NewRequestWithContext(uploadCtx, "PUT", presignedURL, bytes.NewReader(fileContent))
	if err != nil {
		logger.Errorf("    ✗ Failed to create S3 upload request: %v", err)
		return "", fmt.Errorf("failed to create S3 upload request: %w", err)
	}

	// Use the content type we determined earlier (or from storage response if provided)
	uploadContentType := contentType
	if respContentType, ok := storageResult["content_type"].(string); ok && respContentType != "" {
		uploadContentType = respContentType
	}
	uploadReq.Header.Set("Content-Type", uploadContentType)
	logger.Debugf("    ✓ Using Content-Type for S3 upload: %s", uploadContentType)

	uploadResp, err := doRequestWithRetry(uploadReq, 2, isRetryableStatus, "S3 upload")
	if err != nil {
		logger.Errorf("    ✗ S3 upload failed: %v", err)
		return "", fmt.Errorf("failed to upload to S3: %w", err)
	}
	defer uploadResp.Body.Close()

	logger.Debugf("    ✓ S3 upload response: Status %d %s", uploadResp.StatusCode, uploadResp.Status)

	if uploadResp.StatusCode != http.StatusOK && uploadResp.StatusCode != http.StatusNoContent {
		uploadRespBody, _ := io.ReadAll(uploadResp.Body)
		logger.Errorf("    ✗ S3 upload returned error status %d: %s", uploadResp.StatusCode, string(uploadRespBody))
		return "", fmt.Errorf("S3 upload returned status %d", uploadResp.StatusCode)
	}

	logger.Infof("    ✓ File uploaded to S3 successfully (Step 2 complete)")

	// fileID should already be set from storage response data.id
	if fileID == "" {
		logger.Warnf("    ⚠ No file_id from storage response, using S3 key as fallback")
		fileID = s3Key
	}

	// Step 3: Attach file to RAG using file_ids
	// IMPORTANT: This step must complete successfully for the file to be considered processed
	attachURL := fmt.Sprintf("%s/api/v1/rags/%s/files", wonderfulAPIURL, wonderfulRAGID)
	logger.Infof("    Step 3: Attaching uploaded file to RAG: %s", attachURL)

	// Create JSON request body with file_ids
	requestBody := map[string]interface{}{
		"file_ids": []string{fileID},
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		logger.Errorf("    ✗ Failed to marshal JSON: %v", err)
		return "", fmt.Errorf("failed to marshal request: %w", err)
	}

	logger.Debugf("    ✓ Attachment request prepared for file_id: %s", fileID)

	// Create attachment request
	attachCtx, attachCancel := context.WithTimeout(ctx, 15*time.Second)
	defer attachCancel()
	attachReq, err := http.NewRequestWithContext(attachCtx, "POST", attachURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		logger.Errorf("    ✗ Failed to create attachment request: %v", err)
		return "", fmt.Errorf("failed to create attachment request: %w", err)
	}

	attachReq.Header.Set("Content-Type", "application/json")
	attachReq.Header.Set("x-api-key", wonderfulAPIKey)
	attachReq.Header.Set("User-Agent", "s3-to-wonderful-rag-sync/1.0")
	attachReq.Header.Set("X-Request-Id", traceID)
	logger.Debugf("    ✓ Attachment request created with Content-Type: application/json")

	// Execute attachment request
	attachResp, err := doRequestWithRetry(attachReq, 3, isRetryableStatus, "attach request")
	if err != nil {
		logger.Errorf("    ✗ Attachment request failed: %v", err)
		return "", fmt.Errorf("failed to execute attachment request: %w", err)
	}
	defer attachResp.Body.Close()

	logger.Debugf("    ✓ Received attachment response: Status %d %s", attachResp.StatusCode, attachResp.Status)

	attachRespBody, err := io.ReadAll(attachResp.Body)
	if err != nil {
		logger.Errorf("    ✗ Failed to read attachment response body: %v", err)
		return "", fmt.Errorf("failed to read attachment response: %w", err)
	}

	if attachResp.StatusCode != http.StatusOK && attachResp.StatusCode != http.StatusCreated {
		errorMsg := string(attachRespBody)
		if attachResp.StatusCode == http.StatusRequestEntityTooLarge {
			logger.Errorf("    ✗ File too large (413 Request Entity Too Large): %s", errorMsg)
			return "", fmt.Errorf("file too large for API (413): file size may exceed server limits")
		}
		logger.Errorf("    ✗ API returned error status %d: %s", attachResp.StatusCode, errorMsg)
		return "", fmt.Errorf("API returned status %d: %s", attachResp.StatusCode, errorMsg)
	}

	logger.Infof("    ✓ File attached to RAG successfully (Step 3 complete)")
	logger.Debugf("    Attachment response: %s", string(attachRespBody))

	// Both upload (Step 2) and attach (Step 3) completed successfully
	logger.Infof("    ✓ Complete: File uploaded and attached to RAG (file_id: %s)", fileID)

	return fileID, nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func formatFileSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
