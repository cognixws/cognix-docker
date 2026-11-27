package engine

import "context"

// Row is a container summarized for a list: id, name, image, state, ports,
// health, and the actions valid from its current state.
type Row struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	State          string   `json:"state"`
	Status         string   `json:"status"`
	Project        string   `json:"project,omitempty"`
	Service        string   `json:"service,omitempty"`
	WorkingDir     string   `json:"workingDir,omitempty"`
	ComposeFiles   string   `json:"composeFiles,omitempty"`
	Health         string   `json:"health,omitempty"`
	HasHealthCheck bool     `json:"hasHealthCheck"`
	Ports          []string `json:"ports,omitempty"`
	Actions        []string `json:"actions"`
	CPUPercent     *float64 `json:"cpuPercent,omitempty"`
	MemoryBytes    uint64   `json:"memoryBytes,omitempty"`
	LimitBytes     uint64   `json:"limitBytes,omitempty"`
}

func RowOf(c Container) Row {
	var actions []string
	for _, verb := range []string{"start", "stop", "restart"} {
		if ValidAction(verb, c.State) {
			actions = append(actions, verb)
		}
	}
	return Row{
		ID: c.ID, Name: c.Name, Image: c.Image, State: c.State, Status: c.Status,
		Project: c.Project, Service: c.Service, WorkingDir: c.WorkingDir, ComposeFiles: c.ComposeFiles,
		Health: c.Health, HasHealthCheck: c.HasHealthCheck, Ports: c.Ports, Actions: actions,
	}
}

var ActionPast = map[string]string{"start": "started", "stop": "stopped", "restart": "restarted"}

func ValidAction(action, state string) bool {
	switch action {
	case "start":
		return state == "created" || state == "exited"
	case "stop":
		return state == "running" || state == "restarting"
	case "restart":
		return state == "running"
	}
	return false
}

// Detail is one container's state, mounts/networks, a resource sample and recent logs.
type Detail struct {
	Row
	StartedAt     string     `json:"startedAt,omitempty"`
	RestartCount  int        `json:"restartCount"`
	ExitCode      int        `json:"exitCode"`
	OOMKilled     bool       `json:"oomKilled"`
	Mounts        []Mount    `json:"mounts,omitempty"`
	NetworkNames  []string   `json:"networkNames,omitempty"`
	StatsError    string     `json:"statsError,omitempty"`
	Logs          string     `json:"logs,omitempty"`
	LogEntries    []LogEntry `json:"logEntries,omitempty"`
	LogsTruncated bool       `json:"logsTruncated,omitempty"`
	LogsError     string     `json:"logsError,omitempty"`
}

func BuildDetail(ctx context.Context, c *Client, id string) (Detail, error) {
	full, err := c.Inspect(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{
		Row:          RowOf(full),
		StartedAt:    full.StartedAt,
		RestartCount: full.RestartCount,
		ExitCode:     full.ExitCode,
		OOMKilled:    full.OOMKilled,
		Mounts:       full.Mounts,
		NetworkNames: full.NetworkNames,
	}
	if full.State == "running" {
		if s, err := c.Stats(ctx, id); err == nil {
			d.CPUPercent, d.MemoryBytes, d.LimitBytes = &s.CPUPercent, s.MemoryBytes, s.LimitBytes
		} else {
			d.StatsError = err.Error()
		}
	}
	if logs, err := c.Logs(ctx, full); err == nil {
		d.Logs, d.LogsTruncated = logs.Text, logs.Truncated
		d.LogEntries = ParseLogEntries(logs.Text, full.Service, full.Name)
	} else {
		d.LogsError = err.Error()
	}
	return d, nil
}
