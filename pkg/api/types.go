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
