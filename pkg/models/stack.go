package models

// Stack represents a stack in the system
type Stack struct {
	UUID        string           `json:"uuid"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Components  []StackComponent `json:"components"`
	Parameters  []StackParameter `json:"parameters"`
	CreatedAt   string           `json:"created_at"`
	UpdatedAt   string           `json:"updated_at"`
}

// StackComponent represents a component in a stack
type StackComponent struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// StackParameter represents a parameter in a stack
type StackParameter struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Description string      `json:"description"`
	Default     interface{} `json:"default"`
	Required    bool        `json:"required"`
}

// DryRunResult represents the result of a stack dry run
type DryRunResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// StackDeploymentStatus represents the status of a stack deployment
type StackDeploymentStatus struct {
	Status    string `json:"status"`    // "pending", "in_progress", "completed", "failed"
	Message   string `json:"message"`   // Status message or error message
	Progress  int    `json:"progress"`  // Progress percentage (0-100)
	Completed bool   `json:"completed"` // Whether the deployment is completed
}
