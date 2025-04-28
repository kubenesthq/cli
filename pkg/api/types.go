package api

type App struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type Pod struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Ready   bool   `json:"ready"`
	Restarts int    `json:"restarts"`
}

type AppConfig struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Replicas    int               `json:"replicas"`
	Ports       []Port            `json:"ports"`
	Env         map[string]string `json:"env"`
	Volumes     []Volume          `json:"volumes"`
	Resources   Resources         `json:"resources"`
	HealthCheck HealthCheck       `json:"healthCheck"`
}

type Port struct {
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
	ServicePort   int    `json:"servicePort"`
}

type Volume struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	Size      string `json:"size"`
}

type Resources struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type HealthCheck struct {
	Path     string `json:"path"`
	Port     int    `json:"port"`
	Interval int    `json:"interval"`
	Timeout  int    `json:"timeout"`
}

type LoginResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

type User struct {
	UUID     string `json:"uuid"`
	Email    string `json:"email"`
	TeamUUID string `json:"team_uuid"`
}

type Team struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type Cluster struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type Project struct {
	UUID        string        `json:"uuid"`
	Name        string        `json:"display_name"`
	Cluster     ProjectCluster `json:"cluster"`
	CreatedAt   string        `json:"created_at"`
}

type ProjectCluster struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type StackDeploy struct {
	UUID            string                 `json:"uuid"`
	Name            string                 `json:"name"`
	Status          string                 `json:"status"`
	ParameterValues map[string]interface{} `json:"parameter_values"`
	Components      []Component            `json:"components"`
}

type Component struct {
	Name    string `json:"name"`
	GitRef  string `json:"git_ref"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type StackDeployApp struct {
	UUID      string                `json:"uuid"`
	Stack     StackDeployStack      `json:"stack"`
	Cluster   StackDeployCluster    `json:"cluster"`
	Project   StackDeployProject    `json:"project"`
	Namespace string                `json:"namespace"`
	Name      string                `json:"name"`
	Status    string                `json:"status"`
	CreatedAt string                `json:"created_at"`
	UpdatedAt string                `json:"updated_at"`
}

type StackDeployStack struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type StackDeployCluster struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type StackDeployProject struct {
	UUID       string `json:"uuid"`
	Name       string `json:"display_name"`
}

type StackDeployDetail struct {
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	Stack    struct {
		Name string `json:"name"`
	} `json:"stack"`
	Project struct {
		UUID string `json:"uuid"`
	} `json:"project"`
	Namespace string `json:"namespace"`
}

type KubeconfigResponse struct {
	Kubeconfig string `json:"kubeconfig"`
	Namespace  string `json:"namespace"`
}
