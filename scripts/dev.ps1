# Windows dev helper. Mirrors the Makefile targets for machines without `make`.
#   .\scripts\dev.ps1 doctor | test | api | web | tidy
param([Parameter(Position = 0)][string]$Task = "help")

$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

# Load .env into the process environment if present.
if (Test-Path ".env") {
    Get-Content ".env" | ForEach-Object {
        if ($_ -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
            [Environment]::SetEnvironmentVariable($Matches[1], $Matches[2].Trim())
        }
    }
}

switch ($Task) {
    "doctor" { go run ./cmd/argus-doctor }
    "test"   { go test ./... }
    "api"    { go run ./cmd/argus-api }
    "web"    { Set-Location web; npm run dev }
    "tidy"   { go mod tidy; gofmt -w ./cmd ./internal }
    default  { Write-Host "tasks: doctor | test | api | web | tidy" }
}
