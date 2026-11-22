// Package server exposes cognix-docker's loopback HTTP routes for COGNIX[WS]'s
// page relay, confirm door, storageProvider door, and local picode-tools/1 IPC.
package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cfpperche/picode-docker/engine"
	"github.com/cfpperche/picode-docker/sysadmin"
)

type Config struct {
	ProxySecret string
	ToolKey     string
	Service     *sysadmin.Service
}

func NewHandler(cfg Config) http.Handler {
	mux := http.NewServeMux()
	svc := cfg.Service

	withProxy := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-PiCode-Proxy-Secret")
			if cfg.ProxySecret == "" || subtle.ConstantTimeCompare([]byte(got), []byte(cfg.ProxySecret)) != 1 {
				reply(w, http.StatusForbidden, map[string]string{"message": "only COGNIX[WS] may call this extension process"})
				return
			}
			fn(w, r)
		}
	}

	withToolKey := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-Docker-Tool-Key")
			if cfg.ToolKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(cfg.ToolKey)) != 1 {
				reply(w, http.StatusForbidden, map[string]string{"message": "unauthorized tool executor"})
				return
			}
			fn(w, r)
		}
	}

	// Overview & Containers
	mux.HandleFunc("GET /overview", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		stats := r.URL.Query().Get("stats") != "0"
		ov, err := svc.Overview(ctx, r.URL.Query().Get("path"), stats)
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
			return
		}
		reply(w, http.StatusOK, ov)
	}))

	mux.HandleFunc("GET /containers", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		ov, err := svc.Overview(ctx, "", false)
		if err != nil || !ov.Reachable {
			msg := "Docker is unavailable"
			if err != nil {
				msg = err.Error()
			} else if ov.Error != "" {
				msg = ov.Error
			}
			reply(w, http.StatusBadGateway, map[string]string{"message": msg})
			return
		}
		reply(w, http.StatusOK, map[string]any{"groups": ov.Groups, "count": ov.TotalContainers})
	}))

	mux.HandleFunc("GET /containers/{id}", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		d, err := svc.ContainerDetail(ctx, r.PathValue("id"))
		if err != nil {
			var apiErr *engine.APIError
			code := http.StatusBadGateway
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				code = http.StatusNotFound
			}
			reply(w, code, map[string]string{"message": err.Error()})
			return
		}
		reply(w, http.StatusOK, d)
	}))

	mux.HandleFunc("GET /stacks/logs", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		logs, err := svc.StackLogs(ctx, r.URL.Query().Get("project"), tail)
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
			return
		}
		reply(w, http.StatusOK, logs)
	}))

	// Direct non-destructive Start (stop & restart go through /confirmations -> confirm door)
	mux.HandleFunc("POST /containers/{id}/start", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var req struct {
			RequestKey string `json:"requestKey"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
		op, err := svc.MutateContainer(ctx, r.PathValue("id"), "start", req.RequestKey, "owner", "")
		if err != nil {
			code := http.StatusBadGateway
			if errors.Is(err, sysadmin.ErrConflict) || strings.Contains(err.Error(), "cannot ") {
				code = http.StatusConflict
			}
			reply(w, code, map[string]any{"message": err.Error(), "operation": op})
			return
		}
		reply(w, http.StatusOK, map[string]any{"message": op.Message, "operation": op})
	}))

	// Confirm Door Protocol:
	// 1. Page calls POST /confirmations -> creates ConfirmationRecord + 5-min Plan
	// 2. Host calls GET /operations/{id} -> returns {confirmation: {title, message, confirmLabel, danger, revision}}
	// 3. After user clicks confirm in native host dialog, host calls POST /__picode/confirm with X-PiCode-Owner-Confirmed: <64-hex revision>
	mux.HandleFunc("POST /confirmations", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		var req sysadmin.ConfirmRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&req); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"message": "invalid confirmation request"})
			return
		}
		rec, plan, err := svc.PrepareConfirmation(ctx, req)
		if err != nil {
			reply(w, http.StatusConflict, map[string]string{"message": err.Error()})
			return
		}
		reply(w, http.StatusOK, map[string]any{"operationId": rec.ID, "plan": plan})
	}))

	mux.HandleFunc("GET /operations/{id}", withProxy(func(w http.ResponseWriter, r *http.Request) {
		rec, err := svc.Store.GetConfirmation(r.PathValue("id"))
		if err != nil {
			code := http.StatusNotFound
			if !errors.Is(err, sql.ErrNoRows) {
				code = http.StatusConflict
			}
			reply(w, code, map[string]string{"message": err.Error()})
			return
		}
		reply(w, http.StatusOK, map[string]any{"confirmation": rec.Confirmation})
	}))

	mux.HandleFunc("POST /__picode/confirm", withProxy(func(w http.ResponseWriter, r *http.Request) {
		confirmedRev := strings.TrimSpace(r.Header.Get("X-PiCode-Owner-Confirmed"))
		if len(confirmedRev) != 64 {
			reply(w, http.StatusForbidden, map[string]string{"message": "owner confirmation header is required"})
			return
		}
		var req struct {
			OperationID string `json:"operationId"`
			Revision    string `json:"revision"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.OperationID == "" {
			reply(w, http.StatusBadRequest, map[string]string{"message": "invalid confirmation body"})
			return
		}
		if subtle.ConstantTimeCompare([]byte(confirmedRev), []byte(req.Revision)) != 1 {
			reply(w, http.StatusConflict, map[string]string{"message": "confirmation revision mismatch"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
		defer cancel()
		job, err := svc.ExecuteConfirmed(ctx, req.OperationID, confirmedRev)
		if err != nil {
			code := http.StatusConflict
			if job.ID != "" {
				code = http.StatusBadGateway
			}
			reply(w, code, map[string]any{"message": err.Error(), "job": job})
			return
		}
		reply(w, http.StatusOK, map[string]any{"job": job})
	}))

	// Health, Monitors & Diagnosis
	mux.HandleFunc("GET /health", withProxy(func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		mons, _ := svc.Store.Monitors()
		open, _ := svc.Store.OpenIncidents()
		var projIncidents []sysadmin.Incident
		var current *sysadmin.Monitor
		if c, err := engine.Connect(r.Context()); err == nil {
			ep := c.Endpoint
			c.Close()
			if m, e := svc.Store.Monitor(ep, project); e == nil {
				current = &m
			} else {
				def := sysadmin.DefaultMonitor(ep, project)
				current = &def
			}
			projIncidents, _ = svc.Store.Incidents(ep, project)
		}
		reply(w, http.StatusOK, map[string]any{
			"monitor":       current,
			"monitors":      mons,
			"incidents":     projIncidents,
			"openIncidents": open,
		})
	}))

	mux.HandleFunc("POST /health/check", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var req struct {
			Project string `json:"project"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
		mon, incs, snap, err := svc.CheckProjectHealth(ctx, req.Project, "owner")
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
			return
		}
		reply(w, http.StatusOK, map[string]any{"monitor": mon, "incidents": incs, "snapshot": snap})
	}))

	mux.HandleFunc("POST /monitors", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		var m sysadmin.Monitor
		if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&m); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"message": "invalid monitor settings"})
			return
		}
		c, err := svc.Connect(ctx)
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
			return
		}
		m.Endpoint = c.Endpoint
		c.Close()
		m.Actor = "owner"
		saved, err := svc.Store.SaveMonitor(m)
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, sysadmin.ErrConflict) {
				code = http.StatusConflict
			}
			reply(w, code, map[string]string{"message": err.Error()})
			return
		}
		svc.Host.PublishChanged(ctx, "monitor")
		reply(w, http.StatusOK, map[string]any{"monitor": saved})
	}))

	mux.HandleFunc("POST /diagnosis", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		var req struct {
			Project string `json:"project"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
		diag, err := svc.Diagnose(ctx, req.Project)
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
			return
		}
		reply(w, http.StatusOK, diag)
	}))

	// Resources & Audit
	mux.HandleFunc("GET /resources", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		c, err := svc.Connect(ctx)
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
			return
		}
		defer c.Close()
		inv, err := c.Resources(ctx)
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
			return
		}
		reply(w, http.StatusOK, inv)
	}))

	mux.HandleFunc("GET /audit", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ops, _ := svc.Store.Operations(50)
		jobs, _ := svc.Store.Jobs()
		plans, _ := svc.Store.PendingPlans()
		reply(w, http.StatusOK, map[string]any{
			"operations":   ops,
			"jobs":         jobs,
			"pendingPlans": plans,
		})
	}))

	// Compose Registration & Preview
	mux.HandleFunc("POST /compose/preview", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		var req struct {
			Path     string   `json:"path"`
			WorkDir  string   `json:"workDir"`
			Project  string   `json:"project"`
			Files    []string `json:"files"`
			Profiles []string `json:"profiles"`
			Register bool     `json:"register"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"message": "invalid Compose request"})
			return
		}
		var reg sysadmin.ComposeRegistration
		if strings.TrimSpace(req.Path) != "" && strings.TrimSpace(req.WorkDir) == "" {
			var err error
			reg, err = sysadmin.ResolveComposePath(req.Path)
			if err != nil {
				reply(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
				return
			}
			if req.Project != "" {
				reg.Project = req.Project
			}
		} else {
			reg = sysadmin.ComposeRegistration{
				Project:  req.Project,
				WorkDir:  req.WorkDir,
				Files:    req.Files,
				Profiles: req.Profiles,
			}
		}
		ws, _ := svc.Host.Workspaces(ctx)
		c, _ := svc.Connect(ctx)
		if c != nil {
			defer c.Close()
		}
		prev, err := sysadmin.PreviewCompose(ctx, svc.ComposeRunner, c, reg, ws)
		if err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		if req.Register && prev.Valid {
			if validReg, ve := sysadmin.ValidateComposeRegistration(reg, ws); ve == nil {
				_, _ = svc.Store.SaveComposeProject(validReg)
				svc.Host.PublishChanged(ctx, "compose")
			}
		}
		reply(w, http.StatusOK, prev)
	}))

	// Settings > Storage door (storageProvider, apiVersion 17)
	mux.HandleFunc("GET /storage/measure", withProxy(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		c, err := svc.Connect(ctx)
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		defer c.Close()
		items, err := c.StorageItems(ctx)
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		reply(w, http.StatusOK, map[string]any{
			"sentence": "The build cache, and the images and volumes no container uses: those go one by one, only the ones you check.",
			"items":    items,
		})
	}))

	mux.HandleFunc("POST /storage/clean", withProxy(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Item string `json:"item"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Item == "" {
			reply(w, http.StatusBadRequest, map[string]string{"error": "give an item"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 9*time.Minute)
		defer cancel()
		c, err := svc.Connect(ctx)
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		defer c.Close()
		freed, err := c.CleanStorageItem(ctx, req.Item)
		switch {
		case errors.Is(err, engine.ErrNotFree):
			reply(w, http.StatusConflict, map[string]string{"error": "no longer there, or something uses it now"})
		case err != nil:
			reply(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		default:
			svc.Host.PublishChanged(ctx, "storage")
			reply(w, http.StatusOK, map[string]any{"freed": freed})
		}
	}))

	// Local picode-tools/1 IPC from `cognix-docker exec`
	mux.HandleFunc("POST /tool", withToolKey(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
		defer cancel()
		var req sysadmin.ToolRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 512*1024)).Decode(&req); err != nil {
			reply(w, http.StatusBadRequest, sysadmin.ToolResult{
				Content: []sysadmin.ToolContent{{Type: "text", Text: "invalid tool payload"}},
				IsError: true,
			})
			return
		}
		res := svc.ExecuteTool(ctx, req)
		reply(w, http.StatusOK, res)
	}))

	return mux
}

func reply(w http.ResponseWriter, code int, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}
