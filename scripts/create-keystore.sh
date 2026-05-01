#!/usr/bin/env sh
set -eu

usage() {
  cat <<'EOF'
Usage: scripts/create-keystore.sh [options]

Creates a self-signed TLS certificate/key pair for the broker.

Defaults:
  --out data/keystore
  --cn localhost
  --san DNS:localhost,IP:127.0.0.1
  --days 3650

Options:
  --out PATH       Combined PEM file written for KeyStorePath
  --cn NAME        Certificate common name
  --san LIST       Comma-separated SAN list, e.g. DNS:edge-rpi-01,IP:192.168.1.50
  --days DAYS      Certificate validity in days
  -h, --help       Show this help

The broker currently expects PEM, not JKS/PKCS12. Use this in config.yaml:

  TCPS:
    Enabled: true
    KeyStorePath: "data/keystore"
    KeyStorePassword: ""

EOF
}

out="data/keystore"
cn="localhost"
san="DNS:localhost,IP:127.0.0.1"
days="3650"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --out)
      out="${2:?missing value for --out}"
      shift 2
      ;;
    --cn)
      cn="${2:?missing value for --cn}"
      shift 2
      ;;
    --san)
      san="${2:?missing value for --san}"
      shift 2
      ;;
    --days)
      days="${2:?missing value for --days}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if ! command -v openssl >/dev/null 2>&1; then
  echo "openssl is required but was not found in PATH" >&2
  exit 1
fi

out_dir=$(dirname "$out")
crt="${out}.crt"
key="${out}.key"
cfg=$(mktemp "${TMPDIR:-/tmp}/monstermq-edge-cert.XXXXXX")
trap 'rm -f "$cfg"' EXIT INT TERM

mkdir -p "$out_dir"

cat >"$cfg" <<EOF
[req]
default_bits = 2048
prompt = no
default_md = sha256
distinguished_name = dn
x509_extensions = ext

[dn]
CN = $cn

[ext]
subjectAltName = $san
EOF

openssl req -x509 -newkey rsa:2048 -nodes \
  -days "$days" \
  -keyout "$key" \
  -out "$crt" \
  -config "$cfg"

cat "$crt" "$key" >"$out"
chmod 600 "$key" "$out"

echo "created combined PEM: $out"
echo "created certificate:  $crt"
echo "created private key:  $key"
echo
echo "Set KeyStorePath to: $out"
