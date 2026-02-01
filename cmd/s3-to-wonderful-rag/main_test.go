package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

func init() {
	// Initialize logger for tests
	logger = logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // Suppress log output in tests

	// Initialize maps
	processedObjects = make(map[string]processedObjectEntry)
	apiRequestCounts = make(map[string][]time.Time)

	// Initialize shutdown context
	shutdownCtx, shutdownCancel = context.WithCancel(context.Background())
}

// TestNormalizePrefix tests the normalizePrefix helper function
func TestNormalizePrefix(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty string", "", ""},
		{"no slashes", "prefix", "prefix"},
		{"leading slash", "/prefix", "prefix"},
		{"trailing slash", "prefix/", "prefix"},
		{"both slashes", "/prefix/", "prefix"},
		{"multiple slashes", "///prefix///", "prefix"},
		{"nested path", "folder/subfolder", "folder/subfolder"},
		{"nested with slashes", "/folder/subfolder/", "folder/subfolder"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizePrefix(tt.input)
			if result != tt.expected {
				t.Errorf("normalizePrefix(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestArchivePrefixes tests the archivePrefixes helper function
func TestArchivePrefixes(t *testing.T) {
	tests := []struct {
		name              string
		prefix            string
		expectedProcessed string
		expectedError     string
	}{
		{"empty prefix", "", "processed", "error"},
		{"simple prefix", "data", "data/processed", "data/error"},
		{"prefix with slash", "data/", "data/processed", "data/error"},
		{"nested prefix", "uploads/docs", "uploads/docs/processed", "uploads/docs/error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processed, errPrefix := archivePrefixes(tt.prefix)
			if processed != tt.expectedProcessed {
				t.Errorf("archivePrefixes(%q) processed = %q, want %q", tt.prefix, processed, tt.expectedProcessed)
			}
			if errPrefix != tt.expectedError {
				t.Errorf("archivePrefixes(%q) error = %q, want %q", tt.prefix, errPrefix, tt.expectedError)
			}
		})
	}
}

// TestIsInArchive tests the isInArchive helper function
func TestIsInArchive(t *testing.T) {
	tests := []struct {
		name          string
		key           string
		archivePrefix string
		expected      bool
	}{
		{"exact match", "processed", "processed", true},
		{"in archive folder", "processed/file.txt", "processed", true},
		{"not in archive", "data/file.txt", "processed", false},
		{"prefix mismatch", "processed_old/file.txt", "processed", false},
		{"nested archive", "data/processed/file.txt", "data/processed", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isInArchive(tt.key, tt.archivePrefix)
			if result != tt.expected {
				t.Errorf("isInArchive(%q, %q) = %v, want %v", tt.key, tt.archivePrefix, result, tt.expected)
			}
		})
	}
}

// TestRelativeKey tests the relativeKey helper function
func TestRelativeKey(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		prefix   string
		expected string
	}{
		{"empty prefix", "file.txt", "", "file.txt"},
		{"with prefix", "data/file.txt", "data", "file.txt"},
		{"nested key", "data/folder/file.txt", "data", "folder/file.txt"},
		{"key without prefix", "other/file.txt", "data", "other/file.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := relativeKey(tt.key, tt.prefix)
			if result != tt.expected {
				t.Errorf("relativeKey(%q, %q) = %q, want %q", tt.key, tt.prefix, result, tt.expected)
			}
		})
	}
}

// TestBuildArchiveKey tests the buildArchiveKey helper function
func TestBuildArchiveKey(t *testing.T) {
	tests := []struct {
		name          string
		archivePrefix string
		relative      string
		expected      string
	}{
		{"simple", "processed", "file.txt", "processed/file.txt"},
		{"with trailing slash", "processed/", "file.txt", "processed/file.txt"},
		{"nested", "data/processed", "folder/file.txt", "data/processed/folder/file.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildArchiveKey(tt.archivePrefix, tt.relative)
			if result != tt.expected {
				t.Errorf("buildArchiveKey(%q, %q) = %q, want %q", tt.archivePrefix, tt.relative, result, tt.expected)
			}
		})
	}
}

// TestBoolToFloat tests the boolToFloat helper function
func TestBoolToFloat(t *testing.T) {
	if boolToFloat(true) != 1 {
		t.Error("boolToFloat(true) should return 1")
	}
	if boolToFloat(false) != 0 {
		t.Error("boolToFloat(false) should return 0")
	}
}

// TestIsValidWonderfulEnv tests the isValidWonderfulEnv helper function
func TestIsValidWonderfulEnv(t *testing.T) {
	validEnvs := []string{"dev", "demo", "sb", "prod"}
	invalidEnvs := []string{"", "staging", "production", "DEV", "Prod"}

	for _, env := range validEnvs {
		if !isValidWonderfulEnv(env) {
			t.Errorf("isValidWonderfulEnv(%q) should return true", env)
		}
	}

	for _, env := range invalidEnvs {
		if isValidWonderfulEnv(env) {
			t.Errorf("isValidWonderfulEnv(%q) should return false", env)
		}
	}
}

// TestBuildWonderfulAPIURL tests the buildWonderfulAPIURL helper function
func TestBuildWonderfulAPIURL(t *testing.T) {
	tests := []struct {
		tenant   string
		env      string
		expected string
	}{
		{"swiss-german", "sb", "https://swiss-german.api.sb.wonderful.ai"},
		{"acme", "prod", "https://acme.api.prod.wonderful.ai"},
		{"test-company", "dev", "https://test-company.api.dev.wonderful.ai"},
	}

	for _, tt := range tests {
		t.Run(tt.tenant+"_"+tt.env, func(t *testing.T) {
			result := buildWonderfulAPIURL(tt.tenant, tt.env)
			if result != tt.expected {
				t.Errorf("buildWonderfulAPIURL(%q, %q) = %q, want %q", tt.tenant, tt.env, result, tt.expected)
			}
		})
	}
}

// TestNewTraceID tests that newTraceID generates valid trace IDs
func TestNewTraceID(t *testing.T) {
	seen := make(map[string]bool)

	for i := 0; i < 100; i++ {
		id := newTraceID()

		// Check that it's not empty
		if id == "" {
			t.Error("newTraceID() returned empty string")
		}

		// Check that it's a valid hex string (32 chars for 16 bytes)
		if len(id) != 32 {
			t.Errorf("newTraceID() returned %q with length %d, expected 32", id, len(id))
		}

		// Check uniqueness
		if seen[id] {
			t.Errorf("newTraceID() generated duplicate ID: %q", id)
		}
		seen[id] = true
	}
}

// TestSecureCompare tests the secureCompare function
func TestSecureCompare(t *testing.T) {
	tests := []struct {
		name     string
		a        string
		b        string
		expected bool
	}{
		{"equal strings", "password123", "password123", true},
		{"different strings", "password123", "password124", false},
		{"different lengths", "short", "longer", false},
		{"empty strings", "", "", true},
		{"one empty", "test", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := secureCompare(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("secureCompare(%q, %q) = %v, want %v", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

// TestValidatePresignedURL tests the validatePresignedURL function
func TestValidatePresignedURL(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		shouldError bool
	}{
		{"valid S3 URL", "https://bucket.s3.us-east-1.amazonaws.com/key?signature=xxx", false},
		{"valid S3 URL without region", "https://bucket.s3.amazonaws.com/key", false},
		{"valid GCS URL", "https://storage.googleapis.com/bucket/key", false},
		{"valid Azure URL", "https://account.blob.core.windows.net/container/blob", false},
		{"HTTP instead of HTTPS", "http://bucket.s3.amazonaws.com/key", true},
		{"untrusted host", "https://evil.com/key", true},
		{"path traversal", "https://bucket.s3.amazonaws.com/../../../etc/passwd", true},
		{"invalid URL", "not a url", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePresignedURL(tt.url)
			if tt.shouldError && err == nil {
				t.Errorf("validatePresignedURL(%q) should have returned error", tt.url)
			}
			if !tt.shouldError && err != nil {
				t.Errorf("validatePresignedURL(%q) returned unexpected error: %v", tt.url, err)
			}
		})
	}
}

// TestSanitizeFileName tests the sanitizeFileName function
func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		expected    string
		shouldError bool
	}{
		{"simple filename", "document.pdf", "document.pdf", false},
		{"filename from path", "folder/subfolder/document.pdf", "document.pdf", false},
		{"filename with spaces", "my document.pdf", "my document.pdf", false},
		{"filename with underscores", "my_document_v2.pdf", "my_document_v2.pdf", false},
		{"filename with hyphens", "my-document-v2.pdf", "my-document-v2.pdf", false},
		// filepath.Base extracts just the filename, so "../../../etc/passwd" becomes "passwd"
		{"path traversal - extracts filename", "../../../etc/passwd", "passwd", false},
		{"double dot filename", "..", "", true},
		{"single dot filename", ".", "", true},
		// filepath.Base of "folder/" is "folder" which is valid
		{"folder path", "folder/", "folder", false},
		{"null byte", "file\x00.txt", "", true},
		{"special chars", "file<>:\"|?*.txt", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := sanitizeFileName(tt.key)
			if tt.shouldError {
				if err == nil {
					t.Errorf("sanitizeFileName(%q) should have returned error", tt.key)
				}
			} else {
				if err != nil {
					t.Errorf("sanitizeFileName(%q) returned unexpected error: %v", tt.key, err)
				}
				if result != tt.expected {
					t.Errorf("sanitizeFileName(%q) = %q, want %q", tt.key, result, tt.expected)
				}
			}
		})
	}
}

// TestFormatFileSize tests the formatFileSize helper function
func TestFormatFileSize(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1048576, "1.00 MB"},
		{1073741824, "1.00 GB"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := formatFileSize(tt.bytes)
			if result != tt.expected {
				t.Errorf("formatFileSize(%d) = %q, want %q", tt.bytes, result, tt.expected)
			}
		})
	}
}

// TestIsRetryableStatus tests the isRetryableStatus function
func TestIsRetryableStatus(t *testing.T) {
	retryableCodes := []int{429, 500, 502, 503, 504}
	nonRetryableCodes := []int{200, 201, 400, 401, 403, 404}

	for _, code := range retryableCodes {
		if !isRetryableStatus(code) {
			t.Errorf("isRetryableStatus(%d) should return true", code)
		}
	}

	for _, code := range nonRetryableCodes {
		if isRetryableStatus(code) {
			t.Errorf("isRetryableStatus(%d) should return false", code)
		}
	}
}

// TestHealthHandler tests the health endpoint
func TestHealthHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/health", healthHandler)

	req, _ := http.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Health endpoint returned %d, want %d", w.Code, http.StatusOK)
	}

	// Check that response contains expected fields
	body := w.Body.String()
	if body == "" {
		t.Error("Health endpoint returned empty body")
	}
}

// TestAPIKeyAuthMiddleware tests the API key authentication middleware
func TestAPIKeyAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		internalAPIKey string
		providedKey    string
		expectedStatus int
	}{
		{"no key configured - deny", "", "", http.StatusServiceUnavailable},
		{"no key provided", "secret123", "", http.StatusUnauthorized},
		{"wrong key", "secret123", "wrongkey", http.StatusForbidden},
		{"correct key", "secret123", "secret123", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set the internal API key
			internalAPIKey = tt.internalAPIKey

			router := gin.New()
			router.Use(apiKeyAuthMiddleware())
			router.GET("/test", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"status": "ok"})
			})

			req, _ := http.NewRequest("GET", "/test", nil)
			if tt.providedKey != "" {
				req.Header.Set("x-api-key", tt.providedKey)
			}

			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("API key auth returned %d, want %d", w.Code, tt.expectedStatus)
			}
		})
	}
}

// TestRateLimitMiddleware tests the rate limiting middleware
func TestRateLimitMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Reset rate limit map
	apiRequestCountsMu.Lock()
	apiRequestCounts = make(map[string][]time.Time)
	apiRequestCountsMu.Unlock()

	router := gin.New()
	router.Use(rateLimitMiddleware())
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Make requests up to the limit
	for i := 0; i < apiRateLimitPerMinute; i++ {
		req, _ := http.NewRequest("GET", "/test", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Request %d returned %d, want %d", i+1, w.Code, http.StatusOK)
		}
	}

	// Next request should be rate limited
	req, _ := http.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("Rate limited request returned %d, want %d", w.Code, http.StatusTooManyRequests)
	}
}

// TestPruneProcessedObjectsCache tests the cache pruning function
func TestPruneProcessedObjectsCache(t *testing.T) {
	// Reset the cache
	processedObjectsMu.Lock()
	processedObjects = make(map[string]processedObjectEntry)

	// Add entries with various ages
	now := time.Now()
	processedObjects["old1"] = processedObjectEntry{processedAt: now.Add(-25 * time.Hour)} // Older than TTL
	processedObjects["old2"] = processedObjectEntry{processedAt: now.Add(-26 * time.Hour)} // Older than TTL
	processedObjects["new1"] = processedObjectEntry{processedAt: now.Add(-1 * time.Hour)}  // Within TTL
	processedObjects["new2"] = processedObjectEntry{processedAt: now.Add(-2 * time.Hour)}  // Within TTL

	// Run pruning
	pruneProcessedObjectsCache()

	// Check that old entries were removed
	if _, exists := processedObjects["old1"]; exists {
		t.Error("old1 should have been pruned")
	}
	if _, exists := processedObjects["old2"]; exists {
		t.Error("old2 should have been pruned")
	}

	// Check that new entries remain
	if _, exists := processedObjects["new1"]; !exists {
		t.Error("new1 should not have been pruned")
	}
	if _, exists := processedObjects["new2"]; !exists {
		t.Error("new2 should not have been pruned")
	}

	processedObjectsMu.Unlock()
}

// TestIsAllowedFileNameChar tests the character validation function
func TestIsAllowedFileNameChar(t *testing.T) {
	allowed := []rune{'a', 'Z', '5', '.', '-', '_', ' ', '(', ')'}
	notAllowed := []rune{'<', '>', ':', '"', '|', '?', '*', '/', '\\'}

	for _, r := range allowed {
		if !isAllowedFileNameChar(r) {
			t.Errorf("isAllowedFileNameChar(%q) should return true", r)
		}
	}

	for _, r := range notAllowed {
		if isAllowedFileNameChar(r) {
			t.Errorf("isAllowedFileNameChar(%q) should return false", r)
		}
	}
}

// Benchmark tests

func BenchmarkNewTraceID(b *testing.B) {
	for i := 0; i < b.N; i++ {
		newTraceID()
	}
}

func BenchmarkSecureCompare(b *testing.B) {
	a := "averylongsecretapikey12345"
	bStr := "averylongsecretapikey12345"
	for i := 0; i < b.N; i++ {
		secureCompare(a, bStr)
	}
}

func BenchmarkSanitizeFileName(b *testing.B) {
	key := "uploads/documents/2024/01/my-important-document.pdf"
	for i := 0; i < b.N; i++ {
		sanitizeFileName(key)
	}
}
