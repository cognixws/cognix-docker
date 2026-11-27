package sysadmin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cfpperche/picode-docker/engine"
)

type WorkspaceInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	ExtensionOn bool   `json:"extensionOn"`
}

type AgentInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CLI         string `json:"cli"`
	WorkspaceID string `json:"workspaceId"`
	Running     bool   `json:"running"`
	State       string `json:"state"`
	WorkDir     string `json:"workDir,omitempty"`
}

// HostClient talks to COGNIX[WS]'s Host API v1 over loopback.
type HostClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewHostClientFromEnv() *HostClient {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("PICODE_URL")), "/")
	tok := strings.TrimSpace(os.Getenv("PICODE_EXT_TOKEN"))
	caFile := strings.TrimSpace(os.Getenv("PICODE_CA_FILE"))

	tr := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	if strings.HasPrefix(base, "https://") {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		loaded := false
		if caFile != "" {
			if pem, err := os.ReadFile(caFile); err == nil {
				pool := x509.NewCertPool()
				if pool.AppendCertsFromPEM(pem) {
					tlsCfg.RootCAs = pool
					loaded = true
				}
			}
		}
		if !loaded {
			if u, err := url.Parse(base); err == nil {
				h := u.Hostname()
				if h == "127.0.0.1" || h == "localhost" || h == "::1" {
					tlsCfg.InsecureSkipVerify = true
				}
			}
		}
		tr.TLSClientConfig = tlsCfg
	}
	return &HostClient{
		BaseURL: base,
		Token:   tok,
		HTTP:    &http.Client{Transport: tr, Timeout: 8 * time.Second},
	}
}

func (h *HostClient) Available() bool {
	return h != nil && h.BaseURL != "" && h.Token != ""
}

func (h *HostClient) doJSON(ctx context.Context, method, path string, in any, out any) error {
	if !h.Available() {
		return errors.New("host API not configured")
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-PiCode-Extension-Token", h.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := h.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("host API %s returned %d: %s", path, res.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && res.StatusCode != http.StatusNoContent {
		return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
	}
	return nil
}

func (h *HostClient) Workspaces(ctx context.Context) ([]WorkspaceInfo, error) {
	if !h.Available() {
		return nil, nil
	}
	var res struct {
		Workspaces []WorkspaceInfo `json:"workspaces"`
	}
	if err := h.doJSON(ctx, http.MethodGet, "/api/ext/v1/workspaces", nil, &res); err != nil {
		return nil, err
	}
	return res.Workspaces, nil
}

func (h *HostClient) AgentActor(ctx context.Context, agentID string) string {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return "agent"
	}
	if h.Available() {
		var res struct {
			Agents []AgentInfo `json:"agents"`
		}
		if err := h.doJSON(ctx, http.MethodGet, "/api/ext/v1/agents", nil, &res); err == nil {
			for _, a := range res.Agents {
				if a.ID == agentID && strings.TrimSpace(a.Name) != "" {
					return "agent:" + a.Name
				}
			}
		}
	}
	return "agent:" + agentID
}

func (h *HostClient) FileInboxReview(ctx context.Context, title, body, workspaceID string) (string, error) {
	if !h.Available() {
		return "", nil
	}
	var res struct {
		ID string `json:"id"`
	}
	err := h.doJSON(ctx, http.MethodPost, "/api/ext/v1/inbox", map[string]string{
		"title":       title,
		"body":        body,
		"workspaceId": workspaceID,
	}, &res)
	return res.ID, err
}

func (h *HostClient) PublishChanged(ctx context.Context, reason string) {
	if !h.Available() {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = h.doJSON(cctx, http.MethodPost, "/api/ext/v1/events", map[string]any{
		"name": "changed",
		"data": map[string]string{"reason": reason, "at": nowUTC()},
	}, nil)
}

// ---------- Service ----------

type Service struct {
	Store         *Store
	Host          *HostClient
	Connect       func(context.Context) (*engine.Client, error)
	ComposeRunner CommandRunner
}

func NewService(st *Store, host *HostClient) *Service {
	return &Service{
		Store:         st,
		Host:          host,
		Connect:       engine.Connect,
		ComposeRunner: DefaultCommandRunner,
	}
}

func (s *Service) client(ctx context.Context) (*engine.Client, error) {
	if s.Connect == nil {
		return engine.Connect(ctx)
	}
	return s.Connect(ctx)
}

func (s *Service) StartBackground(ctx context.Context) error {
	if err := s.Store.Recover(); err != nil {
		return err
	}
	go s.watchEngineEvents(ctx)
	go s.monitorLoop(ctx)
	return nil
}

func (s *Service) watchEngineEvents(ctx context.Context) {
	var mu sync.Mutex
	var timer *time.Timer
	notify := func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			return
		}
		timer = time.AfterFunc(350*time.Millisecond, func() {
			mu.Lock()
			timer = nil
			mu.Unlock()
			s.Host.PublishChanged(ctx, "engine")
		})
	}
	for {
		if ctx.Err() != nil {
			return
		}
		c, err := s.client(ctx)
		if err == nil {
			_ = c.Events(ctx, notify)
			c.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(4 * time.Second):
		}
	}
}

func (s *Service) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	sem := make(chan struct{}, 4)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			monitors, err := s.Store.Monitors()
			if err != nil {
				continue
			}
			now := time.Now().UTC()
			var wg sync.WaitGroup
			for _, m := range monitors {
				if !m.Enabled {
					continue
				}
				last, _ := time.Parse(time.RFC3339Nano, m.SampledAt)
				if !last.IsZero() && now.Sub(last) < time.Duration(m.IntervalSeconds)*time.Second {
					continue
				}
				wg.Add(1)
				sem <- struct{}{}
				go func(mon Monitor) {
					defer wg.Done()
					defer func() { <-sem }()
					cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
					defer cancel()
					if changed, _ := s.evaluateMonitor(cctx, mon); changed {
						s.Host.PublishChanged(ctx, "health")
					}
				}(m)
			}
			wg.Wait()
		}
	}
}

// ---------- Overview & Stack Queries ----------

type StackGroup struct {
	Project        string               `json:"project"`
	WorkingDir     string               `json:"workingDir,omitempty"`
	ComposeFiles   string               `json:"composeFiles,omitempty"`
	RunningCount   int                  `json:"runningCount"`
	TotalCount     int                  `json:"totalCount"`
	UnhealthyCount int                  `json:"unhealthyCount"`
	Monitored      bool                 `json:"monitored"`
	Registered     *ComposeRegistration `json:"registered,omitempty"`
	Containers     []engine.Row         `json:"containers"`
}

type OverviewResponse struct {
	Reachable bool   `json:"reachable"`
	Endpoint  string `json:"endpoint,omitempty"`
	Error     string `json:"error,omitempty"`
	// Problem says which case it is when Docker cannot be used, with its
	// one action (engine.ProblemOf).
	Problem           *engine.Problem       `json:"problem,omitempty"`
	TotalContainers   int                   `json:"totalContainers"`
	RunningContainers int                   `json:"runningContainers"`
	Groups            []StackGroup          `json:"groups"`
	OpenIncidents     []Incident            `json:"openIncidents"`
	PendingPlans      []Plan                `json:"pendingPlans"`
	ComposeProjects   []ComposeRegistration `json:"composeProjects"`
	OpenCompose       *ComposePreview       `json:"openCompose,omitempty"`
	OpenComposeReg    *ComposeRegistration  `json:"openComposeReg,omitempty"`
}

func (s *Service) Overview(ctx context.Context, openPath string, includeStats bool) (OverviewResponse, error) {
	incidents, _ := s.Store.OpenIncidents()
	plans, _ := s.Store.PendingPlans()
	regs, _ := s.Store.ComposeProjects()
	monitors, _ := s.Store.Monitors()

	out := OverviewResponse{
		Groups:          []StackGroup{},
		OpenIncidents:   nonNilSlice(incidents),
		PendingPlans:    nonNilSlice(plans),
		ComposeProjects: nonNilSlice(regs),
	}

	c, err := s.client(ctx)
	if err != nil {
		out.Reachable = false
		out.Error = humanizeDockerError(err)
		var nr *engine.NotReadyError
		if errors.As(err, &nr) {
			p := engine.ProblemOf(nr.Diagnosis)
			out.Problem = &p
		}
		return out, nil
	}
	defer c.Close()

	rows, err := c.Containers(ctx)
	if err != nil {
		out.Reachable = false
		out.Endpoint = c.Endpoint
		out.Error = humanizeDockerError(err)
		return out, nil
	}
	out.Reachable = true
	out.Endpoint = c.Endpoint
	out.TotalContainers = len(rows)

	regMap := map[string]ComposeRegistration{}
	for _, r := range regs {
		regMap[r.Project] = r
	}
	monMap := map[string]bool{}
	for _, m := range monitors {
		if m.Enabled && (m.Endpoint == c.Endpoint || m.Endpoint == "") {
			monMap[m.Project] = true
		}
	}

	// Sample lightweight stats concurrently for up to 12 running containers when requested.
	statsByID := map[string]engine.Stats{}
	if includeStats {
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		sampled := 0
		for _, ct := range rows {
			if ct.State != "running" || sampled >= 12 {
				continue
			}
			sampled++
			wg.Add(1)
			sem <- struct{}{}
			go func(id string) {
				defer wg.Done()
				defer func() { <-sem }()
				sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				if st, e := c.Stats(sctx, id); e == nil {
					mu.Lock()
					statsByID[id] = st
					mu.Unlock()
				}
			}(ct.ID)
		}
		wg.Wait()
	}

	byProj := map[string]*StackGroup{}
	for _, ct := range rows {
		if ct.State == "running" {
			out.RunningContainers++
		}
		g := byProj[ct.Project]
		if g == nil {
			g = &StackGroup{
				Project:    ct.Project,
				Monitored:  monMap[ct.Project],
				Containers: []engine.Row{},
			}
			if reg, ok := regMap[ct.Project]; ok {
				cp := reg
				g.Registered = &cp
				g.WorkingDir = reg.WorkDir
			}
			byProj[ct.Project] = g
		}
		if g.WorkingDir == "" && ct.WorkingDir != "" {
			g.WorkingDir = ct.WorkingDir
		}
		if g.ComposeFiles == "" && ct.ComposeFiles != "" {
			g.ComposeFiles = ct.ComposeFiles
		}
		g.TotalCount++
		if ct.State == "running" {
			g.RunningCount++
		}
		if ct.Health == "unhealthy" || ct.State == "restarting" || ct.OOMKilled {
			g.UnhealthyCount++
		}
		row := engine.RowOf(ct)
		if st, ok := statsByID[ct.ID]; ok {
			cpu := st.CPUPercent
			row.CPUPercent = &cpu
			row.MemoryBytes = st.MemoryBytes
			row.LimitBytes = st.LimitBytes
		}
		g.Containers = append(g.Containers, row)
	}

	// Include registered Compose stacks that currently have zero containers on the host.
	for _, reg := range regs {
		if _, exists := byProj[reg.Project]; !exists {
			cp := reg
			byProj[reg.Project] = &StackGroup{
				Project:      reg.Project,
				WorkingDir:   reg.WorkDir,
				ComposeFiles: strings.Join(reg.Files, ","),
				Monitored:    monMap[reg.Project],
				Registered:   &cp,
				Containers:   []engine.Row{},
			}
		}
	}

	names := make([]string, 0, len(byProj))
	for name := range byProj {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "") != (names[j] == "") {
			return names[j] == ""
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		out.Groups = append(out.Groups, *byProj[name])
	}

	if strings.TrimSpace(openPath) != "" {
		if reg, e := ResolveComposePath(openPath); e == nil {
			ws, _ := s.Host.Workspaces(ctx)
			if prev, pe := PreviewCompose(ctx, s.ComposeRunner, c, reg, ws); pe == nil {
				out.OpenCompose = &prev
				out.OpenComposeReg = &reg
			}
		}
	}
	return out, nil
}

func humanizeDockerError(err error) string {
	if err == nil {
		return ""
	}
	var nr *engine.NotReadyError
	if errors.As(err, &nr) {
		return nr.Error() // already says which case
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not reachable") || strings.Contains(lower, "no such file") || strings.Contains(lower, "connection refused") || strings.Contains(lower, "permission denied"):
		return "Docker is stopped or this account lacks permission to connect, so start Docker and check again."
	case strings.Contains(lower, "unix sockets only") || strings.Contains(lower, "local docker connection"):
		return "Your active Docker context points to a remote host, so switch to a local context and check again."
	default:
		return msg
	}
}

type DetailWithShell struct {
	engine.Detail
	ShellCommand string `json:"shellCommand"`
}

func (s *Service) ContainerDetail(ctx context.Context, id string) (DetailWithShell, error) {
	c, err := s.client(ctx)
	if err != nil {
		return DetailWithShell{}, err
	}
	defer c.Close()
	d, err := engine.BuildDetail(ctx, c, id)
	if err != nil {
		return DetailWithShell{}, err
	}
	target := d.Name
	if target == "" {
		target = d.ID[:min(12, len(d.ID))]
	}
	return DetailWithShell{
		Detail:       d,
		ShellCommand: "docker exec -it " + target + " sh",
	}, nil
}

type StackLogsResponse struct {
	Project  string            `json:"project"`
	Services []string          `json:"services"`
	Entries  []engine.LogEntry `json:"entries"`
}

func (s *Service) StackLogs(ctx context.Context, project string, tail int) (StackLogsResponse, error) {
	c, err := s.client(ctx)
	if err != nil {
		return StackLogsResponse{}, err
	}
	defer c.Close()
	containers, err := c.Containers(ctx)
	if err != nil {
		return StackLogsResponse{}, err
	}
	if tail <= 0 || tail > 400 {
		tail = 120
	}
	out := StackLogsResponse{Project: project, Services: []string{}, Entries: []engine.LogEntry{}}
	seenSvc := map[string]bool{}
	for _, ct := range containers {
		if ct.Project != project {
			continue
		}
		full, err := c.Inspect(ctx, ct.ID)
		if err != nil {
			continue
		}
		svcName := full.Service
		if svcName == "" {
			svcName = full.Name
		}
		if !seenSvc[svcName] {
			seenSvc[svcName] = true
			out.Services = append(out.Services, svcName)
		}
		if lg, err := c.LogsTail(ctx, full, tail); err == nil {
			out.Entries = append(out.Entries, engine.ParseLogEntries(lg.Text, svcName, full.Name)...)
		}
	}
	sort.Strings(out.Services)
	sort.SliceStable(out.Entries, func(i, j int) bool {
		if out.Entries[i].Timestamp == out.Entries[j].Timestamp {
			return out.Entries[i].Service < out.Entries[j].Service
		}
		return out.Entries[i].Timestamp < out.Entries[j].Timestamp
	})
	if len(out.Entries) > 600 {
		out.Entries = out.Entries[len(out.Entries)-600:]
	}
	return out, nil
}

// ---------- Audited Container Mutation ----------

func (s *Service) MutateContainer(ctx context.Context, id, action, requestKey, actor, agentID string) (Operation, error) {
	if action != "start" && action != "stop" && action != "restart" {
		return Operation{}, errors.New("action must be start, stop or restart")
	}
	if strings.TrimSpace(requestKey) == "" {
		requestKey = newID("req")
	}
	c, err := s.client(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer c.Close()

	before, err := c.Inspect(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	if !engine.ValidAction(action, before.State) {
		return Operation{}, fmt.Errorf("%s cannot %s while %s", before.Name, action, before.State)
	}

	op, fresh, err := s.Store.BeginOperation(Operation{
		RequestKey:    requestKey,
		Endpoint:      c.Endpoint,
		ContainerID:   before.ID,
		ContainerName: before.Name,
		Action:        action,
		Actor:         actor,
		AgentID:       agentID,
	})
	if err != nil {
		return Operation{}, err
	}
	if !fresh {
		return op, nil
	}

	mutErr := c.Mutate(ctx, before.ID, action)
	state, msg := "succeeded", fmt.Sprintf("%s %s.", strings.Title(engine.ActionPast[action]), before.Name)
	if mutErr != nil {
		state, msg = "failed", mutErr.Error()
	} else {
		after, insErr := c.Inspect(ctx, before.ID)
		switch {
		case insErr != nil:
			state, msg = "unknown", "Action sent, but final container state could not be verified."
		case (action == "start" || action == "restart") && after.State != "running":
			state, msg = "failed", fmt.Sprintf("%s is %s after %s.", before.Name, after.State, action)
		case action == "stop" && after.State == "running":
			state, msg = "failed", fmt.Sprintf("%s is still running after stop.", before.Name)
		}
	}
	_ = s.Store.FinishOperation(op.ID, state, msg)
	s.Host.PublishChanged(ctx, "container")
	updated, _ := s.Store.Operation(op.ID)
	if updated.State == "failed" {
		return updated, errors.New(updated.Message)
	}
	return updated, nil
}

// ---------- Health Evaluation & Diagnosis ----------

type HealthSnapshot struct {
	Endpoint   string       `json:"endpoint"`
	Project    string       `json:"project"`
	SampledAt  string       `json:"sampledAt"`
	Signals    []Signal     `json:"signals"`
	Containers []engine.Row `json:"containers"`
}

func (s *Service) CheckProjectHealth(ctx context.Context, project, actor string) (Monitor, []Incident, HealthSnapshot, error) {
	c, err := s.client(ctx)
	if err != nil {
		return Monitor{}, nil, HealthSnapshot{}, err
	}
	defer c.Close()

	mon, err := s.Store.Monitor(c.Endpoint, project)
	if errors.Is(err, sql.ErrNoRows) {
		mon = DefaultMonitor(c.Endpoint, project)
		mon.Actor = actor
		mon, err = s.Store.SaveMonitor(mon)
	}
	if err != nil {
		return Monitor{}, nil, HealthSnapshot{}, err
	}

	snap, signals, err := collectProjectHealth(ctx, c, mon)
	if err != nil {
		return mon, nil, HealthSnapshot{}, err
	}
	rawSnap, _ := json.Marshal(snap)
	if err := s.Store.RecordHealth(c.Endpoint, project, mon.Revision, rawSnap, signals, time.Now().UTC()); err != nil && !errors.Is(err, ErrConflict) {
		return mon, nil, snap, err
	}
	mon, _ = s.Store.Monitor(c.Endpoint, project)
	incs, _ := s.Store.Incidents(c.Endpoint, project)
	return mon, nonNilSlice(incs), snap, nil
}

func (s *Service) evaluateMonitor(ctx context.Context, mon Monitor) (bool, error) {
	c, err := s.client(ctx)
	if err != nil {
		return false, err
	}
	defer c.Close()
	if c.Endpoint != mon.Endpoint {
		return false, nil
	}
	beforeOpen, _ := s.Store.OpenIncidents()
	snap, signals, err := collectProjectHealth(ctx, c, mon)
	if err != nil {
		return false, err
	}
	rawSnap, _ := json.Marshal(snap)
	if err := s.Store.RecordHealth(mon.Endpoint, mon.Project, mon.Revision, rawSnap, signals, time.Now().UTC()); err != nil {
		return false, err
	}
	afterOpen, _ := s.Store.OpenIncidents()
	return len(beforeOpen) != len(afterOpen), nil
}

func collectProjectHealth(ctx context.Context, c *engine.Client, mon Monitor) (HealthSnapshot, []Signal, error) {
	list, err := c.Containers(ctx)
	if err != nil {
		return HealthSnapshot{}, nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	snap := HealthSnapshot{Endpoint: c.Endpoint, Project: mon.Project, SampledAt: now, Signals: []Signal{}, Containers: []engine.Row{}}
	signals := []Signal{}

	matched := 0
	for _, ct := range list {
		if ct.Project != mon.Project {
			continue
		}
		matched++
		full, err := c.Inspect(ctx, ct.ID)
		if err != nil {
			continue
		}
		row := engine.RowOf(full)

		if full.OOMKilled {
			signals = append(signals, Signal{Key: "oom:" + full.Name, Title: full.Name + " ran out of memory", State: "bad", Detail: "Docker killed this container after exhausting its memory limit."})
		} else {
			signals = append(signals, Signal{Key: "oom:" + full.Name, Title: full.Name + " memory limit", State: "good", Detail: "No out-of-memory kill recorded."})
		}

		if full.State == "restarting" || (full.State == "exited" && full.ExitCode != 0) {
			signals = append(signals, Signal{Key: "state:" + full.Name, Title: full.Name + " exited unexpectedly", State: "bad", Detail: fmt.Sprintf("State is %s (exit code %d, restarts %d).", full.State, full.ExitCode, full.RestartCount)})
		} else {
			signals = append(signals, Signal{Key: "state:" + full.Name, Title: full.Name + " state", State: "good", Detail: "State is " + full.State + "."})
		}

		if full.HasHealthCheck {
			switch full.Health {
			case "unhealthy":
				signals = append(signals, Signal{Key: "health:" + full.Name, Title: full.Name + " failed its health check", State: "bad", Detail: "Container health probe reported unhealthy."})
			case "healthy":
				signals = append(signals, Signal{Key: "health:" + full.Name, Title: full.Name + " health check", State: "good", Detail: "Container health probe is healthy."})
			default:
				signals = append(signals, Signal{Key: "health:" + full.Name, Title: full.Name + " health check starting", State: "unknown", Detail: "Health probe has not settled yet."})
			}
		}

		if full.State == "running" {
			if st, e := c.Stats(ctx, full.ID); e == nil {
				cpu := st.CPUPercent
				row.CPUPercent = &cpu
				row.MemoryBytes = st.MemoryBytes
				row.LimitBytes = st.LimitBytes
				if int(st.CPUPercent) >= mon.CPUPercent {
					signals = append(signals, Signal{Key: "cpu:" + full.Name, Title: full.Name + " sustained high CPU", State: "bad", Detail: fmt.Sprintf("CPU %.1f%% exceeds threshold %d%%.", st.CPUPercent, mon.CPUPercent)})
				} else {
					signals = append(signals, Signal{Key: "cpu:" + full.Name, Title: full.Name + " CPU", State: "good", Detail: fmt.Sprintf("CPU %.1f%%.", st.CPUPercent)})
				}
				if st.LimitBytes > 0 {
					memPct := int(float64(st.MemoryBytes) * 100 / float64(st.LimitBytes))
					if memPct >= mon.MemoryPercent {
						signals = append(signals, Signal{Key: "mem:" + full.Name, Title: full.Name + " sustained high memory", State: "bad", Detail: fmt.Sprintf("Memory %d%% exceeds threshold %d%%.", memPct, mon.MemoryPercent)})
					} else {
						signals = append(signals, Signal{Key: "mem:" + full.Name, Title: full.Name + " memory", State: "good", Detail: fmt.Sprintf("Memory %d%%.", memPct)})
					}
				}
			}
		}
		snap.Containers = append(snap.Containers, row)
	}
	if matched == 0 {
		signals = append(signals, Signal{Key: "project:empty", Title: "No containers found", State: "unknown", Detail: "No containers exist for this stack right now."})
	}
	snap.Signals = signals
	return snap, signals, nil
}

type Finding struct {
	Severity      string `json:"severity"` // critical | warning | info
	Title         string `json:"title"`
	Observation   string `json:"observation"`
	Hypothesis    string `json:"hypothesis"`
	Procedure     string `json:"procedure,omitempty"` // restart-unhealthy | start-stopped | restart-high-memory
	ContainerID   string `json:"containerId,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
	LogExcerpt    string `json:"logExcerpt,omitempty"`
}

type Diagnosis struct {
	Endpoint  string    `json:"endpoint"`
	Project   string    `json:"project"`
	SampledAt string    `json:"sampledAt"`
	Summary   string    `json:"summary"`
	Findings  []Finding `json:"findings"`
}

func (s *Service) Diagnose(ctx context.Context, project string) (Diagnosis, error) {
	c, err := s.client(ctx)
	if err != nil {
		return Diagnosis{}, err
	}
	defer c.Close()

	list, err := c.Containers(ctx)
	if err != nil {
		return Diagnosis{}, err
	}
	out := Diagnosis{
		Endpoint:  c.Endpoint,
		Project:   project,
		SampledAt: time.Now().UTC().Format(time.RFC3339),
		Findings:  []Finding{},
	}
	count := 0
	for _, ct := range list {
		if ct.Project != project {
			continue
		}
		count++
		full, err := c.Inspect(ctx, ct.ID)
		if err != nil {
			continue
		}
		excerpt := ""
		if lg, e := c.LogsTail(ctx, full, 20); e == nil && strings.TrimSpace(lg.Text) != "" {
			lines := strings.Split(strings.TrimSpace(lg.Text), "\n")
			if len(lines) > 6 {
				lines = lines[len(lines)-6:]
			}
			excerpt = strings.Join(lines, "\n")
		}

		if full.OOMKilled {
			out.Findings = append(out.Findings, Finding{
				Severity:      "critical",
				Title:         full.Name + " was killed for exceeding memory",
				Observation:   fmt.Sprintf("Container %s has OOMKilled=true (exit code %d, %d restarts).", full.Name, full.ExitCode, full.RestartCount),
				Hypothesis:    "Its memory limit is lower than peak workload demand or a process leaked memory.",
				Procedure:     "restart-unhealthy",
				ContainerID:   full.ID,
				ContainerName: full.Name,
				LogExcerpt:    excerpt,
			})
		} else if full.State == "restarting" || (full.State == "exited" && full.ExitCode != 0) {
			out.Findings = append(out.Findings, Finding{
				Severity:      "critical",
				Title:         full.Name + " exited with an error",
				Observation:   fmt.Sprintf("Container %s is %s with exit code %d (%d restarts).", full.Name, full.State, full.ExitCode, full.RestartCount),
				Hypothesis:    "Configuration, missing environment variable, or startup dependency failed.",
				Procedure:     "start-stopped",
				ContainerID:   full.ID,
				ContainerName: full.Name,
				LogExcerpt:    excerpt,
			})
		} else if full.Health == "unhealthy" {
			out.Findings = append(out.Findings, Finding{
				Severity:      "critical",
				Title:         full.Name + " is failing its health probe",
				Observation:   fmt.Sprintf("Container %s is running, but Docker's health probe reports unhealthy (%d restarts).", full.Name, full.RestartCount),
				Hypothesis:    "The service inside the container is hung or rejecting local probe requests.",
				Procedure:     "restart-unhealthy",
				ContainerID:   full.ID,
				ContainerName: full.Name,
				LogExcerpt:    excerpt,
			})
		} else if full.State == "exited" || full.State == "created" {
			out.Findings = append(out.Findings, Finding{
				Severity:      "warning",
				Title:         full.Name + " is stopped",
				Observation:   fmt.Sprintf("Container %s is %s (exit code %d).", full.Name, full.State, full.ExitCode),
				Hypothesis:    "The service was stopped earlier or never started.",
				Procedure:     "start-stopped",
				ContainerID:   full.ID,
				ContainerName: full.Name,
			})
		}

		if full.State == "running" {
			if st, e := c.Stats(ctx, full.ID); e == nil && st.LimitBytes > 0 {
				pct := int(float64(st.MemoryBytes) * 100 / float64(st.LimitBytes))
				if pct >= 85 {
					out.Findings = append(out.Findings, Finding{
						Severity:      "warning",
						Title:         full.Name + " is using high memory",
						Observation:   fmt.Sprintf("Memory usage is %d%% (%d MiB of %d MiB).", pct, st.MemoryBytes/(1024*1024), st.LimitBytes/(1024*1024)),
						Hypothesis:    "Sustained cache growth or workload spike is approaching the container's limit.",
						Procedure:     "restart-high-memory",
						ContainerID:   full.ID,
						ContainerName: full.Name,
					})
				}
			}
			if !full.HasHealthCheck {
				out.Findings = append(out.Findings, Finding{
					Severity:      "info",
					Title:         full.Name + " has no health check",
					Observation:   fmt.Sprintf("Container %s is running without a Docker HEALTHCHECK instruction.", full.Name),
					Hypothesis:    "Docker can tell if the process exits, but cannot detect an unresponsive port.",
					ContainerID:   full.ID,
					ContainerName: full.Name,
				})
			}
		}
	}

	switch {
	case count == 0:
		out.Summary = "No containers exist for this stack."
	case len(out.Findings) == 0:
		out.Summary = fmt.Sprintf("All %d containers in %s are running cleanly.", count, displayProject(project))
	default:
		out.Summary = fmt.Sprintf("%d finding(s) across %d container(s) in %s.", len(out.Findings), count, displayProject(project))
	}
	return out, nil
}

func displayProject(project string) string {
	if project == "" {
		return "Standalone containers"
	}
	return project
}

// ---------- Plans, Execution & UI Confirmations ----------

type PlanInput struct {
	Kind         string              `json:"kind"` // container | project | resource | procedure | compose
	Project      string              `json:"project,omitempty"`
	WorkspaceID  string              `json:"workspaceId,omitempty"`
	ContainerID  string              `json:"containerId,omitempty"`
	Action       string              `json:"action,omitempty"`    // start | stop | restart
	Procedure    string              `json:"procedure,omitempty"` // restart-unhealthy | start-stopped | restart-high-memory
	ResourceKind string              `json:"resourceKind,omitempty"`
	ResourceID   string              `json:"resourceId,omitempty"`
	Compose      ComposeRegistration `json:"compose,omitempty"`
	Actor        string              `json:"actor"`
	AgentID      string              `json:"agentId,omitempty"`
}

func containerFingerprint(ct engine.Container) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%d", ct.ID, ct.ImageID, ct.State, ct.Health, ct.RestartCount)))
	return hex.EncodeToString(sum[:16])
}

func (s *Service) PreviewPlan(ctx context.Context, in PlanInput) (Plan, error) {
	c, err := s.client(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer c.Close()

	rawIn, _ := json.Marshal(in)
	p := Plan{
		Kind:        in.Kind,
		Endpoint:    c.Endpoint,
		Project:     in.Project,
		WorkspaceID: in.WorkspaceID,
		Actor:       in.Actor,
		AgentID:     in.AgentID,
		Input:       rawIn,
		Steps:       []Step{},
	}

	switch in.Kind {
	case "container":
		if in.Action != "stop" && in.Action != "restart" && in.Action != "start" {
			return Plan{}, errors.New("action must be start, stop or restart")
		}
		full, err := c.Inspect(ctx, in.ContainerID)
		if err != nil {
			return Plan{}, err
		}
		if !engine.ValidAction(in.Action, full.State) {
			return Plan{}, fmt.Errorf("%s cannot %s while %s", full.Name, in.Action, full.State)
		}
		p.Project = full.Project
		p.Title = fmt.Sprintf("%s container %s", strings.Title(in.Action), full.Name)
		p.Impact = fmt.Sprintf("Will %s %s (%s). Volumes and configuration stay intact.", in.Action, full.Name, full.Image)
		fp := containerFingerprint(full)
		p.Fingerprint = fp
		p.Steps = append(p.Steps, Step{
			Kind: "container", Target: full.ID, Name: full.Name, Action: in.Action, Fingerprint: fp, State: "queued",
		})

	case "project":
		if in.Action != "start" && in.Action != "stop" && in.Action != "restart" {
			return Plan{}, errors.New("project action must be start, stop or restart")
		}
		list, err := c.Containers(ctx)
		if err != nil {
			return Plan{}, err
		}
		h := sha256.New()
		for _, ct := range list {
			if ct.Project != in.Project {
				continue
			}
			full, e := c.Inspect(ctx, ct.ID)
			if e != nil || !engine.ValidAction(in.Action, full.State) {
				continue
			}
			fp := containerFingerprint(full)
			_, _ = h.Write([]byte(fp))
			p.Steps = append(p.Steps, Step{
				Kind: "container", Target: full.ID, Name: full.Name, Action: in.Action, Fingerprint: fp, State: "queued",
			})
		}
		if len(p.Steps) == 0 {
			return Plan{}, fmt.Errorf("no containers in %s can %s right now", displayProject(in.Project), in.Action)
		}
		p.Title = fmt.Sprintf("%s stack %s (%d containers)", strings.Title(in.Action), displayProject(in.Project), len(p.Steps))
		p.Impact = fmt.Sprintf("Applies %s sequentially across %d container(s) in %s with post-step verification.", in.Action, len(p.Steps), displayProject(in.Project))
		p.Fingerprint = hex.EncodeToString(h.Sum(nil)[:16])

	case "procedure":
		list, err := c.Containers(ctx)
		if err != nil {
			return Plan{}, err
		}
		h := sha256.New()
		for _, ct := range list {
			if ct.Project != in.Project {
				continue
			}
			full, e := c.Inspect(ctx, ct.ID)
			if e != nil {
				continue
			}
			fp := containerFingerprint(full)
			switch in.Procedure {
			case "restart-unhealthy":
				if full.State == "running" && (full.Health == "unhealthy" || full.OOMKilled) {
					_, _ = h.Write([]byte(fp))
					p.Steps = append(p.Steps, Step{Kind: "container", Target: full.ID, Name: full.Name, Action: "restart", Fingerprint: fp, Condition: "unhealthy", State: "queued"})
				}
			case "start-stopped":
				if full.State == "exited" || full.State == "created" {
					_, _ = h.Write([]byte(fp))
					p.Steps = append(p.Steps, Step{Kind: "container", Target: full.ID, Name: full.Name, Action: "start", Fingerprint: fp, Condition: "stopped", State: "queued"})
				}
			case "restart-high-memory":
				if full.State == "running" {
					if st, se := c.Stats(ctx, full.ID); se == nil && st.LimitBytes > 0 {
						pct := int(float64(st.MemoryBytes) * 100 / float64(st.LimitBytes))
						if pct >= 85 {
							_, _ = h.Write([]byte(fp))
							p.Steps = append(p.Steps, Step{Kind: "container", Target: full.ID, Name: full.Name, Action: "restart", Fingerprint: fp, MemoryPercent: 85, Condition: "memory>=85%", State: "queued"})
						}
					}
				}
			default:
				return Plan{}, errors.New("procedure must be restart-unhealthy, start-stopped or restart-high-memory")
			}
		}
		if len(p.Steps) == 0 {
			return Plan{}, fmt.Errorf("no containers in %s currently match procedure %s", displayProject(in.Project), in.Procedure)
		}
		p.Title = fmt.Sprintf("Run %s on %s", in.Procedure, displayProject(in.Project))
		p.Impact = fmt.Sprintf("Repairs %d matching container(s) in %s and verifies each state.", len(p.Steps), displayProject(in.Project))
		p.Fingerprint = hex.EncodeToString(h.Sum(nil)[:16])

	case "resource":
		inv, err := c.Resources(ctx)
		if err != nil {
			return Plan{}, err
		}
		res, err := engine.FindResource(inv, in.ResourceKind, in.ResourceID)
		if err != nil {
			return Plan{}, err
		}
		if !res.Removable || len(res.Consumers) > 0 {
			reason := res.Reason
			if reason == "" {
				reason = "Resource is currently in use."
			}
			return Plan{}, errors.New(reason)
		}
		fpSum := sha256.Sum256([]byte(res.Kind + "|" + res.ID + "|free"))
		fp := hex.EncodeToString(fpSum[:16])
		p.Project = res.Project
		p.Title = fmt.Sprintf("Remove %s %s", res.Kind, res.Name)
		p.Impact = fmt.Sprintf("Permanently deletes unused %s %s after re-checking that zero containers reference it.", res.Kind, res.Name)
		if res.Kind == "volume" {
			p.Impact = fmt.Sprintf("Permanently deletes unused volume %s and any data stored inside it.", res.Name)
		}
		p.Fingerprint = fp
		p.Steps = append(p.Steps, Step{
			Kind: res.Kind, Target: res.ID, Name: res.Name, Action: "remove", Fingerprint: fp, State: "queued",
		})

	case "compose":
		ws, _ := s.Host.Workspaces(ctx)
		validReg, err := ValidateComposeRegistration(in.Compose, ws)
		if err != nil {
			return Plan{}, err
		}
		prev, err := PreviewCompose(ctx, s.ComposeRunner, c, validReg, ws)
		if err != nil {
			return Plan{}, err
		}
		if !prev.Valid {
			return Plan{}, errors.New(prev.Error)
		}
		saved, err := s.Store.SaveComposeProject(validReg)
		if err != nil {
			return Plan{}, err
		}
		in.Compose = saved
		rawIn, _ = json.Marshal(in)
		p.Input = rawIn
		p.Project = saved.Project
		p.WorkspaceID = saved.WorkspaceID
		p.Title = fmt.Sprintf("Deploy stack %s", saved.Project)
		p.Impact = fmt.Sprintf("Reconciles %d service(s) from %s via docker compose up -d (volumes preserved).", len(prev.Services), strings.Join(saved.Files, ", "))
		p.Fingerprint = prev.Fingerprint
		p.Steps = append(p.Steps, Step{
			Kind: "compose", Target: saved.Project, Name: saved.Project, Action: "up", Fingerprint: prev.Fingerprint, State: "queued",
		})

	default:
		return Plan{}, fmt.Errorf("unsupported plan kind %q", in.Kind)
	}

	created, err := s.Store.CreatePlan(p)
	if err != nil {
		return Plan{}, err
	}
	s.Host.PublishChanged(ctx, "plan")
	return created, nil
}

func (s *Service) NotifyPlanInInbox(ctx context.Context, plan Plan) Plan {
	if plan.InboxID != "" || !s.Host.Available() {
		return plan
	}
	var stepLines []string
	for _, st := range plan.Steps {
		stepLines = append(stepLines, fmt.Sprintf("- **%s** `%s` (%s)", st.Action, st.Name, st.Kind))
	}
	body := fmt.Sprintf("%s\n\n%s\n\nOpen **Docker ▸ Plans** to review and approve before it expires in 5 minutes.", plan.Impact, strings.Join(stepLines, "\n"))
	id, err := s.Host.FileInboxReview(ctx, "Docker plan ready: "+plan.Title, body, plan.WorkspaceID)
	if err == nil && id != "" {
		if updated, e := s.Store.SetPlanInboxID(plan.ID, id); e == nil {
			return updated
		}
	}
	return plan
}

func (s *Service) ExecutePlan(ctx context.Context, planID, requestKey, approvedBy, approverAgentID string) (Job, error) {
	if strings.TrimSpace(requestKey) == "" {
		requestKey = newID("jreq")
	}
	plan, err := s.Store.Plan(planID)
	if err != nil {
		return Job{}, errors.New("plan not found")
	}
	exp, err := time.Parse(time.RFC3339Nano, plan.ExpiresAt)
	if err != nil || !time.Now().Before(exp) {
		return Job{}, errors.New("this plan expired after 5 minutes; generate a fresh preview")
	}

	c, err := s.client(ctx)
	if err != nil {
		return Job{}, err
	}
	defer c.Close()
	if c.Endpoint != plan.Endpoint {
		return Job{}, errors.New("active Docker connection changed since this plan was created")
	}

	stepsCopy := make([]Step, len(plan.Steps))
	copy(stepsCopy, plan.Steps)
	job, fresh, err := s.Store.BeginJob(Job{
		RequestKey:      requestKey,
		PlanID:          plan.ID,
		Kind:            plan.Kind,
		Title:           plan.Title,
		Endpoint:        plan.Endpoint,
		Project:         plan.Project,
		Actor:           plan.Actor,
		AgentID:         plan.AgentID,
		ApprovedBy:      approvedBy,
		ApproverAgentID: approverAgentID,
		Steps:           stepsCopy,
	})
	if err != nil {
		return Job{}, err
	}
	if !fresh {
		return job, nil
	}

	succeeded, failedCount := 0, 0
	var planInput PlanInput
	_ = json.Unmarshal(plan.Input, &planInput)

	for i := range job.Steps {
		st := &job.Steps[i]
		st.State = "running"
		_ = s.Store.UpdateJob(job)

		switch st.Kind {
		case "container":
			before, e := c.Inspect(ctx, st.Target)
			if e != nil {
				st.State, st.Message = "failed", e.Error()
				failedCount++
				continue
			}
			if st.Fingerprint != "" && containerFingerprint(before) != st.Fingerprint {
				st.State, st.Message = "failed", fmt.Sprintf("%s changed state since the plan was previewed.", before.Name)
				failedCount++
				continue
			}
			if err := c.Mutate(ctx, before.ID, st.Action); err != nil {
				st.State, st.Message = "failed", err.Error()
				failedCount++
				continue
			}
			after, e := c.Inspect(ctx, before.ID)
			switch {
			case e != nil:
				st.State, st.Message = "unknown", "Mutation sent, but post-check failed."
				failedCount++
			case (st.Action == "start" || st.Action == "restart") && after.State != "running":
				st.State, st.Message = "failed", fmt.Sprintf("%s is %s after %s.", before.Name, after.State, st.Action)
				failedCount++
			case st.Action == "stop" && after.State == "running":
				st.State, st.Message = "failed", fmt.Sprintf("%s is still running.", before.Name)
				failedCount++
			default:
				st.State, st.Message = "succeeded", fmt.Sprintf("%s is %s.", before.Name, after.State)
				succeeded++
			}

		case "image", "volume", "network":
			inv, e := c.Resources(ctx)
			if e != nil {
				st.State, st.Message = "failed", e.Error()
				failedCount++
				continue
			}
			res, e := engine.FindResource(inv, st.Kind, st.Target)
			if e != nil || !res.Removable || len(res.Consumers) > 0 {
				st.State, st.Message = "failed", "Resource is no longer unused or already removed."
				failedCount++
				continue
			}
			if err := c.RemoveResource(ctx, st.Kind, st.Target); err != nil {
				st.State, st.Message = "failed", err.Error()
				failedCount++
				continue
			}
			st.State, st.Message = "succeeded", fmt.Sprintf("Removed %s %s.", st.Kind, st.Name)
			succeeded++

		case "compose":
			ws, _ := s.Host.Workspaces(ctx)
			prev, e := PreviewCompose(ctx, s.ComposeRunner, c, planInput.Compose, ws)
			if e != nil || !prev.Valid {
				msg := "Compose validation failed before deploy."
				if e != nil {
					msg = e.Error()
				} else if prev.Error != "" {
					msg = prev.Error
				}
				st.State, st.Message = "failed", msg
				failedCount++
				continue
			}
			if st.Fingerprint != "" && prev.Fingerprint != st.Fingerprint {
				st.State, st.Message = "failed", "Compose files or live containers changed since preview; preview again."
				failedCount++
				continue
			}
			summary, err := DeployCompose(ctx, s.ComposeRunner, planInput.Compose)
			if err != nil {
				st.State, st.Message = "failed", err.Error()
				failedCount++
				continue
			}
			st.State, st.Message = "succeeded", summary
			succeeded++
		}
	}

	switch {
	case failedCount == 0:
		job.State = "succeeded"
		job.Message = fmt.Sprintf("Completed %d step(s).", succeeded)
	case succeeded > 0:
		job.State = "partial"
		job.Message = fmt.Sprintf("%d step(s) succeeded, %d failed.", succeeded, failedCount)
	default:
		job.State = "failed"
		if len(job.Steps) > 0 && job.Steps[0].Message != "" {
			job.Message = job.Steps[0].Message
		} else {
			job.Message = "Plan execution failed."
		}
	}
	_ = s.Store.UpdateJob(job)
	s.Host.PublishChanged(ctx, "job")
	if job.State == "failed" {
		return job, errors.New(job.Message)
	}
	return job, nil
}

// ---------- UI Confirmation Door Bridge ----------

type ConfirmRequest struct {
	Kind         string              `json:"kind"` // container | project | resource | procedure | compose | approve-plan
	PlanID       string              `json:"planId,omitempty"`
	ContainerID  string              `json:"containerId,omitempty"`
	Project      string              `json:"project,omitempty"`
	Action       string              `json:"action,omitempty"`
	Procedure    string              `json:"procedure,omitempty"`
	ResourceKind string              `json:"resourceKind,omitempty"`
	ResourceID   string              `json:"resourceId,omitempty"`
	Compose      ComposeRegistration `json:"compose,omitempty"`
}

type confirmPayload struct {
	PlanID string `json:"planId"`
}

func (s *Service) PrepareConfirmation(ctx context.Context, req ConfirmRequest) (ConfirmationRecord, Plan, error) {
	var plan Plan
	var err error
	if req.Kind == "approve-plan" {
		plan, err = s.Store.Plan(req.PlanID)
		if err != nil {
			return ConfirmationRecord{}, Plan{}, errors.New("plan not found")
		}
		exp, pe := time.Parse(time.RFC3339Nano, plan.ExpiresAt)
		if pe != nil || !time.Now().Before(exp) {
			return ConfirmationRecord{}, Plan{}, errors.New("this plan expired; generate a new preview")
		}
	} else {
		plan, err = s.PreviewPlan(ctx, PlanInput{
			Kind:         req.Kind,
			Project:      req.Project,
			ContainerID:  req.ContainerID,
			Action:       req.Action,
			Procedure:    req.Procedure,
			ResourceKind: req.ResourceKind,
			ResourceID:   req.ResourceID,
			Compose:      req.Compose,
			Actor:        "owner",
		})
		if err != nil {
			return ConfirmationRecord{}, Plan{}, err
		}
	}

	label := "Approve plan"
	danger := false
	switch plan.Kind {
	case "container":
		if len(plan.Steps) > 0 && plan.Steps[0].Action == "stop" {
			label, danger = "Stop container", true
		} else if len(plan.Steps) > 0 && plan.Steps[0].Action == "restart" {
			label = "Restart container"
		}
	case "project":
		if len(plan.Steps) > 0 && plan.Steps[0].Action == "stop" {
			label, danger = "Stop stack", true
		} else if len(plan.Steps) > 0 && plan.Steps[0].Action == "restart" {
			label = "Restart stack"
		} else {
			label = "Start stack"
		}
	case "resource":
		danger = true
		if len(plan.Steps) > 0 {
			label = "Remove " + plan.Steps[0].Kind
		} else {
			label = "Remove item"
		}
	case "compose":
		label = "Deploy stack"
	case "procedure":
		label = "Run repair"
	}

	var lines []string
	lines = append(lines, plan.Impact)
	if len(plan.Steps) > 1 {
		for i, st := range plan.Steps {
			if i >= 6 {
				lines = append(lines, fmt.Sprintf("…and %d more step(s).", len(plan.Steps)-i))
				break
			}
			lines = append(lines, fmt.Sprintf("• %s %s", strings.Title(st.Action), st.Name))
		}
	}
	rec, err := s.Store.CreateConfirmation(plan.Kind, plan.Title, strings.Join(lines, "\n"), label, danger, confirmPayload{PlanID: plan.ID})
	return rec, plan, err
}

func (s *Service) ExecuteConfirmed(ctx context.Context, confirmationID, revision string) (Job, error) {
	rec, err := s.Store.ConsumeConfirmation(confirmationID, revision)
	if err != nil {
		return Job{}, err
	}
	var payload confirmPayload
	if err := json.Unmarshal(rec.Payload, &payload); err != nil || payload.PlanID == "" {
		return Job{}, errors.New("invalid confirmation payload")
	}
	return s.ExecutePlan(ctx, payload.PlanID, "cfm-"+rec.ID, "owner", "")
}

func nonNilSlice[T any](in []T) []T {
	if in == nil {
		return []T{}
	}
	return in
}
