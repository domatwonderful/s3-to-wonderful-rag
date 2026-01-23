# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /build

# Install build dependencies (only git, minimal)
RUN apk add --no-cache git

# Copy go mod files first for better caching
COPY go.mod ./

# Download dependencies (this layer will be cached if go.mod doesn't change)
RUN go mod download

# Copy source code
COPY cmd/s3-to-wonderful-rag ./cmd/s3-to-wonderful-rag

# Generate go.sum based on actual imports (needed for go build)
RUN go mod tidy

# Build arguments for multi-platform support
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Build the application
# -ldflags="-w -s": strip debug info and symbol table (smaller binary)
# No -v (verbose), no -a (rebuild all), no -installsuffix (not needed with CGO_ENABLED=0)
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-w -s" -o wonderful-rag-sync ./cmd/s3-to-wonderful-rag

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/wonderful-rag-sync .

# Expose port
EXPOSE 8080

# Run the application
CMD ["./wonderful-rag-sync"]
