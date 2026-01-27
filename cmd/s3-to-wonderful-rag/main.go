package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
)

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
	processedObjects   map[string]bool
	processedObjectsMu sync.RWMutex

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
	client     *s3.S3
	downloader *s3manager.Downloader
}

func (s *S3Storage) Provider() string { return "s3" }
func (s *S3Storage) Location() string { return s.bucket }
func (s *S3Storage) Prefix() string   { return s.prefix }

func (s *S3Storage) ListObjects(ctx context.Context) ([]ObjectInfo, error) {
	listInput := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
	}
	if s.prefix != "" {
		listInput.Prefix = aws.String(s.prefix)
	}

	objects := []ObjectInfo{}
	err := s.client.ListObjectsV2PagesWithContext(ctx, listInput, func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		for _, obj := range page.Contents {
			if obj == nil || obj.Key == nil || obj.Size == nil {
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
		return true
	})
	if err != nil {
		return nil, err
	}
	return objects, nil
}

func (s *S3Storage) DownloadObject(ctx context.Context, key string) ([]byte, error) {
	buf := aws.NewWriteAtBuffer([]byte{})
	_, err := s.downloader.DownloadWithContext(ctx, buf, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *S3Storage) MoveObject(ctx context.Context, srcKey, destKey string) error {
	if srcKey == destKey {
		return nil
	}
	_, err := s.client.CopyObjectWithContext(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(destKey),
		CopySource: aws.String(url.PathEscape(s.bucket + "/" + srcKey)),
	})
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(srcKey),
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
			return nil, err
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
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
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
			return nil, err
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
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (a *AzureStorage) MoveObject(ctx context.Context, srcKey, destKey string) error {
	if srcKey == destKey {
		return nil
	}
	data, err := a.DownloadObject(ctx, srcKey)
	if err != nil {
		return err
	}
	_, err = a.client.UploadBuffer(ctx, a.container, destKey, data, &azblob.UploadBufferOptions{
		HTTPHeaders: &blob.HTTPHeaders{
			BlobContentType: to.Ptr("application/octet-stream"),
		},
	})
	if err != nil {
		return err
	}
	_, err = a.client.DeleteBlob(ctx, a.container, srcKey, nil)
	return err
}

func init() {
	logger = logrus.New()
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetLevel(logrus.DebugLevel) // Verbose logging
	processedObjects = make(map[string]bool)
	rand.Seed(time.Now().UnixNano())

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

	// Parse sync interval (prefer seconds, fallback to minutes)
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

	// Start background sync job
	logger.Info("Starting background sync job...")
	go func() {
		// Initial sync
		logger.Info("Performing initial sync...")
		syncStorageToWonderful()

		// Periodic sync
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()

		logger.Infof("Starting periodic sync (interval: %v)", syncInterval)
		for range ticker.C {
			logger.Debugf("Sync interval reached (%v), triggering sync...", syncInterval)
			syncStorageToWonderful()
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
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

		awsConfig := &aws.Config{
			Region: aws.String(awsRegion),
		}
		if awsAccessKeyID != "" && awsSecretAccessKey != "" {
			logger.Info("Using AWS Access Key credentials for authentication")
			logger.Debugf("AWS Access Key ID: %s (length: %d)", maskCredential(awsAccessKeyID), len(awsAccessKeyID))
			awsConfig.Credentials = credentials.NewStaticCredentials(
				awsAccessKeyID,
				awsSecretAccessKey,
				"",
			)
		} else {
			logger.Info("No AWS credentials provided, using IAM role or default credentials chain")
		}

		awsSession, err := session.NewSession(awsConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create AWS session: %w", err)
		}

		s3Client := s3.New(awsSession)
		downloader := s3manager.NewDownloader(awsSession)
		storage := &S3Storage{
			bucket:     bucket,
			prefix:     prefix,
			client:     s3Client,
			downloader: downloader,
		}

		// Test S3 connection
		if _, err := s3Client.HeadBucket(&s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
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

func newTraceID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Int63())
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
	jitter := time.Duration(rand.Int63n(200)) * time.Millisecond
	time.Sleep(backoff + jitter)
}

func setupRouter() *gin.Engine {
	router := gin.Default()
	router.Use(gin.Logger(), gin.Recovery())

	// Health check (public)
	router.GET("/health", healthHandler)

	// Metrics endpoint (public - typically restricted by network policy or service mesh)
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// API routes - protected by API key authentication
	api := router.Group("/api/v1")
	api.Use(apiKeyAuthMiddleware())
	{
		api.POST("/sync", triggerSync)
		api.GET("/stats", getStats)
		api.GET("/processed-files", getProcessedFiles)
	}

	return router
}

// apiKeyAuthMiddleware validates API key for internal endpoints
func apiKeyAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// If INTERNAL_API_KEY is not set, log warning but allow access (backward compatibility)
		if internalAPIKey == "" {
			logger.Warn("INTERNAL_API_KEY not set - API endpoints are not protected. This is a security risk.")
			c.Next()
			return
		}

		// Check for API key in header (x-api-key) or query parameter (api_key)
		providedKey := c.GetHeader("x-api-key")
		if providedKey == "" {
			providedKey = c.Query("api_key")
		}

		if providedKey == "" {
			logger.Warn("API request without authentication attempted")
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Missing API key. Provide x-api-key header or api_key query parameter.",
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

// secureCompare performs constant-time string comparison
func secureCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	result := 0
	for i := 0; i < len(a); i++ {
		result |= int(a[i]) ^ int(b[i])
	}
	return result == 0
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":            "healthy",
		"service":           "wonderful-rag-sync",
		"storage_provider":  storageProvider,
		"storage_location":  storageLocation,
		"storage_prefix":    storagePrefix,
		"wonderful_api_url": wonderfulAPIURL,
	})
}

func triggerSync(c *gin.Context) {
	go syncStorageToWonderful()
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
	logger.Info("=== Starting Storage to Wonderful API Sync ===")
	syncStartTime := time.Now()
	providerLabel := storageProvider
	syncRunsTotal.WithLabelValues(providerLabel).Inc()
	syncInProgress.WithLabelValues(providerLabel).Set(1)
	defer syncInProgress.WithLabelValues(providerLabel).Set(0)

	ctx := context.Background()
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
		fileName := key
		if lastSlash := strings.LastIndex(key, "/"); lastSlash >= 0 {
			fileName = key[lastSlash+1:]
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
		processedObjectsMu.Lock()
		processedObjects[key] = true
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

	logger.Debugf("    ✓ Storage request payload: %s", string(storageBody))

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

	logger.Debugf("    ✓ JSON request body: %s", string(jsonBody))

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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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
