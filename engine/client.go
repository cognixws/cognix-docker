// Package engine talks to the Docker Engine's own HTTP API over its Unix
// socket. It never executes a shell (except to ask an installed `docker`
// CLI which context is selected, purely to resolve the socket path) and
// never elevates privileges.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const apiVersion = "1.44"
const MaxLogBytes = 64 * 1024

var fullID = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Client struct {
	Endpoint string
	HTTP     *http.Client
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source,omitempty"`
	Name        string `json:"name,omitempty"`
	Destination string `json:"destination"`
	RW          bool   `json:"rw"`
}

type networkAttachment struct{ NetworkID string }

func networkIDs(networks map[string]networkAttachment) []string {
	ids := []string{}
	for _, n := range networks {
		if n.NetworkID != "" {
			ids = append(ids, n.NetworkID)
		}
	}
	sort.Strings(ids)
	return ids
}

func networkNames(networks map[string]networkAttachment) []string {
	names := []string{}
	for name := range networks {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

type Container struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	ImageID        string   `json:"imageId,omitempty"`
	State          string   `json:"state"`
	Status         string   `json:"status"`
	Project        string   `json:"project,omitempty"`
	Service        string   `json:"service,omitempty"`
	WorkingDir     string   `json:"workingDir,omitempty"`
	ComposeFiles   string   `json:"composeFiles,omitempty"`
	StartedAt      string   `json:"startedAt,omitempty"`
	Health         string   `json:"health,omitempty"`
	HasHealthCheck bool     `json:"hasHealthCheck"`
	RestartCount   int      `json:"restartCount"`
	ExitCode       int      `json:"exitCode"`
	OOMKilled      bool     `json:"oomKilled"`
	Ports          []string `json:"ports,omitempty"`
	Mounts         []Mount  `json:"mounts,omitempty"`
	Networks       []string `json:"networks,omitempty"`
	NetworkNames   []string `json:"networkNames,omitempty"`
	Secrets        []string `json:"-"`
	TTY            bool     `json:"-"`
}

type Stats struct {
	CPUPercent  float64 `json:"cpuPercent"`
	MemoryBytes uint64  `json:"memoryBytes"`
	LimitBytes  uint64  `json:"limitBytes"`
	SampledAt   string  `json:"sampledAt"`
}

type Logs struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	SampledAt string `json:"sampledAt"`
}

type LogEntry struct {
	Timestamp string `json:"ts"`
	Service   string `json:"service"`
	Container string `json:"container"`
	Level     string `json:"level"` // INF | WRN | ERR
	Message   string `json:"message"`
}

// EndpointFrom respects Docker's explicit context before DOCKER_HOST.
func EndpointFrom(env func(string) string, contextHost func() (string, error)) (string, error) {
	endpoint := strings.TrimSpace(env("PICODE_DOCKER_HOST"))
	if endpoint == "" {
		if env("DOCKER_CONTEXT") == "" {
			endpoint = strings.TrimSpace(env("DOCKER_HOST"))
		}
		if endpoint == "" {
			if contextHost == nil {
				return "", errors.New("choose a local Docker connection")
			}
			var err error
			endpoint, err = contextHost()
			if err != nil {
				return "", err
			}
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || !filepath.IsAbs(u.Path) || u.Path == "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("choose a local Docker connection: this extension supports Unix sockets only")
	}
	return "unix://" + filepath.Clean(u.Path), nil
}

func LocalClient(ctx context.Context) (*Client, error) {
	endpoint, err := EndpointFrom(os.Getenv, func() (string, error) {
		if _, err := exec.LookPath("docker"); err != nil {
			return "unix:///var/run/docker.sock", nil
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "docker", "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}")
		out, err := cmd.Output()
		if err != nil {
			return "", errors.New("Docker's connection could not be resolved: check the selected Docker context")
		}
		var endpoint string
		if err = json.Unmarshal(out, &endpoint); err != nil {
			return "", errors.New("Docker returned an invalid connection")
		}
		return endpoint, nil
	})
	if err != nil {
		return nil, err
	}
	return NewClient(endpoint)
}

func NewClient(endpoint string) (*Client, error) {
	endpoint, err := EndpointFrom(func(k string) string {
		if k == "PICODE_DOCKER_HOST" {
			return endpoint
		}
		return ""
	}, nil)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(endpoint)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", u.Path)
	}, MaxIdleConns: 6, IdleConnTimeout: 30 * time.Second}
	return &Client{Endpoint: endpoint, HTTP: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.HTTP.CloseIdleConnections() }

func (c *Client) request(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Docker is not reachable: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		var body struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body)
		if body.Message == "" {
			body.Message = "Docker returned " + res.Status
		}
		return nil, &APIError{Status: res.StatusCode, Message: body.Message}
	}
	return res, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	res, err := c.request(ctx, "GET", path)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(io.LimitReader(res.Body, 4*1024*1024)).Decode(out)
}

func (c *Client) Check(ctx context.Context) error {
	var v struct {
		APIVersion    string
		MinAPIVersion string
	}
	if err := c.get(ctx, "/version", &v); err != nil {
		return err
	}
	parse := func(v string) int {
		var a, b int
		if _, err := fmt.Sscanf(v, "%d.%d", &a, &b); err != nil {
			return 0
		}
		return a*1000 + b
	}
	if parse(v.APIVersion) < parse(apiVersion) || (v.MinAPIVersion != "" && parse(v.MinAPIVersion) > parse(apiVersion)) {
		return errors.New("this Docker API version is incompatible with this extension (requires API 1.44)")
	}
	return nil
}

func labels(c *Container, l map[string]string) {
	c.Project = l["com.docker.compose.project"]
	c.Service = l["com.docker.compose.service"]
	c.WorkingDir = l["com.docker.compose.project.working_dir"]
	c.ComposeFiles = l["com.docker.compose.project.config_files"]
}

type apiPort struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

func formatListPorts(ports []apiPort) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, p := range ports {
		if p.PublicPort <= 0 {
			continue
		}
		host := ""
		if p.IP != "" && p.IP != "0.0.0.0" && p.IP != "::" {
			host = p.IP
		}
		typ := p.Type
		if typ == "" {
			typ = "tcp"
		}
		s := fmt.Sprintf("%s:%d->%d/%s", host, p.PublicPort, p.PrivatePort, typ)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func formatInspectPorts(pm map[string][]struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}) []string {
	seen := map[string]bool{}
	out := []string{}
	for priv, binds := range pm {
		for _, b := range binds {
			if b.HostPort == "" {
				continue
			}
			host := ""
			if b.HostIP != "" && b.HostIP != "0.0.0.0" && b.HostIP != "::" {
				host = b.HostIP
			}
			s := fmt.Sprintf("%s:%s->%s", host, b.HostPort, priv)
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var rows []struct {
		ID                   string `json:"Id"`
		Names                []string
		Image, State, Status string
		ImageID              string
		Ports                []apiPort
		Mounts               []Mount
		NetworkSettings      struct{ Networks map[string]networkAttachment }
		Labels               map[string]string
	}
	if err := c.get(ctx, "/v"+apiVersion+"/containers/json?all=true", &rows); err != nil {
		return nil, err
	}
	out := []Container{}
	for _, r := range rows {
		v := Container{ID: r.ID, Image: r.Image, State: r.State, Status: r.Status, Name: r.ID[:min(12, len(r.ID))]}
		v.ImageID, v.Mounts = r.ImageID, r.Mounts
		v.Ports = formatListPorts(r.Ports)
		v.Networks = networkIDs(r.NetworkSettings.Networks)
		v.NetworkNames = networkNames(r.NetworkSettings.Networks)
		if strings.Contains(strings.ToLower(r.Status), "(healthy)") {
			v.Health, v.HasHealthCheck = "healthy", true
		} else if strings.Contains(strings.ToLower(r.Status), "(unhealthy)") {
			v.Health, v.HasHealthCheck = "unhealthy", true
		} else if strings.Contains(strings.ToLower(r.Status), "(health: starting)") {
			v.Health, v.HasHealthCheck = "starting", true
		}
		if len(r.Names) > 0 {
			v.Name = strings.TrimPrefix(r.Names[0], "/")
		}
		labels(&v, r.Labels)
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) Inspect(ctx context.Context, id string) (Container, error) {
	if !fullID.MatchString(id) {
		return Container{}, errors.New("select a container using its full ID")
	}
	var r struct {
		ID              string `json:"Id"`
		Name            string
		Image           string
		RestartCount    int
		Mounts          []Mount
		NetworkSettings struct {
			Networks map[string]networkAttachment
			Ports    map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			}
		}
		Config struct {
			Image       string
			Tty         bool
			Labels      map[string]string
			Env         []string
			Healthcheck *struct{ Test []string }
		}
		State struct {
			Status, StartedAt string
			Health            *struct{ Status string }
			ExitCode          int
			OOMKilled         bool
		}
	}
	if err := c.get(ctx, "/v"+apiVersion+"/containers/"+id+"/json", &r); err != nil {
		return Container{}, err
	}
	v := Container{ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), Image: r.Config.Image, State: r.State.Status, Status: r.State.Status, StartedAt: r.State.StartedAt, TTY: r.Config.Tty}
	v.ImageID, v.RestartCount, v.ExitCode, v.OOMKilled = r.Image, r.RestartCount, r.State.ExitCode, r.State.OOMKilled
	v.Mounts = r.Mounts
	v.Ports = formatInspectPorts(r.NetworkSettings.Ports)
	v.Networks = networkIDs(r.NetworkSettings.Networks)
	v.NetworkNames = networkNames(r.NetworkSettings.Networks)
	v.Secrets = sensitiveValues(r.Config.Env)
	v.HasHealthCheck = r.Config.Healthcheck != nil && len(r.Config.Healthcheck.Test) > 0 && r.Config.Healthcheck.Test[0] != "NONE"
	if r.State.Health != nil {
		v.Health = r.State.Health.Status
		v.HasHealthCheck = true
	}
	labels(&v, r.Config.Labels)
	if v.ID != id {
		return Container{}, errors.New("Docker returned a different container")
	}
	return v, nil
}

func (c *Client) Stats(ctx context.Context, id string) (Stats, error) {
	if !fullID.MatchString(id) {
		return Stats{}, errors.New("invalid container ID")
	}
	type cpu struct {
		CPUUsage struct {
			TotalUsage  uint64
			PercpuUsage []uint64
		}
		SystemCPUUsage uint64
		OnlineCPUs     uint64
	}
	var r struct {
		CPU    json.RawMessage `json:"cpu_stats"`
		Prev   json.RawMessage `json:"precpu_stats"`
		Memory *struct {
			Usage, Limit uint64
			Stats        map[string]uint64
		} `json:"memory_stats"`
	}
	if err := c.get(ctx, "/v"+apiVersion+"/containers/"+id+"/stats?stream=false", &r); err != nil {
		return Stats{}, err
	}
	if r.Memory == nil {
		return Stats{}, errors.New("Docker did not report a resource sample")
	}
	parseCPU := func(raw json.RawMessage) cpu {
		var x struct {
			Usage struct {
				Total uint64   `json:"total_usage"`
				Per   []uint64 `json:"percpu_usage"`
			} `json:"cpu_usage"`
			System uint64 `json:"system_cpu_usage"`
			Online uint64 `json:"online_cpus"`
		}
		_ = json.Unmarshal(raw, &x)
		var v cpu
		v.CPUUsage.TotalUsage = x.Usage.Total
		v.CPUUsage.PercpuUsage = x.Usage.Per
		v.SystemCPUUsage = x.System
		v.OnlineCPUs = x.Online
		return v
	}
	a, b := parseCPU(r.CPU), parseCPU(r.Prev)
	n := a.OnlineCPUs
	if n == 0 {
		n = uint64(len(a.CPUUsage.PercpuUsage))
	}
	if n == 0 || a.SystemCPUUsage <= b.SystemCPUUsage || a.CPUUsage.TotalUsage < b.CPUUsage.TotalUsage {
		return Stats{}, errors.New("Docker did not report a usable CPU sample")
	}
	s := Stats{MemoryBytes: r.Memory.Usage, LimitBytes: r.Memory.Limit, SampledAt: time.Now().UTC().Format(time.RFC3339)}
	cache := r.Memory.Stats["inactive_file"]
	if cache == 0 {
		cache = r.Memory.Stats["total_inactive_file"]
	}
	if cache < s.MemoryBytes {
		s.MemoryBytes -= cache
	}
	if a.SystemCPUUsage > b.SystemCPUUsage && a.CPUUsage.TotalUsage >= b.CPUUsage.TotalUsage {
		s.CPUPercent = float64(a.CPUUsage.TotalUsage-b.CPUUsage.TotalUsage) / float64(a.SystemCPUUsage-b.SystemCPUUsage) * float64(n) * 100
	}
	return s, nil
}

func (c *Client) Logs(ctx context.Context, container Container) (Logs, error) {
	return c.LogsTail(ctx, container, 200)
}

func (c *Client) LogsTail(ctx context.Context, container Container, tail int) (Logs, error) {
	if !fullID.MatchString(container.ID) {
		return Logs{}, errors.New("invalid container ID")
	}
	if tail <= 0 || tail > 1000 {
		tail = 200
	}
	res, err := c.request(ctx, "GET", "/v"+apiVersion+"/containers/"+container.ID+"/logs?stdout=true&stderr=true&timestamps=true&tail="+strconv.Itoa(tail))
	if err != nil {
		return Logs{}, err
	}
	defer res.Body.Close()
	text, truncated, err := ReadLogs(res.Body, container.TTY)
	text = Redact(text, container.Secrets)
	if len(text) > MaxLogBytes {
		text, truncated = strings.ToValidUTF8(text[:MaxLogBytes], ""), true
	}
	return Logs{Text: text, Truncated: truncated, SampledAt: time.Now().UTC().Format(time.RFC3339)}, err
}

// ParseLogEntries structures plain timestamped Docker log lines into entries
// with inferred severity so both single-container and multiplexed stack logs
// filter cleanly on the client.
func ParseLogEntries(text, service, containerName string) []LogEntry {
	out := []LogEntry{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		ts := ""
		msg := line
		if sp := strings.IndexByte(line, ' '); sp > 18 && sp < 40 && len(line) > 4 && line[4] == '-' {
			ts = line[:sp]
			msg = strings.TrimSpace(line[sp+1:])
		}
		lvl := classifyLevel(msg)
		svc := service
		if svc == "" {
			svc = containerName
		}
		out = append(out, LogEntry{Timestamp: ts, Service: svc, Container: containerName, Level: lvl, Message: msg})
	}
	return out
}

var errRe = regexp.MustCompile(`(?i)\b(error|err|fatal|panic|oomkilled|sigkill|exception|failed)\b`)
var wrnRe = regexp.MustCompile(`(?i)\b(warn|warning|wrn|deprecated|timeout|retry)\b`)

func classifyLevel(msg string) string {
	switch {
	case errRe.MatchString(msg):
		return "ERR"
	case wrnRe.MatchString(msg):
		return "WRN"
	default:
		return "INF"
	}
}

// Events streams container/image/volume/network events from the Engine socket.
func (c *Client) Events(ctx context.Context, onEvent func()) error {
	filters := url.QueryEscape(`{"type":["container","image","volume","network"]}`)
	res, err := c.request(ctx, "GET", "/v"+apiVersion+"/events?filters="+filters)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 16*1024), 256*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) > 0 && onEvent != nil {
			onEvent()
		}
	}
	return sc.Err()
}

var ansi = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))`)

// ReadLogs handles Docker's multiplex framing and TTY's raw byte stream.
func ReadLogs(r io.Reader, tty bool) (string, bool, error) {
	var out bytes.Buffer
	if tty {
		if _, err := io.Copy(&out, io.LimitReader(r, MaxLogBytes+1)); err != nil {
			return "", false, err
		}
	} else {
		for out.Len() <= MaxLogBytes {
			var head [8]byte
			_, err := io.ReadFull(r, head[:])
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", false, err
			}
			if head[0] > 2 || head[1] != 0 || head[2] != 0 || head[3] != 0 {
				return "", false, errors.New("invalid Docker log frame")
			}
			n := int64(binary.BigEndian.Uint32(head[4:]))
			remaining := int64(MaxLogBytes + 1 - out.Len())
			if _, err := io.CopyN(&out, r, min(n, remaining)); err != nil {
				return "", false, err
			}
			if n > remaining {
				break
			}
		}
	}
	truncated := out.Len() > MaxLogBytes
	raw := out.Bytes()
	if truncated {
		raw = raw[:MaxLogBytes]
	}
	clean := ansi.ReplaceAllString(strings.ToValidUTF8(string(raw), ""), "")
	clean = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, clean)
	return clean, truncated, nil
}

func (c *Client) Mutate(ctx context.Context, id, action string) error {
	if !fullID.MatchString(id) || (action != "start" && action != "stop" && action != "restart") {
		return errors.New("invalid Docker action")
	}
	path := "/v" + apiVersion + "/containers/" + id + "/" + action
	if action != "start" {
		path += "?t=10"
	}
	res, err := c.request(ctx, "POST", path)
	if err == nil {
		_ = res.Body.Close()
	}
	return err
}

var secretName = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|private[_-]?key|credential)`)
var labeledSecret = regexp.MustCompile(`(?i)([a-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|private[_-]?key|credential)[a-z0-9_.-]*["']?\s*[:=]\s*)(?:"[^"\n]*"|'[^'\n]*'|[^\s,;]+)`)
var bearerSecret = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`)
var urlSecret = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@]+:[^\s/@]+@`)

func sensitiveValues(env []string) []string {
	values := []string{}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && len(value) >= 4 && (secretName.MatchString(name) || urlSecret.MatchString(value)) {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

// Redact is best-effort masking, not interpretation of application output.
func Redact(text string, secrets []string) string {
	for _, secret := range secrets {
		if len(secret) >= 4 {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	text = labeledSecret.ReplaceAllString(text, "${1}[redacted]")
	text = bearerSecret.ReplaceAllString(text, "Bearer [redacted]")
	return urlSecret.ReplaceAllString(text, "${1}[redacted]@")
}
