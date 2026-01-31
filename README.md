# S3 to Wonderful RAG Sync

A Kubernetes deployment that automatically syncs files from AWS S3, GCS, or Azure Blob Storage to the Wonderful AI Platform API.

## Supported Cloud Providers

| Provider | Status | Notes |
|----------|--------|-------|
| AWS S3 | Tested | Verified in local tests |
| Google Cloud Storage | Tested | Verified in local tests |
| Azure Blob Storage | Not tested | Configuration supported, not validated yet |

## Architecture

For a comprehensive architecture overview including detailed component diagrams, data flows, and security architecture, see [ARCHITECTURE.md](./ARCHITECTURE.md).

### High-Level Overview

```
┌──────────────────────────────────────────────────────────────┐
│              Cloud Storage (AWS S3 / GCS / Azure)            │
│  • Source bucket/container with files                        │
│  • Archive folders: {prefix}/processed/, {prefix}/error/     │
└──────────────────────┬───────────────────────────────────────┘
                       │
                       │ List, Download, Move
                       │
┌──────────────────────▼───────────────────────────────────────┐
│              Kubernetes Pod (wonderful-rag-sync)             │
│  ┌────────────────────────────────────────────────────────┐  │
│  │  Go Application (Port 8080)                            │  │
│  │                                                         │  │
│  │  Components:                                            │  │
│  │  • Storage Client Interface (S3/GCS/Azure)             │  │
│  │  • Background Sync Goroutine (periodic)                │  │
│  │  • HTTP Server (Gin) - health, metrics, API            │  │
│  │  • Prometheus Metrics                                  │  │
│  │                                                         │  │
│  │  Upload Process (3 Steps):                             │  │
│  │  1. POST /api/v1/storage (get pre-signed S3 URL)       │  │
│  │  2. PUT {presigned-url} (upload to Wonderful's S3)     │  │
│  │  3. POST /api/v1/rags/{id}/files (attach to RAG)       │  │
│  └────────────────────────────────────────────────────────┘  │
└──────────────────────┬───────────────────────────────────────┘
                       │
                       │ HTTPS (x-api-key auth)
                       │
┌──────────────────────▼───────────────────────────────────────┐
│          Wonderful RAG API                                   │
│  https://{tenant}.api.{env}.wonderful.ai                     │
│  • Provides pre-signed S3 URLs for uploads                   │
│  • Manages RAG file attachments                              │
└──────────────────────────────────────────────────────────────┘
```

### Data Flow

1. **Discovery**: Service lists files from configured storage provider (filtered by prefix)
2. **Download**: Files are downloaded from source storage into memory
3. **Upload (3-step process)**:
   - Step 1: Request pre-signed S3 URL from Wonderful API
   - Step 2: Upload file directly to S3 using pre-signed URL
   - Step 3: Attach uploaded file to RAG via API
4. **Archive**: Successfully processed files → `processed/`, failed files → `error/`
5. **Metrics**: All operations are tracked via Prometheus metrics

## Components

### Sync Service
- **Type**: Deployment
- **Image**: `ghcr.io/domatwonderful/s3-to-wonderful-rag:latest` (example)
- **Language**: Go
- **Features**:
  - Automatic S3 bucket monitoring
  - File download and upload to Wonderful API
  - Tracks processed files to avoid duplicates
  - Configurable sync interval
  - REST API for manual triggers and stats

## Configuration

### Environment Variables

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `STORAGE_PROVIDER` | Storage provider (`s3`, `gcs`, `azure`) | Yes | `s3` |
| `AWS_REGION` | AWS region for S3 | Yes (S3) | `us-east-1` |
| `S3_BUCKET` | S3 bucket name | Yes (S3) | - |
| `S3_PREFIX` | S3 prefix/folder path | No (S3) | - |
| `AWS_ACCESS_KEY_ID` | AWS access key (or use IAM role) | No (S3) | - |
| `AWS_SECRET_ACCESS_KEY` | AWS secret key (or use IAM role) | No (S3) | - |
| `GCS_BUCKET` | GCS bucket name | Yes (GCS) | - |
| `GCS_PREFIX` | GCS prefix/folder path | No (GCS) | - |
| `AZURE_STORAGE_ACCOUNT` | Azure storage account | Yes (Azure)* | - |
| `AZURE_STORAGE_KEY` | Azure storage key | Yes (Azure)* | - |
| `AZURE_STORAGE_CONTAINER` | Azure container name | Yes (Azure) | - |
| `AZURE_STORAGE_PREFIX` | Azure prefix/folder path | No (Azure) | - |
| `AZURE_STORAGE_CONNECTION_STRING` | Azure connection string | Yes (Azure)* | - |
| `WONDERFUL_TENANT` | Wonderful tenant (subdomain) | Yes | `swiss-german` |
| `WONDERFUL_ENV` | Wonderful environment (`dev`, `demo`, `sb`, `prod`) | Yes | `sb` |
| `WONDERFUL_RAG_ID` | RAG ID for file uploads | Yes | - |
| `WONDERFUL_API_KEY` | Wonderful API key | Yes | - |
| `SYNC_INTERVAL_SECONDS` | Sync interval in seconds | No | `1800` |
| `SYNC_INTERVAL_MINUTES` | Sync interval in minutes (fallback) | No | `30` |
| `MAX_FILE_SIZE_MB` | Max file size in MB (0 = no limit) | No | `0` |
| `PORT` | HTTP server port | No | `8080` |

*Credentials depend on provider. For Azure, set `AZURE_STORAGE_CONNECTION_STRING`, or `AZURE_STORAGE_ACCOUNT` + `AZURE_STORAGE_KEY`.

### Helm Values

Edit `charts/s3-to-wonderful-rag/values.yaml` or pass overrides with `--set` to configure:
- Storage provider
- Provider-specific bucket/container and prefix
- AWS region (S3 only)
- Wonderful tenant and environment
- Sync interval

## Archiving Processed and Failed Files

After a successful upload+attach, files are moved into a `processed/` folder in
the same bucket/container. Failed uploads are moved into an `error/` folder.
If you configure a prefix, the service will use `<prefix>/processed/` and
`<prefix>/error/` instead.

## Storage Providers

### AWS S3
- Set `STORAGE_PROVIDER=s3`
- Required: `S3_BUCKET`, `AWS_REGION`
- Optional: `S3_PREFIX`
- Auth: IAM role or `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`

### Google Cloud Storage
- Set `STORAGE_PROVIDER=gcs`
- Required: `GCS_BUCKET`
- Optional: `GCS_PREFIX`
- Auth: Workload Identity or `GOOGLE_APPLICATION_CREDENTIALS` with a mounted service account key file

### Azure Blob Storage
- Set `STORAGE_PROVIDER=azure`
- Required: `AZURE_STORAGE_CONTAINER`
- Optional: `AZURE_STORAGE_PREFIX`
- Auth: `AZURE_STORAGE_CONNECTION_STRING` or `AZURE_STORAGE_ACCOUNT` + `AZURE_STORAGE_KEY`

### Secrets

Create a Kubernetes secret (or ExternalSecret) and reference it via Helm:
```bash
helm install s3-to-wonderful-rag ./charts/s3-to-wonderful-rag \
  --set secrets.existingSecret=your-secret-name
```

Your secret should contain:
- `wonderful_rag_id`
- `wonderful_api_key`
- Optional provider credentials (e.g. `aws_access_key_id`, `aws_secret_access_key`)

## API Endpoints

### Health Check
```
GET /health
```

### Prometheus Metrics
```
GET /metrics
```

Custom metrics include:
- `wonderful_rag_sync_runs_total`
- `wonderful_rag_sync_errors_total`
- `wonderful_rag_files_processed_total`
- `wonderful_rag_files_failed_total`
- `wonderful_rag_files_skipped_total`
- `wonderful_rag_sync_duration_seconds`
- `wonderful_rag_sync_in_progress`
- `wonderful_rag_last_sync_timestamp`
- `wonderful_rag_last_sync_success`
- `wonderful_rag_last_sync_files_found`
- `wonderful_rag_last_sync_files_processed`
- `wonderful_rag_last_sync_files_failed`
- `wonderful_rag_last_sync_files_skipped`

### Trigger Manual Sync
```
POST /api/v1/sync
```

### Get Statistics
```
GET /api/v1/stats
```

### Get Processed Files
```
GET /api/v1/processed-files
```

## Deployment

### Using Helm
```bash
helm install s3-to-wonderful-rag ./charts/s3-to-wonderful-rag \
  --set env.storageProvider=s3 \
  --set env.awsRegion=us-east-1 \
  --set env.s3Bucket=starsliderragdemo \
  --set env.wonderfulTenant=swiss-german \
  --set env.wonderfulEnv=sb \
  --set secrets.existingSecret=your-secret-name
```

## How It Works

1. **Initial Sync**: On startup, the service immediately performs a sync
2. **Scheduled Sync**: Then syncs at regular intervals (configurable, default 30 minutes)
3. **File Processing**:
   - Lists all files in the selected storage provider (with optional prefix filter)
   - Downloads each file from the provider
   - Uploads to Wonderful API with metadata
   - Tracks processed files to avoid duplicates
   - Logs success/failure for each file

## File Upload Format

Files are uploaded to the Wonderful API endpoint:
```
POST https://{tenant}.api.{env}.wonderful.ai/api/v1/rags/{rag_id}/files
```

Files are uploaded as multipart/form-data with:
- **file**: The file content
- **source**: "s3"
- **s3_key**: S3 object key
- **s3_bucket**: S3 bucket name

## Troubleshooting

### Service won't start
- Check all required environment variables are set
- Verify secrets are correctly created in Kubernetes
- Check pod logs: `kubectl logs -n wonderful-rag deployment/wonderful-rag`

### Files not syncing
- Verify bucket/container name and prefix are correct
- Check storage credentials or managed identity permissions
- Verify Wonderful API credentials
- Check logs for specific error messages

### Upload failures
- Verify Wonderful API URL is correct
- Check API key is valid
- Verify network connectivity from cluster to API
- Check API response in logs

### View logs
```bash
# View all logs
kubectl logs -f -n wonderful-rag deployment/wonderful-rag

# View logs from specific pod
kubectl logs -f -n wonderful-rag <pod-name>

# View logs from last 100 lines
kubectl logs --tail=100 -n wonderful-rag deployment/wonderful-rag
```

## Development

### Project Structure

```
s3-to-wonderful-rag/
├── cmd/                      # Application entrypoint
│   └── s3-to-wonderful-rag/
│       └── main.go
├── build/                    # Build assets
│   └── docker/
│       └── Dockerfile
├── charts/                   # Helm chart
│   └── s3-to-wonderful-rag/
├── .dockerignore             # Docker ignore rules
├── go.mod                    # Go module definition
└── README.md                 # This file
```

### Building the Docker Image

Build and publish your image to a registry your cluster can access, then update
`charts/s3-to-wonderful-rag/values.yaml` (image repository/tag/digest).

For manual builds:
```bash
docker build -f build/docker/Dockerfile -t ghcr.io/domatwonderful/s3-to-wonderful-rag:latest .
```

### Running Locally with `container` (macOS)

Build and run using Apple's native container CLI:
```bash
container system start
container builder start
container build -f build/docker/Dockerfile -t s3-to-wonderful-rag:local .
container run --rm -d --name s3-to-wonderful-rag-local --env-file .env -p 8080:8080 s3-to-wonderful-rag:local
```

Check logs and stop:
```bash
container logs -n 200 s3-to-wonderful-rag-local
container stop s3-to-wonderful-rag-local
```

### Helm Chart

Install with the built-in chart:
```bash
helm install s3-to-wonderful-rag ./charts/s3-to-wonderful-rag \
  --set env.storageProvider=s3 \
  --set env.awsRegion=us-east-1 \
  --set env.s3Bucket=starsliderragdemo \
  --set env.wonderfulTenant=swiss-german \
  --set env.wonderfulEnv=sb \
  --set secrets.wonderfulRagId=YOUR_RAG_ID \
  --set secrets.wonderfulApiKey=YOUR_API_KEY
```

If you already have a Kubernetes secret with credentials:
```bash
helm install s3-to-wonderful-rag ./charts/s3-to-wonderful-rag \
  --set secrets.existingSecret=your-secret-name
```

## Security Best Practices

1. **Secrets Management**: Use Kubernetes secrets or External Secrets Operator
2. **IAM Roles**: Prefer IAM roles for service accounts over access keys
3. **Network Policies**: Restrict network access to only required endpoints
4. **RBAC**: Use least-privilege service accounts
5. **TLS**: Use TLS for all external API calls
6. **Rotate Secrets**: Regularly rotate API keys and AWS credentials

