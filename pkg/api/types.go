package api

type App struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type Pod struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Ready    bool   `json:"ready"`
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
	UUID      string         `json:"uuid"`
	Name      string         `json:"display_name"`
	Namespace string         `json:"namespace"`
	Cluster   ProjectCluster `json:"cluster"`
	CreatedAt string         `json:"created_at"`
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
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Phase     string `json:"phase"`
	Status    string `json:"status,omitempty"`
	BuildMode string `json:"build_mode,omitempty"`
	Image     string `json:"image,omitempty"`
	ImageTag  string `json:"image_tag,omitempty"`
	GitRef    string `json:"git_ref,omitempty"`
	GitURL    string `json:"git_url,omitempty"`
	Message   string `json:"message,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Chart     *struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		Repository string `json:"repository"`
	} `json:"chart,omitempty"`
	AppSpec *AppSpec `json:"appSpec,omitempty"`
}

type AppSpec struct {
	DisplayName    string `json:"displayName,omitempty"`
	RegistrySecret string `json:"registrySecret,omitempty"`
	Mode           string `json:"mode,omitempty"`
	Image          string `json:"image,omitempty"`
	HelmChart      *struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		Repository string `json:"repository"`
	} `json:"helmChart,omitempty"`
}

type StackDeployApp struct {
	UUID      string             `json:"uuid"`
	Stack     StackDeployStack   `json:"stack"`
	Cluster   StackDeployCluster `json:"cluster"`
	Project   StackDeployProject `json:"project"`
	Namespace string             `json:"namespace"`
	Name      string             `json:"name"`
	Status    string             `json:"status"`
	CreatedAt string             `json:"created_at"`
	UpdatedAt string             `json:"updated_at"`
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
	UUID string `json:"uuid"`
	Name string `json:"display_name"`
}

type StackDeployDetail struct {
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
	Stack struct {
		Name string `json:"name"`
	} `json:"stack"`
	Project struct {
		UUID string `json:"uuid"`
	} `json:"project"`
	Namespace string `json:"namespace"`
}

type Parameter struct {
	Name         string      `json:"name"`
	Description  string      `json:"description"`
	Type         string      `json:"type"`
	Value        interface{} `json:"value"`
	DefaultValue interface{} `json:"defaultValue"`
}

type StackDeployDetailWithComponents struct {
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
	Stack struct {
		Name string `json:"name"`
	} `json:"stack"`
	Project struct {
		UUID string `json:"uuid"`
	} `json:"project"`
	Namespace  string      `json:"namespace"`
	Components []Component `json:"components"`
	Parameters []Parameter `json:"parameters"`
}

type KubeconfigResponse struct {
	Kubeconfig string `json:"kubeconfig"`
	Namespace  string `json:"namespace"`
}

type Registry struct {
	UUID      string `json:"uuid"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
	CreatedBy struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"created_by"`
}
