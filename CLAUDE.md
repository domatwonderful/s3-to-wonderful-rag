# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is a Go-based Kubernetes service that syncs files from cloud storage (AWS S3, Google Cloud Storage, or Azure Blob Storage) to the Wonderful AI Platform RAG API. It runs as a containerized deployment with scheduled sync operations and exposes REST/metrics endpoints.

## Architecture

### Core Components

**Single Binary Application** (`cmd/s3-to-wonderful-rag/main.go`):
- All code is in a single Go file (~1200 lines)
- Storage abstraction via `StorageClient` interface with three implementations: `S3Storage`, `GCSStorage`, `AzureStorage`
- HTTP server using Gin framework for health checks, metrics, and manual sync triggers
- Background goroutine running periodic syncs based on configurable interval
- Prometheus metrics for observability

**Upload Process** (3-step flow in `uploadToWonderful` function):
1. Request pre-signed S3 URL from Wonderful API (`POST /api/v1/storage`)
2. Upload file directly to S3 using pre-signed URL (`PUT` request)
3. Attach uploaded file to RAG (`POST /api/v1/rags/{rag_id}/files` with `file_ids`)

**File Archiving**:
- Successfully processed files → moved to `{prefix}/processed/` folder
- Failed files → moved to `{prefix}/error/` folder
- Archived files are skipped in subsequent syncs

### Storage Provider Configuration

The service uses `STORAGE_PROVIDER` env var to determine which cloud provider to use:
- `s3` - AWS S3 (requires `S3_BUCKET`, `AWS_REGION`, optional IAM role or access keys)
- `gcs`/`gcp`/`google` - Google Cloud Storage (requires `GCS_BUCKET`, optional Workload Identity)
- `azure` - Azure Blob Storage (requires `AZURE_STORAGE_CONTAINER`, credentials via connection string or account+key)

## Development Commands

### Local Development with Docker

Using Apple's native `container` CLI on macOS:
```bash
# Start container system
container system start
container builder start

# Build image
container build -f build/docker/Dockerfile -t s3-to-wonderful-rag:local .

# Run with env file
container run --rm -d --name s3-to-wonderful-rag-local --env-file .env -p 8080:8080 s3-to-wonderful-rag:local

# View logs
container logs -n 200 s3-to-wonderful-rag-local

# Stop container
container stop s3-to-wonderful-rag-local
```

Using standard Docker:
```bash
docker build -f build/docker/Dockerfile -t s3-to-wonderful-rag:local .
docker run --rm -d --name s3-to-wonderful-rag-local --env-file .env -p 8080:8080 s3-to-wonderful-rag:local
docker logs -f s3-to-wonderful-rag-local
docker stop s3-to-wonderful-rag-local
```

### Go Development

```bash
# Download dependencies
go mod download

# Run locally (requires .env or environment variables)
go run cmd/s3-to-wonderful-rag/main.go

# Build binary
go build -o wonderful-rag-sync ./cmd/s3-to-wonderful-rag

# Tidy dependencies
go mod tidy
```

### Helm Chart Operations

```bash
# Install chart with inline secrets
helm install s3-to-wonderful-rag ./charts/s3-to-wonderful-rag \
  --set env.storageProvider=s3 \
  --set env.awsRegion=us-east-1 \
  --set env.s3Bucket=your-bucket \
  --set env.wonderfulTenant=swiss-german \
  --set env.wonderfulEnv=sb \
  --set secrets.wonderfulRagId=YOUR_RAG_ID \
  --set secrets.wonderfulApiKey=YOUR_API_KEY

# Install chart with existing Kubernetes secret
helm install s3-to-wonderful-rag ./charts/s3-to-wonderful-rag \
  --set secrets.existingSecret=your-secret-name

# Upgrade release
helm upgrade s3-to-wonderful-rag ./charts/s3-to-wonderful-rag

# Uninstall
helm uninstall s3-to-wonderful-rag

# Test template rendering
helm template s3-to-wonderful-rag ./charts/s3-to-wonderful-rag
```

### Kubernetes Operations

```bash
# View logs
kubectl logs -f -n wonderful-rag deployment/wonderful-rag

# Trigger manual sync
kubectl exec -n wonderful-rag deployment/wonderful-rag -- curl -X POST http://localhost:8080/api/v1/sync

# Check metrics
kubectl port-forward -n wonderful-rag deployment/wonderful-rag 8080:8080
curl http://localhost:8080/metrics
```

## Configuration Details

### Wonderful API Environment URLs

The service constructs the API URL as: `https://{tenant}.api.{env}.wonderful.ai`

Valid environments (`WONDERFUL_ENV`):
- `dev` - Development
- `demo` - Demo environment
- `sb` - Sandbox (default)
- `prod` - Production

### Sync Interval Configuration

Two env vars control sync frequency (prefer seconds):
- `SYNC_INTERVAL_SECONDS` - Interval in seconds (e.g., `1800` for 30 minutes)
- `SYNC_INTERVAL_MINUTES` - Fallback in minutes (e.g., `30`)
- Default: 30 minutes if neither is set

### File Size Limits

`MAX_FILE_SIZE_MB` - Maximum file size in MB (default `0` = no limit). Files exceeding this limit are skipped and counted as failed.

## API Endpoints

The service exposes these HTTP endpoints on port 8080:

- `GET /health` - Health check with service info
- `GET /metrics` - Prometheus metrics
- `POST /api/v1/sync` - Trigger manual sync (async)
- `GET /api/v1/stats` - Get sync statistics
- `GET /api/v1/processed-files` - List processed files

## Prometheus Metrics

Key metrics exported (all labeled by `provider`):
- `wonderful_rag_sync_runs_total` - Total sync runs
- `wonderful_rag_files_processed_total` - Successfully processed files
- `wonderful_rag_files_failed_total` - Failed files
- `wonderful_rag_files_skipped_total` - Skipped files (archived or too large)
- `wonderful_rag_sync_duration_seconds` - Sync duration histogram
- `wonderful_rag_last_sync_success` - Last sync success status (1/0)
- `wonderful_rag_last_sync_files_found` - Files found in last sync

## Testing

### Local Testing with S3

1. Copy `.env.example` to `.env`
2. Set `STORAGE_PROVIDER=s3`
3. Configure S3 credentials and bucket
4. Set Wonderful API credentials
5. Run: `go run cmd/s3-to-wonderful-rag/main.go`

### Local Testing with GCS

1. Set `STORAGE_PROVIDER=gcs` in `.env`
2. Set `GCS_BUCKET` and optional `GCS_PREFIX`
3. Set `GOOGLE_APPLICATION_CREDENTIALS` to service account JSON path
4. Ensure service account has Storage Object Viewer permission
5. Run the application

### Testing in Kubernetes

Deploy with Helm and check logs for sync activity. Use `kubectl port-forward` to access metrics/API endpoints locally.

## CI/CD

GitHub Actions workflow (`.github/workflows/docker-build.yml`) automatically:
- Builds Docker image on push to `main` or manual trigger
- Pushes to `ghcr.io/domatwonderful/s3-to-wonderful-rag:latest` and `ghcr.io/domatwonderful/s3-to-wonderful-rag:sha-{commit}`
- Uses multi-stage build with Go 1.24 and Alpine Linux

## Important Implementation Notes

### Code Organization

All application logic is in `cmd/s3-to-wonderful-rag/main.go`. There are no internal packages. When adding features, keep code in this single file unless it becomes significantly large (>2000 lines), at which point consider refactoring into internal packages.

### Error Handling and Retries

- HTTP requests use `doRequestWithRetry` with exponential backoff + jitter
- Retryable status codes: 429 (Too Many Requests), 5xx (Server Errors)
- Default retry attempts: 3 for API calls, 2 for S3 uploads
- File moves to archive folders are logged but don't fail the sync

### Logging

- Uses structured JSON logging via `logrus`
- Log level: Debug (verbose output)
- File operations include size formatting and duration tracking
- Trace IDs (`X-Request-Id`) for request correlation

### Security

Helm chart includes security best practices:
- Non-root user (UID 10001)
- Read-only root filesystem
- Drop all capabilities
- Network policies restricting egress to DNS and HTTPS
- Pod Disruption Budget enabled by default

### Storage Client Interface

When adding new storage providers, implement the `StorageClient` interface:
```go
type StorageClient interface {
    Provider() string
    ListObjects(ctx context.Context) ([]ObjectInfo, error)
    DownloadObject(ctx context.Context, key string) ([]byte, error)
    MoveObject(ctx context.Context, srcKey, destKey string) error
    Location() string
    Prefix() string
}
```

## Secrets Management

Helm chart supports two patterns:
1. **Existing Secret**: Set `secrets.existingSecret` to reference a pre-created Kubernetes secret
2. **Inline Values**: Pass secrets via `--set secrets.wonderfulRagId=...` etc.

Required secret keys:
- `wonderful_rag_id` - RAG ID for file attachments
- `wonderful_api_key` - API key for authentication
- Provider-specific: `aws_access_key_id`, `aws_secret_access_key`, `azure_storage_key`, etc.
