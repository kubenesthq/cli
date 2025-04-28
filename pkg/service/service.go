package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"kubenest.io/cli/pkg/api"
	"kubenest.io/cli/pkg/models"
)

// Service handles the business logic for Kubenest resources
type Service struct {
	client *api.Client
}

// NewService creates a new Service instance
func NewService(client *api.Client) *Service {
	return &Service{client: client}
}

// ListTeams retrieves a list of teams
func (s *Service) ListTeams(ctx context.Context) (*models.TeamList, error) {
	resp, err := s.client.Get(ctx, "/teams")
	if err != nil {
		return nil, fmt.Errorf("failed to list teams: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var teams models.TeamList
	if err := json.Unmarshal(body, &teams); err != nil {
		return nil, fmt.Errorf("failed to unmarshal teams: %w", err)
	}

	return &teams, nil
}

// ListClusters retrieves a list of clusters
func (s *Service) ListClusters(ctx context.Context) (*models.ClusterList, error) {
	resp, err := s.client.Get(ctx, "/clusters")
	if err != nil {
		return nil, fmt.Errorf("failed to list clusters: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var clusters models.ClusterList
	if err := json.Unmarshal(body, &clusters); err != nil {
		return nil, fmt.Errorf("failed to unmarshal clusters: %w", err)
	}

	return &clusters, nil
}

// ListProjects retrieves a list of projects
func (s *Service) ListProjects(ctx context.Context) (*models.ProjectList, error) {
	resp, err := s.client.Get(ctx, "/projects")
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var projects models.ProjectList
	if err := json.Unmarshal(body, &projects); err != nil {
		return nil, fmt.Errorf("failed to unmarshal projects: %w", err)
	}

	return &projects, nil
}

// ListStackDeploys retrieves a list of stack deployments
func (s *Service) ListStackDeploys(ctx context.Context) (*models.StackDeployList, error) {
	resp, err := s.client.Get(ctx, "/stack-deploys")
	if err != nil {
		return nil, fmt.Errorf("failed to list stack deployments: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var stackDeploys models.StackDeployList
	if err := json.Unmarshal(body, &stackDeploys); err != nil {
		return nil, fmt.Errorf("failed to unmarshal stack deployments: %w", err)
	}

	return &stackDeploys, nil
}
