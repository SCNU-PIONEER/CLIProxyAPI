#!/usr/bin/env bash
set -euo pipefail

# Resolve from this script, not the caller's working directory.
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/web"
echo "Building and embedding the Plus dashboard (Node.js 24 required)..."
npm ci
npm run build:embed
