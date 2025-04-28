# Kubenest CLI

A command-line interface for managing applications deployed on Kubernetes clusters through the Kubenest platform.

## Features

- 🔐 Authentication
  - Login/Logout
  - Token-based authentication
- 🎯 Context Management
  - Set cluster and project context
- 📦 Application Management
  - List deployed applications
  - Deploy new applications
  - View application logs
- 🐳 Container Operations
  - Execute commands in pods
  - Copy files to/from pods

## Installation

### Prerequisites

- Go 1.21 or later
- Access to a Kubenest backend instance

### Building from Source

```bash
# Clone the repository
git clone https://github.com/kubenesthq/cli.git
cd cli

# Build the CLI
go build -o kubenest ./cmd/kubenest

# Move the binary to your PATH (optional)
sudo mv kubenest /usr/local/bin/
```

## Configuration

The CLI stores its configuration in `~/.kubenest/config.json`. This file contains:
- API URL
- Authentication token
- Current cluster and project context

## Usage

### Authentication

```bash
# Login to Kubenest
kubenest login

# Logout from Kubenest
kubenest logout
```

### Context Management

```bash
# Set or view current context (cluster and project)
kubenest context
```

### Application Management

```bash
# List all deployed applications
kubenest apps

# Deploy a new application
kubenest deploy

# View application logs
kubenest logs
```

### Container Operations

```bash
# Execute a command in a pod
kubenest exec

# Copy files to/from a pod
kubenest copy
```

## Application Configuration

When deploying applications, you can use a JSON configuration file with the following structure:

```json
{
  "name": "my-app",
  "image": "nginx:latest",
  "replicas": 2,
  "ports": [
    {
      "containerPort": 80,
      "protocol": "TCP",
      "servicePort": 80
    }
  ],
  "env": {
    "ENV_VAR": "value"
  },
  "volumes": [
    {
      "name": "data",
      "mountPath": "/data",
      "size": "1Gi"
    }
  ],
  "resources": {
    "cpu": "500m",
    "memory": "512Mi"
  },
  "healthCheck": {
    "path": "/health",
    "port": 80,
    "interval": 30,
    "timeout": 10
  }
}
```

## Development

### Project Structure

```
.
├── cmd/
│   └── kubenest/        # CLI entry point
├── pkg/
│   ├── api/            # API client and types
│   ├── cmd/            # Command implementations
│   └── config/         # Configuration management
└── go.mod              # Go module definition
```

### Building and Testing

```bash
# Build the CLI
go build -o kubenest ./cmd/kubenest

# Run tests
go test ./...
```


# Contexts

Design Principles for Kubenest CLI Contexts

Concept | Pattern
Team | Acts like an org/account scope (think gh, aws)
Cluster | Scope for all StackDeploy activity
Project | Optional scope for namespaces/apps
Defaults | Can be saved in CLI config or passed explicitly
Overrides | Always allow --team, --cluster, --project flags for one-off calls


# Set context
kubenest context set-team acme
kubenest context set-cluster dev-cluster
kubenest context set-project web-app

# See current context
kubenest context show
# 🖥️  Team: acme
# 🧭  Cluster: dev-cluster
# 📦  Project: web-app

# Use context-aware commands
kubenest stack list
kubenest deploy list
kubenest deploy create -f stackdeploy.yaml

# One-off override
kubenest deploy list --cluster staging-cluster


🔧 Store context in CLI config file:

```yaml
# ~/.kubenest/config.yaml
context:
  team: acme
  cluster: dev-cluster
  project: web-app
```
