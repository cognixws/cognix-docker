package sysadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cfpperche/picode-docker/engine"
)

const ToolProtocol = "picode-tools/1"

type ToolContext struct {
	Agent        string `json:"agent"`
	Workspace    string `json:"workspace"`
	WorkDir      string `json:"workDir"`
	ExtensionDir string `json:"extensionDir"`
	DataDir      string `json:"dataDir"`
}

type ToolRequest struct {
	Protocol   string          `json:"protocol"`
	RequestID  string          `json:"requestId"`
	Capability string          `json:"capability"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	Context    ToolContext     `json:"context"`
	Settings   map[string]any  `json:"settings,omitempty"`
}

type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolResult struct {
	Content           []ToolContent  `json:"content"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

func textResult(text string, structured map[string]any) ToolResult {
	return ToolResult{
		Content:           []ToolContent{{Type: "text", Text: text}},
		StructuredContent: structured,
	}
}

func errResult(msg string) ToolResult {
	return ToolResult{
		Content: []ToolContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}

// ExecuteTool dispatches a picode-tools/1 tool call against the single-writer
// sysadmin service so every CLI and Assistant shares the same locks, plans and audit.
func (s *Service) ExecuteTool(ctx context.Context, req ToolRequest) ToolResult {
	if req.Protocol != ToolProtocol {
		return errResult("unsupported tool protocol")
	}
	actor := s.Host.AgentActor(ctx, req.Context.Agent)
	agentID := strings.TrimSpace(req.Context.Agent)

	switch req.Capability + "/" + req.Tool {
	case "inspect/containers":
		var args struct {
			Project string `json:"project"`
			State   string `json:"state"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		ov, err := s.Overview(ctx, "", true)
		if err != nil {
			return errResult(err.Error())
		}
		if !ov.Reachable {
			return errResult(ov.Error)
		}
		groups := []StackGroup{}
		total := 0
		for _, g := range ov.Groups {
			if args.Project != "" && g.Project != args.Project {
				continue
			}
			filtered := g
			if args.State != "" {
				filtered.Containers = nil
				for _, ct := range g.Containers {
					if ct.State == args.State {
						filtered.Containers = append(filtered.Containers, ct)
					}
				}
			}
			total += len(filtered.Containers)
			if len(filtered.Containers) > 0 || args.Project != "" {
				groups = append(groups, filtered)
			}
		}
		return textResult(
			fmt.Sprintf("%d container(s) across %d stack group(s) on %s.", total, len(groups), ov.Endpoint),
			map[string]any{"endpoint": ov.Endpoint, "count": total, "groups": groups},
		)

	case "inspect/container":
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil || strings.TrimSpace(args.ID) == "" {
			return errResult("provide full 64-character container id")
		}
		d, err := s.ContainerDetail(ctx, args.ID)
		if err != nil {
			return errResult(err.Error())
		}
		raw, _ := json.Marshal(d)
		var structured map[string]any
		_ = json.Unmarshal(raw, &structured)
		return textResult(
			fmt.Sprintf("%s (%s): state=%s health=%s shell=%q", d.Name, d.Image, d.State, d.Health, d.ShellCommand),
			structured,
		)

	case "inspect/logs":
		var args struct {
			ID      string `json:"id"`
			Project string `json:"project"`
			Tail    int    `json:"tail"`
			Level   string `json:"level"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		if args.ID == "" && args.Project == "" {
			return errResult("provide either container id or stack project name")
		}
		var entries []any
		var lines []string
		if args.ID != "" {
			c, err := s.client(ctx)
			if err != nil {
				return errResult(humanizeDockerError(err))
			}
			defer c.Close()
			full, err := c.Inspect(ctx, args.ID)
			if err != nil {
				return errResult(err.Error())
			}
			lg, err := c.LogsTail(ctx, full, args.Tail)
			if err != nil {
				return errResult(err.Error())
			}
			for _, e := range parseFilteredEntries(lg.Text, full.Service, full.Name, args.Level) {
				entries = append(entries, e)
				lines = append(lines, fmt.Sprintf("[%s] %s %s: %s", e.Level, e.Timestamp, e.Service, e.Message))
			}
		} else {
			sl, err := s.StackLogs(ctx, args.Project, args.Tail)
			if err != nil {
				return errResult(humanizeDockerError(err))
			}
			for _, e := range sl.Entries {
				if args.Level != "" && !strings.EqualFold(e.Level, args.Level) {
					continue
				}
				entries = append(entries, e)
				lines = append(lines, fmt.Sprintf("[%s] %s %s: %s", e.Level, e.Timestamp, e.Service, e.Message))
			}
		}
		summary := strings.Join(lines, "\n")
		if summary == "" {
			summary = "No matching log lines."
		}
		return textResult(summary, map[string]any{"count": len(entries), "entries": entries})

	case "inspect/resources":
		var args struct {
			Kind       string `json:"kind"`
			UnusedOnly bool   `json:"unusedOnly"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		c, err := s.client(ctx)
		if err != nil {
			return errResult(humanizeDockerError(err))
		}
		defer c.Close()
		inv, err := c.Resources(ctx)
		if err != nil {
			return errResult(err.Error())
		}
		filtered := make([]any, 0, len(inv.Items))
		removable := 0
		for _, item := range inv.Items {
			if args.Kind != "" && item.Kind != args.Kind {
				continue
			}
			if args.UnusedOnly && !item.Removable {
				continue
			}
			if item.Removable {
				removable++
			}
			filtered = append(filtered, item)
		}
		return textResult(
			fmt.Sprintf("%d resource(s) listed (%d unused and eligible for single-item removal).", len(filtered), removable),
			map[string]any{"endpoint": inv.Endpoint, "sampledAt": inv.SampledAt, "items": filtered},
		)

	case "inspect/health":
		var args struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		if args.Project != "" {
			mon, incs, snap, err := s.CheckProjectHealth(ctx, args.Project, actor)
			if err != nil {
				return errResult(humanizeDockerError(err))
			}
			return textResult(
				fmt.Sprintf("Health sampled for %s: %d signal(s), %d incident(s).", displayProject(args.Project), len(snap.Signals), len(incs)),
				map[string]any{"monitor": mon, "incidents": incs, "snapshot": snap},
			)
		}
		mons, _ := s.Store.Monitors()
		open, _ := s.Store.OpenIncidents()
		return textResult(
			fmt.Sprintf("%d configured monitor(s), %d open incident(s).", len(mons), len(open)),
			map[string]any{"monitors": nonNilSlice(mons), "openIncidents": nonNilSlice(open)},
		)

	case "inspect/open-panel":
		var args struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		return textResult(
			"Opened the Docker panel beside your session.",
			map[string]any{
				"picode/panel": map[string]any{"panel": "docker"},
				"project":      args.Project,
			},
		)

	case "operate/manage":
		var args struct {
			ID         string `json:"id"`
			Action     string `json:"action"`
			RequestKey string `json:"requestKey"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return errResult("invalid input")
		}
		if args.Action == "start" {
			op, err := s.MutateContainer(ctx, args.ID, "start", args.RequestKey, actor, agentID)
			if err != nil {
				return errResult(err.Error())
			}
			return textResult(op.Message, map[string]any{"operation": op, "requiresApproval": false})
		}
		if args.Action != "stop" && args.Action != "restart" {
			return errResult("action must be start, stop or restart")
		}
		plan, err := s.PreviewPlan(ctx, PlanInput{
			Kind:        "container",
			ContainerID: args.ID,
			Action:      args.Action,
			WorkspaceID: req.Context.Workspace,
			Actor:       actor,
			AgentID:     agentID,
		})
		if err != nil {
			return errResult(err.Error())
		}
		plan = s.NotifyPlanInInbox(ctx, plan)
		return textResult(
			fmt.Sprintf("Created 5-minute approval plan %s (%s). The owner can approve it in Docker ▸ Plans.", plan.ID, plan.Title),
			map[string]any{
				"requiresApproval": true,
				"plan":             plan,
				"picode/panel":     map[string]any{"panel": "docker"},
			},
		)

	case "operate/history":
		var args struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		ops, _ := s.Store.Operations(args.Limit)
		jobs, _ := s.Store.Jobs()
		plans, _ := s.Store.PendingPlans()
		return textResult(
			fmt.Sprintf("%d operation(s), %d job(s), %d pending plan(s).", len(ops), len(jobs), len(plans)),
			map[string]any{
				"operations":   nonNilSlice(ops),
				"jobs":         nonNilSlice(jobs),
				"pendingPlans": nonNilSlice(plans),
			},
		)

	case "compose/compose-preview":
		var args struct {
			WorkDir  string   `json:"workDir"`
			Project  string   `json:"project"`
			Files    []string `json:"files"`
			Profiles []string `json:"profiles"`
			Register bool     `json:"register"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return errResult("invalid compose-preview input")
		}
		workDir := strings.TrimSpace(args.WorkDir)
		if workDir == "" {
			workDir = req.Context.WorkDir
		}
		ws, _ := s.Host.Workspaces(ctx)
		c, _ := s.client(ctx)
		if c != nil {
			defer c.Close()
		}
		reg := ComposeRegistration{
			WorkspaceID: req.Context.Workspace,
			Project:     args.Project,
			WorkDir:     workDir,
			Files:       args.Files,
			Profiles:    args.Profiles,
		}
		prev, err := PreviewCompose(ctx, s.ComposeRunner, c, reg, ws)
		if err != nil {
			return errResult(err.Error())
		}
		if args.Register && prev.Valid {
			validReg, ve := ValidateComposeRegistration(reg, ws)
			if ve == nil {
				_, _ = s.Store.SaveComposeProject(validReg)
				s.Host.PublishChanged(ctx, "compose")
			}
		}
		raw, _ := json.Marshal(prev)
		var structured map[string]any
		_ = json.Unmarshal(raw, &structured)
		if !prev.Valid {
			return errResult("Compose configuration invalid: " + prev.Error)
		}
		return textResult(
			fmt.Sprintf("Compose stack %s validated (%d service(s), fingerprint %s).", prev.Project, len(prev.Services), prev.Fingerprint[:12]),
			structured,
		)

	case "compose/compose-deploy":
		var args struct {
			WorkDir  string   `json:"workDir"`
			Project  string   `json:"project"`
			Files    []string `json:"files"`
			Profiles []string `json:"profiles"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return errResult("invalid compose-deploy input")
		}
		workDir := strings.TrimSpace(args.WorkDir)
		if workDir == "" {
			workDir = req.Context.WorkDir
		}
		plan, err := s.PreviewPlan(ctx, PlanInput{
			Kind:        "compose",
			WorkspaceID: req.Context.Workspace,
			Actor:       actor,
			AgentID:     agentID,
			Compose: ComposeRegistration{
				WorkspaceID: req.Context.Workspace,
				Project:     args.Project,
				WorkDir:     workDir,
				Files:       args.Files,
				Profiles:    args.Profiles,
			},
		})
		if err != nil {
			return errResult(err.Error())
		}
		plan = s.NotifyPlanInInbox(ctx, plan)
		return textResult(
			fmt.Sprintf("Created Compose deploy plan %s for %s. The owner can review and approve it in Docker ▸ Plans.", plan.ID, plan.Project),
			map[string]any{
				"requiresApproval": true,
				"plan":             plan,
				"picode/panel":     map[string]any{"panel": "docker"},
			},
		)

	case "maintain/diagnose":
		var args struct {
			Project string `json:"project"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		diag, err := s.Diagnose(ctx, args.Project)
		if err != nil {
			return errResult(humanizeDockerError(err))
		}
		raw, _ := json.Marshal(diag)
		var structured map[string]any
		_ = json.Unmarshal(raw, &structured)
		return textResult(diag.Summary, structured)

	case "maintain/plan":
		var args struct {
			Kind         string `json:"kind"`
			Project      string `json:"project"`
			Action       string `json:"action"`
			Procedure    string `json:"procedure"`
			ResourceKind string `json:"resourceKind"`
			ResourceID   string `json:"resourceId"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return errResult("invalid plan input")
		}
		plan, err := s.PreviewPlan(ctx, PlanInput{
			Kind:         args.Kind,
			Project:      args.Project,
			WorkspaceID:  req.Context.Workspace,
			Action:       args.Action,
			Procedure:    args.Procedure,
			ResourceKind: args.ResourceKind,
			ResourceID:   args.ResourceID,
			Actor:        actor,
			AgentID:      agentID,
		})
		if err != nil {
			return errResult(err.Error())
		}
		return textResult(
			fmt.Sprintf("Created 5-minute plan %s: %s (%d step(s)). Call execute-plan to send it for owner approval.", plan.ID, plan.Title, len(plan.Steps)),
			map[string]any{"plan": plan},
		)

	case "maintain/execute-plan":
		var args struct {
			PlanID string `json:"planId"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil || strings.TrimSpace(args.PlanID) == "" {
			return errResult("provide planId")
		}
		plan, err := s.Store.Plan(args.PlanID)
		if err != nil {
			return errResult("plan not found")
		}
		plan = s.NotifyPlanInInbox(ctx, plan)
		return textResult(
			fmt.Sprintf("Plan %s (%s) is queued for the owner's confirmation on Docker ▸ Plans.", plan.ID, plan.Title),
			map[string]any{
				"requiresApproval": true,
				"plan":             plan,
				"inboxId":          plan.InboxID,
				"picode/panel":     map[string]any{"panel": "docker"},
			},
		)

	case "maintain/jobs":
		var args struct {
			JobID string `json:"jobId"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		if args.JobID != "" {
			j, err := s.Store.Job(args.JobID)
			if err != nil {
				return errResult("job not found")
			}
			return textResult(fmt.Sprintf("Job %s: %s (%s)", j.ID, j.State, j.Message), map[string]any{"job": j})
		}
		jobs, _ := s.Store.Jobs()
		return textResult(fmt.Sprintf("%d maintenance job(s).", len(jobs)), map[string]any{"jobs": nonNilSlice(jobs)})

	case "maintain/monitors":
		var args struct {
			Action          string `json:"action"` // list | configure | check
			Project         string `json:"project"`
			Enabled         *bool  `json:"enabled"`
			IntervalSeconds int    `json:"intervalSeconds"`
			CPUPercent      int    `json:"cpuPercent"`
			MemoryPercent   int    `json:"memoryPercent"`
			BadSamples      int    `json:"badSamples"`
			RetentionDays   int    `json:"retentionDays"`
			Revision        int    `json:"revision"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			return errResult("invalid monitors input")
		}
		switch args.Action {
		case "", "list":
			mons, _ := s.Store.Monitors()
			open, _ := s.Store.OpenIncidents()
			return textResult(fmt.Sprintf("%d monitor(s), %d open incident(s).", len(mons), len(open)), map[string]any{"monitors": nonNilSlice(mons), "openIncidents": nonNilSlice(open)})
		case "check":
			mon, incs, snap, err := s.CheckProjectHealth(ctx, args.Project, actor)
			if err != nil {
				return errResult(humanizeDockerError(err))
			}
			return textResult(fmt.Sprintf("Checked %s: %d signal(s), %d incident(s).", displayProject(args.Project), len(snap.Signals), len(incs)), map[string]any{"monitor": mon, "incidents": incs, "snapshot": snap})
		case "configure":
			c, err := s.client(ctx)
			if err != nil {
				return errResult(humanizeDockerError(err))
			}
			defer c.Close()
			mon := DefaultMonitor(c.Endpoint, args.Project)
			if existing, e := s.Store.Monitor(c.Endpoint, args.Project); e == nil {
				mon = existing
			}
			if args.Enabled != nil {
				mon.Enabled = *args.Enabled
			} else {
				mon.Enabled = true
			}
			if args.IntervalSeconds != 0 {
				mon.IntervalSeconds = args.IntervalSeconds
			}
			if args.CPUPercent != 0 {
				mon.CPUPercent = args.CPUPercent
			}
			if args.MemoryPercent != 0 {
				mon.MemoryPercent = args.MemoryPercent
			}
			if args.BadSamples != 0 {
				mon.BadSamples = args.BadSamples
			}
			if args.RetentionDays != 0 {
				mon.RetentionDays = args.RetentionDays
			}
			if args.Revision != 0 {
				mon.Revision = args.Revision
			}
			mon.Actor = actor
			saved, err := s.Store.SaveMonitor(mon)
			if err != nil {
				return errResult(err.Error())
			}
			s.Host.PublishChanged(ctx, "monitor")
			return textResult(fmt.Sprintf("Saved monitor for %s (enabled=%v, every %ds).", displayProject(saved.Project), saved.Enabled, saved.IntervalSeconds), map[string]any{"monitor": saved})
		default:
			return errResult("action must be list, configure or check")
		}
	}

	return errResult(fmt.Sprintf("unknown tool %s/%s", req.Capability, req.Tool))
}

func parseFilteredEntries(text, service, name, level string) []engine.LogEntry {
	raw := engine.ParseLogEntries(text, service, name)
	out := []engine.LogEntry{}
	for _, e := range raw {
		if level != "" && !strings.EqualFold(e.Level, level) {
			continue
		}
		out = append(out, e)
	}
	return out
}
