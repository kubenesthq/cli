package models

import "time"

// Team represents a Kubenest team
type Team struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TeamList represents a list of teams
type TeamList struct {
	Items []Team `json:"items"`
	Total int    `json:"total"`
}

// Cluster represents a Kubernetes cluster
type Cluster struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Region      string    `json:"region"`
	Version     string    `json:"version"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ClusterList represents a list of clusters
type ClusterList struct {
	Items []Cluster `json:"items"`
	Total int       `json:"total"`
}

// Project represents a Kubenest project
type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	TeamID      string    `json:"team_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ProjectList represents a list of projects
type ProjectList struct {
	Items []Project `json:"items"`
	Total int       `json:"total"`
}

// StackDeploy represents a stack deployment
type StackDeploy struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ProjectID string    `json:"project_id"`
	ClusterID string    `json:"cluster_id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StackDeployList represents a list of stack deployments
type StackDeployList struct {
	Items []StackDeploy `json:"items"`
	Total int           `json:"total"`
}

// ListResponse is a generic response for list endpoints
type ListResponse struct {
	Items      interface{} `json:"items"`
	TotalCount int         `json:"total_count"`
}

// ErrorResponse represents an API error response
type ErrorResponse struct {
	Error       string `json:"error"`
	Code        string `json:"code"`
	Description string `json:"description"`
}
