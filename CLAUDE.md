# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the Kubenest CLI - a command-line interface for managing applications deployed on Kubernetes clusters through the Kubenest platform. It's built in Go using the Cobra CLI framework.

## Build Commands

```bash
# Build the CLI binary
go build -o kubenest ./cmd/kubenest

# Build with optimizations (smaller binary)
go build -ldflags="-s -w" -o kubenest ./cmd/kubenest

# Cross-compile for different platforms
GOOS=linux GOARCH=amd64 go build -o kubenest-linux-amd64 ./cmd/kubenest
GOOS=darwin GOARCH=arm64 go build -o kubenest-darwin-arm64 ./cmd/kubenest
GOOS=windows GOARCH=amd64 go build -o kubenest-windows-amd64.exe ./cmd/kubenest
```

## Architecture

### Entry Point
- `cmd/kubenest/main.go` - Main entry point that initializes config, creates API client, and sets up Cobra commands

### Core Packages
- `pkg/api/` - HTTP client for Kubenest API communication with authentication and team context
- `pkg/config/` - Configuration management (stored in `~/.kubenest/config.json`)
- `pkg/cmd/` - Command implementations for all CLI operations
- `pkg/models/` - Data structures for API responses
- `pkg/service/` - Business logic layer
- `pkg/term/` - Terminal utilities
- `pkg/utils/` - Shared utilities

### Context System
The CLI uses a hierarchical context system:
- **Team** - Acts like an org/account scope
- **Cluster** - Scope for all deployment activity  
- **Project** - Optional scope for namespaces/apps

Context can be set via commands (`kubenest context`) or overridden with flags (`--team`, `--cluster`, `--project`).

### Authentication
- Token-based authentication stored in config
- API requests include `Authorization: Bearer <token>` header
- Team context sent via `X-Team-UUID` header (except for `/teams` and `/login` endpoints)

### API Client Features
- Configurable base URL (defaults to https://api.kubenest.io)
- Request timeout configuration
- Debug mode with curl command output (set `DEBUG=1`)
- Automatic JSON marshaling/unmarshaling
- Error response parsing

## Key Commands
- `login/logout` - Authentication
- `context` - Set/view team, cluster, project context
- `teams/clusters/projects` - Resource listing
- `apps` - Application management
- `logs` - Application log viewing
- `exec` - Execute commands in pods (planned)
- `copy` - File copy to/from pods (planned)

## Configuration
Config stored in `~/.kubenest/config.json` with fields:
- `api_url` - Backend API URL
- `token` - Authentication token
- `team_uuid`, `cluster_uuid`, `project_uuid` - Context UUIDs
- User info fields for display

## Dependencies
- Cobra for CLI framework
- Kubernetes client libraries for potential future features
- Standard HTTP client for API communication
- Terminal utilities for interactive prompts

## Release Process
- GitHub Actions workflow triggers on version tags (`v*`)
- Builds for 6 platforms: Linux/macOS/Windows on AMD64/ARM64
- Creates GitHub release with binaries and checksums
- Uses Go 1.24 toolchain