$ErrorActionPreference = "Stop"

# Keep the caller's location intact and use the same validated embed path as CI.
Push-Location (Join-Path $PSScriptRoot "web")
try {
    Write-Host "Building and embedding the Plus dashboard (Node.js 24 required)..."
    npm ci
    if ($LASTEXITCODE -ne 0) { throw "Frontend dependency installation failed." }
    npm run build:embed
    if ($LASTEXITCODE -ne 0) { throw "Frontend build or embedding failed." }
}
finally {
    Pop-Location
}
