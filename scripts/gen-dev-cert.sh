#!/usr/bin/env bash
set -euo pipefail

# gen-dev-cert.sh — generate self-signed CA + server cert for dev access
# Output: certs/ca.crt, certs/server.crt, certs/server.key
# SAN: configurable domain, localhost, 127.0.0.1, gw
# Requires: openssl (macOS or Linux)

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CERT_DIR="${SCRIPT_DIR}/../certs"
ENV_FILE="${SCRIPT_DIR}/../.env"

if [ -f "$ENV_FILE" ]; then
  # Load repo-local gateway settings such as AICG_TLS_DOMAIN.
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

TLS_DOMAIN="${AICG_TLS_DOMAIN:-}"
if [ -z "$TLS_DOMAIN" ] && [ -n "${AICG_PUBLIC_BASE_URL:-}" ]; then
  TLS_DOMAIN="$(printf '%s\n' "$AICG_PUBLIC_BASE_URL" | sed -E 's#^[a-zA-Z]+://([^/:]+).*$#\1#')"
fi
if [ -z "$TLS_DOMAIN" ]; then
  TLS_DOMAIN="localhost"
fi

mkdir -p "$CERT_DIR"

# 1. Generate CA private key and self-signed root certificate
openssl genrsa -out "$CERT_DIR/ca.key" 4096 2>/dev/null
openssl req -x509 -new -nodes -key "$CERT_DIR/ca.key" \
  -sha256 -days 3650 \
  -subj "/CN=AgentGate Dev CA" \
  -out "$CERT_DIR/ca.crt"

# 2. Generate server private key
openssl genrsa -out "$CERT_DIR/server.key" 2048 2>/dev/null

# 3. Create server CSR with SAN extension
cat > "$CERT_DIR/server.cnf" <<'XEOF'
[req]
default_bits = 2048
prompt = no
default_md = sha256
distinguished_name = dn
req_extensions = req_ext

[dn]
CN = __TLS_DOMAIN__

[req_ext]
subjectAltName = @alt_names

[alt_names]
DNS.1 = __TLS_DOMAIN__
DNS.2 = localhost
DNS.3 = gw
IP.1 = 127.0.0.1
XEOF

sed -i.bak "s/__TLS_DOMAIN__/${TLS_DOMAIN}/g" "$CERT_DIR/server.cnf" && rm -f "$CERT_DIR/server.cnf.bak"

openssl req -new -key "$CERT_DIR/server.key" \
  -out "$CERT_DIR/server.csr" \
  -config "$CERT_DIR/server.cnf"

# 4. Sign server cert with dev CA
openssl x509 -req -in "$CERT_DIR/server.csr" \
  -CA "$CERT_DIR/ca.crt" -CAkey "$CERT_DIR/ca.key" \
  -CAcreateserial -out "$CERT_DIR/server.crt" \
  -days 365 -sha256 \
  -extfile "$CERT_DIR/server.cnf" -extensions req_ext

# 5. Clean up temporary files
rm -f "$CERT_DIR/server.csr" "$CERT_DIR/server.cnf" "$CERT_DIR/ca.srl" "$CERT_DIR/ca.key"

chmod 600 "$CERT_DIR/server.key"
chmod 644 "$CERT_DIR/server.crt" "$CERT_DIR/ca.crt"

echo "[gen-dev-cert] done"
echo "  CA:    certs/ca.crt"
echo "  Cert:  certs/server.crt"
echo "  Key:   certs/server.key (mode 0600)"
echo ""
echo "  Use:   aicg login --ca=certs/ca.crt --gateway=${AICG_PUBLIC_BASE_URL:-https://${TLS_DOMAIN}:8443}"
