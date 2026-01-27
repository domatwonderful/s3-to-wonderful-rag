# Security Guide

This document outlines security best practices for deploying and operating the s3-to-wonderful-rag service.

## Critical Security Fixes

The following critical security issues have been addressed:

### 1. API Authentication

**Issue:** Internal API endpoints were previously accessible without authentication, allowing unauthorized users to trigger sync operations, view statistics, and list processed files.

**Fix:** API key authentication middleware protects sensitive endpoints.

**Configuration:**
```bash
# Set INTERNAL_API_KEY environment variable
export INTERNAL_API_KEY="your-secure-random-key-here"

# Or in Helm values:
helm install s3-to-wonderful-rag ./charts/s3-to-wonderful-rag \
  --set secrets.existingSecret=my-secret
```

**Usage:**
```bash
# Trigger sync with API key in header
curl -X POST http://localhost:8080/api/v1/sync \
  -H "x-api-key: your-secure-random-key-here"

# Or via query parameter
curl -X POST "http://localhost:8080/api/v1/sync?api_key=your-secure-random-key-here"
```

**Protected Endpoints:**
- `POST /api/v1/sync` - Trigger manual sync
- `GET /api/v1/stats` - View sync statistics
- `GET /api/v1/processed-files` - List processed files

**Public Endpoints:**
- `GET /health` - Health check (always accessible)
- `GET /metrics` - Prometheus metrics (restrict via network policy or service mesh)

**Important:** If `INTERNAL_API_KEY` is not set, the service logs a warning and allows unauthenticated access (for backward compatibility). **Always set this in production.**

### 2. Network Policy Restrictions

**Issue:** The network policy allowed egress to any IP address on port 443 (`cidr: 0.0.0.0/0`), potentially allowing data exfiltration if the container is compromised.

**Fix:** Configurable CIDR restrictions for HTTPS egress.

**Configuration:**

Identify the IP ranges for your cloud provider and Wonderful API:

#### AWS S3 IP Ranges
Find current ranges at: https://ip-ranges.amazonaws.com/ip-ranges.json
```bash
# Example for us-east-1:
curl -s https://ip-ranges.amazonaws.com/ip-ranges.json | \
  jq -r '.prefixes[] | select(.service=="S3" and .region=="us-east-1") | .ip_prefix'
```

#### Google Cloud Storage
GCS uses Google's public IP ranges:
- `34.64.0.0/10`
- `35.184.0.0/13`
- Full list: https://www.gstatic.com/ipranges/cloud.json

#### Azure Blob Storage
Azure Storage uses regional IP ranges:
- Download: https://www.microsoft.com/en-us/download/details.aspx?id=56519
- Search for "Storage" in your region

#### Wonderful API
Contact your Wonderful AI administrator for the API endpoint IP addresses or DNS name, then resolve to IPs:
```bash
nslookup swiss-german.api.sb.wonderful.ai
```

**Helm Configuration:**
```yaml
# values.yaml or --set
networkPolicy:
  enabled: true
  allowedCidrs:
    # AWS S3 (us-east-1)
    - 52.216.0.0/15
    - 3.5.0.0/16
    - 54.231.0.0/16
    # Wonderful API (example - replace with actual)
    - 1.2.3.4/32
```

**Verify Network Policy:**
```bash
kubectl get networkpolicy -n wonderful-rag
kubectl describe networkpolicy -n wonderful-rag
```

### 3. Secrets Management

**Issue:** Helm chart allowed secrets to be passed via `--set` command line arguments, which are stored in:
- Shell history (plain text)
- Helm release history (plain text, accessible via `helm get values`)
- kubectl/helm command output

**Fix:** Added warnings and documentation promoting external secret management.

**Recommended Approach:**

#### Option 1: External Secrets Operator (Best Practice)
```yaml
# external-secret.yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: s3-to-wonderful-rag-secrets
  namespace: wonderful-rag
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: SecretStore
  target:
    name: s3-to-wonderful-rag-secrets
    creationPolicy: Owner
  data:
    - secretKey: wonderful_rag_id
      remoteRef:
        key: wonderful/rag-id
    - secretKey: wonderful_api_key
      remoteRef:
        key: wonderful/api-key
    - secretKey: internal_api_key
      remoteRef:
        key: wonderful/internal-api-key
    - secretKey: aws_access_key_id
      remoteRef:
        key: aws/access-key-id
    - secretKey: aws_secret_access_key
      remoteRef:
        key: aws/secret-access-key
```

Then reference in Helm:
```bash
helm install s3-to-wonderful-rag ./charts/s3-to-wonderful-rag \
  --set secrets.existingSecret=s3-to-wonderful-rag-secrets
```

#### Option 2: Sealed Secrets
```bash
# Create secret locally
kubectl create secret generic s3-to-wonderful-rag-secrets \
  --from-literal=wonderful_rag_id=xxx \
  --from-literal=wonderful_api_key=xxx \
  --from-literal=internal_api_key=xxx \
  --dry-run=client -o yaml | \
  kubeseal -o yaml > sealed-secret.yaml

# Apply sealed secret
kubectl apply -f sealed-secret.yaml

# Install Helm chart
helm install s3-to-wonderful-rag ./charts/s3-to-wonderful-rag \
  --set secrets.existingSecret=s3-to-wonderful-rag-secrets
```

#### Option 3: Cloud Provider Secret Manager
- **AWS:** Use AWS Secrets Manager + External Secrets Operator or CSI Secret Store Driver
- **GCP:** Use Google Secret Manager + External Secrets Operator or Workload Identity
- **Azure:** Use Azure Key Vault + External Secrets Operator or CSI Secret Store Driver

## Additional Security Recommendations

### 1. Use IAM Roles Instead of Static Credentials

**AWS (IRSA - IAM Roles for Service Accounts):**
```yaml
serviceAccount:
  create: true
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/s3-to-wonderful-rag-role
```

Then remove `aws_access_key_id` and `aws_secret_access_key` from secrets.

**GCP (Workload Identity):**
```yaml
serviceAccount:
  create: true
  annotations:
    iam.gke.io/gcp-service-account: s3-to-wonderful-rag@project.iam.gserviceaccount.com
```

**Azure (Managed Identity):**
```yaml
serviceAccount:
  create: true
  annotations:
    azure.workload.identity/client-id: "12345678-1234-1234-1234-123456789012"
```

### 2. Restrict Metrics Endpoint

If you expose metrics via Ingress or Service, restrict access:

**Option 1: Service Mesh (e.g., Istio)**
```yaml
apiVersion: security.istio.io/v1beta1
kind: AuthorizationPolicy
metadata:
  name: metrics-authz
spec:
  selector:
    matchLabels:
      app: s3-to-wonderful-rag
  rules:
    - to:
        - operation:
            paths: ["/metrics"]
      from:
        - source:
            namespaces: ["monitoring"]
```

**Option 2: NetworkPolicy**
```yaml
# Add to existing network policy
ingress:
  - from:
      - namespaceSelector:
          matchLabels:
            name: monitoring
      - podSelector:
          matchLabels:
            app: prometheus
    ports:
      - protocol: TCP
        port: 8080
        endPort: 8080
```

### 3. Enable Pod Security Standards

Label namespace with Pod Security Standards:
```bash
kubectl label namespace wonderful-rag \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted
```

### 4. Implement Log Sanitization

Consider filtering sensitive data from logs before shipping to centralized logging:
- API keys (redact in application or use log processor)
- File contents (already not logged)
- Full S3 keys (mask or hash)

### 5. Regular Security Scanning

**Container Image Scanning:**
```bash
# Trivy
trivy image ghcr.io/domatwonderful/s3-to-wonderful-rag:latest

# Grype
grype ghcr.io/domatwonderful/s3-to-wonderful-rag:latest
```

**Kubernetes Manifest Scanning:**
```bash
# Kubesec
kubesec scan charts/s3-to-wonderful-rag/templates/deployment.yaml

# Checkov
checkov -d charts/s3-to-wonderful-rag/
```

### 6. Monitoring and Alerting

Set up alerts for:
- Failed authentication attempts (check logs for "invalid API key")
- Unusual network connections (via Falco or similar)
- High sync failure rates
- Unexpected egress patterns

### 7. Audit Logging

Enable Kubernetes audit logging to track:
- Secret access
- ConfigMap modifications
- Pod exec attempts

## Compliance Considerations

Depending on your compliance requirements (SOC 2, ISO 27001, HIPAA, etc.), consider:

1. **Data Encryption:**
   - In transit: HTTPS enforced (already implemented)
   - At rest: Enable encryption in S3/GCS/Azure (provider-side)

2. **Data Retention:**
   - Configure lifecycle policies in cloud storage
   - Implement log retention policies

3. **Access Control:**
   - Use RBAC to limit who can access secrets
   - Implement least-privilege IAM policies

4. **Incident Response:**
   - Document incident response procedures
   - Regular security drills

## Security Contacts

For security issues or questions:
- Open a GitHub issue (for non-sensitive topics)
- Contact your security team for sensitive concerns
- Follow responsible disclosure practices

## Changelog

- **2026-01-27:** Initial security hardening
  - Added API authentication for internal endpoints
  - Restricted network policies with configurable CIDRs
  - Enhanced secrets management documentation
  - Added ingress network policy
