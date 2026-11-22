package sysadmin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cfpperche/picode-docker/engine"
)

const (
	testID1 = "1111111111111111111111111111111111111111111111111111111111111111"
	testID2 = "2222222222222222222222222222222222222222222222222222222222222222"
	testNet = "3333333333333333333333333333333333333333333333333333333333333333"
	testImg = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

type fakeDocker struct {
	mu         sync.Mutex
	state1     string
	state2     string
	health2    string
	netRemoved bool
}

func startFakeDocker(t *testing.T, fd *fakeDocker) (string, func(context.Context) (*engine.Client, error)) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "dktest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.45", "MinAPIVersion": "1.24"})
	})
	mux.HandleFunc("GET /v1.44/containers/json", func(w http.ResponseWriter, r *http.Request) {
		fd.mu.Lock()
		s1, s2, h2 := fd.state1, fd.state2, fd.health2
		fd.mu.Unlock()
		status2 := s2
		if h2 != "" {
			status2 += " (" + h2 + ")"
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"Id": testID1, "Names": []string{"/billing-db"}, "Image": "postgres:16", "ImageID": "sha256:aaaa",
				"State": s1, "Status": s1,
				"Ports":  []map[string]any{{"IP": "0.0.0.0", "PrivatePort": 5432, "PublicPort": 5432, "Type": "tcp"}},
				"Labels": map[string]string{"com.docker.compose.project": "billing", "com.docker.compose.service": "db"},
			},
			{
				"Id": testID2, "Names": []string{"/billing-api"}, "Image": "ghcr.io/acme/api:1.0", "ImageID": "sha256:bbbb",
				"State": s2, "Status": status2,
				"Labels": map[string]string{"com.docker.compose.project": "billing", "com.docker.compose.service": "api"},
			},
		})
	})
	mux.HandleFunc("GET /v1.44/containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		fd.mu.Lock()
		s1, s2, h2 := fd.state1, fd.state2, fd.health2
		fd.mu.Unlock()
		if id == testID1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": testID1, "Name": "/billing-db", "Image": "sha256:aaaa", "RestartCount": 0,
				"Config": map[string]any{"Image": "postgres:16", "Tty": true, "Labels": map[string]string{"com.docker.compose.project": "billing", "com.docker.compose.service": "db"}},
				"State":  map[string]any{"Status": s1, "StartedAt": "2026-11-21T10:00:00Z"},
			})
			return
		}
		if id == testID2 {
			var hc any
			var hs any
			if h2 != "" {
				hc = map[string]any{"Test": []string{"CMD", "curl", "-f", "http://localhost/health"}}
				hs = map[string]any{"Status": h2}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": testID2, "Name": "/billing-api", "Image": "sha256:bbbb", "RestartCount": 2,
				"Config": map[string]any{"Image": "ghcr.io/acme/api:1.0", "Tty": true, "Healthcheck": hc, "Labels": map[string]string{"com.docker.compose.project": "billing", "com.docker.compose.service": "api"}},
				"State":  map[string]any{"Status": s2, "Health": hs, "StartedAt": "2026-11-21T10:00:00Z"},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("GET /v1.44/containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("2026-11-21T10:01:00Z INFO ready\n2026-11-21T10:02:00Z ERROR connection timeout token=supersecret123\n"))
	})
	mux.HandleFunc("GET /v1.44/containers/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cpu_stats":    map[string]any{"cpu_usage": map[string]any{"total_usage": 200}, "system_cpu_usage": 2000, "online_cpus": 2},
			"precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 100}, "system_cpu_usage": 1000, "online_cpus": 2},
			"memory_stats": map[string]any{"usage": 128 * 1024 * 1024, "limit": 512 * 1024 * 1024},
		})
	})
	mux.HandleFunc("POST /v1.44/containers/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id, act := r.PathValue("id"), r.PathValue("action")
		fd.mu.Lock()
		defer fd.mu.Unlock()
		target := &fd.state1
		if id == testID2 {
			target = &fd.state2
		}
		if act == "start" || act == "restart" {
			*target = "running"
		} else if act == "stop" {
			*target = "exited"
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1.44/system/df", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Images":  []map[string]any{{"Id": testImg, "RepoTags": []string{"unused:latest"}, "Size": 10485760, "SharedSize": 0, "Containers": 0}},
			"Volumes": []map[string]any{},
		})
	})
	mux.HandleFunc("GET /v1.44/images/json", func(w http.ResponseWriter, r *http.Request) {
		size := int64(10485760)
		_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": testImg, "RepoTags": []string{"unused:latest"}, "Size": size}})
	})
	mux.HandleFunc("GET /v1.44/volumes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": []any{}})
	})
	mux.HandleFunc("GET /v1.44/networks", func(w http.ResponseWriter, r *http.Request) {
		fd.mu.Lock()
		gone := fd.netRemoved
		fd.mu.Unlock()
		nets := []map[string]any{{"Id": "bridgeid", "Name": "bridge", "Driver": "bridge", "Scope": "local"}}
		if !gone {
			nets = append(nets, map[string]any{"Id": testNet, "Name": "orphan_net", "Driver": "bridge", "Scope": "local"})
		}
		_ = json.NewEncoder(w).Encode(nets)
	})
	mux.HandleFunc("DELETE /v1.44/networks/{id}", func(w http.ResponseWriter, r *http.Request) {
		fd.mu.Lock()
		fd.netRemoved = true
		fd.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1.44/networks/{id}", func(w http.ResponseWriter, r *http.Request) {
		fd.mu.Lock()
		gone := fd.netRemoved
		fd.mu.Unlock()
		if gone {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": testNet})
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	endpoint := "unix://" + sock
	return endpoint, func(ctx context.Context) (*engine.Client, error) {
		return engine.NewClient(endpoint)
	}
}

func TestUrlSecretRedaction(t *testing.T) {
	got := engine.Redact("DSN=postgres://admin:s3cr3t@db.internal:5432/app https://u:p@h2.local/x", nil)
	if strings.Contains(got, "s3cr3t") || strings.Contains(got, "u:p") {
		t.Fatalf("expected URL credentials with digits in scheme/host to be redacted, got %q", got)
	}
}

func TestStoreLocksIdempotencyAndRecovery(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	op1, fresh, err := st.BeginOperation(Operation{
		RequestKey: "k1", Endpoint: "unix:///var/run/docker.sock",
		ContainerID: testID1, ContainerName: "billing-db", Action: "start", Actor: "owner",
	})
	if err != nil || !fresh {
		t.Fatalf("expected fresh operation, err=%v fresh=%v", err, fresh)
	}

	// Idempotent replay with same request key and target returns existing running operation.
	opReplay, fresh2, err := st.BeginOperation(Operation{
		RequestKey: "k1", Endpoint: "unix:///var/run/docker.sock",
		ContainerID: testID1, ContainerName: "billing-db", Action: "start", Actor: "owner",
	})
	if err != nil || fresh2 || opReplay.ID != op1.ID {
		t.Fatalf("expected idempotent replay, got err=%v fresh=%v id=%s", err, fresh2, opReplay.ID)
	}

	// Conflicting lock on same container with a different request key is refused.
	if _, _, err := st.BeginOperation(Operation{
		RequestKey: "k2", Endpoint: "unix:///var/run/docker.sock",
		ContainerID: testID1, ContainerName: "billing-db", Action: "stop", Actor: "agent:ops",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict on locked target, got %v", err)
	}

	// Recovery marks interrupted operation unknown and releases the lock without replaying mutations.
	if err := st.Recover(); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.Operation(op1.ID)
	if err != nil || recovered.State != "unknown" {
		t.Fatalf("expected unknown state after recovery, got %+v err=%v", recovered, err)
	}

	// Target lock is now free again.
	if _, fresh3, err := st.BeginOperation(Operation{
		RequestKey: "k3", Endpoint: "unix:///var/run/docker.sock",
		ContainerID: testID1, ContainerName: "billing-db", Action: "start", Actor: "owner",
	}); err != nil || !fresh3 {
		t.Fatalf("expected lock released after recovery, got err=%v fresh=%v", err, fresh3)
	}
}

func TestConfirmationsRequireExactRevision(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rec, err := st.CreateConfirmation("container", "Stop billing-db", "Impact details", "Stop container", true, map[string]string{"planId": "plan-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Revision) != 64 {
		t.Fatalf("expected 64-hex revision, got %q", rec.Revision)
	}

	if _, err := st.ConsumeConfirmation(rec.ID, strings.Repeat("a", 64)); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected wrong revision to fail with ErrConflict, got %v", err)
	}
	consumed, err := st.ConsumeConfirmation(rec.ID, rec.Revision)
	if err != nil || consumed.ID != rec.ID {
		t.Fatalf("expected valid revision to consume once, got err=%v", err)
	}
	if _, err := st.ConsumeConfirmation(rec.ID, rec.Revision); err == nil {
		t.Fatalf("expected second consume of same confirmation to fail")
	}
}

func TestMonitorHysteresis(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ep := "unix:///var/run/docker.sock"
	m := DefaultMonitor(ep, "billing")
	m.Enabled = true
	m.IntervalSeconds = 30
	m.BadSamples = 2
	m.Actor = "owner"
	m, err = st.SaveMonitor(m)
	if err != nil {
		t.Fatal(err)
	}

	base := time.Now().UTC().Add(-10 * time.Minute)
	bad := []Signal{{Key: "health:api", Title: "api unhealthy", State: "bad", Detail: "probe failed"}}
	good := []Signal{{Key: "health:api", Title: "api healthy", State: "good", Detail: "probe ok"}}

	// Sample 1: pending, not yet open.
	if err := st.RecordHealth(ep, "billing", m.Revision, json.RawMessage(`{}`), bad, base); err != nil {
		t.Fatal(err)
	}
	if open, _ := st.OpenIncidents(); len(open) != 0 {
		t.Fatalf("expected 0 open incidents after 1 bad sample, got %d", len(open))
	}

	// Sample 2 (+31s): reaches BadSamples=2 -> open!
	if err := st.RecordHealth(ep, "billing", m.Revision, json.RawMessage(`{}`), bad, base.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
	open, _ := st.OpenIncidents()
	if len(open) != 1 || open[0].State != "open" {
		t.Fatalf("expected 1 open incident after 2 consecutive bad samples, got %+v", open)
	}

	// 1 good sample keeps it open; 2nd good sample resolves it.
	_ = st.RecordHealth(ep, "billing", m.Revision, json.RawMessage(`{}`), good, base.Add(62*time.Second))
	if open, _ = st.OpenIncidents(); len(open) != 1 {
		t.Fatalf("expected incident still open after 1 good sample")
	}
	_ = st.RecordHealth(ep, "billing", m.Revision, json.RawMessage(`{}`), good, base.Add(93*time.Second))
	if open, _ = st.OpenIncidents(); len(open) != 0 {
		t.Fatalf("expected incident resolved after 2 consecutive good samples, got %+v", open)
	}
}

func TestComposeWorkspaceBoundaryAndDiff(t *testing.T) {
	wsRoot := t.TempDir()
	outside := t.TempDir()
	projDir := filepath.Join(wsRoot, "billing")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "compose.yml"), []byte("services:\n  api:\n    image: ghcr.io/acme/api:2.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workspaces := []WorkspaceInfo{{ID: "ws-1", Name: "Main", Path: wsRoot, ExtensionOn: true}}

	// Outside enabled workspace is refused.
	if _, err := ValidateComposeRegistration(ComposeRegistration{WorkDir: outside}, workspaces); err == nil {
		t.Fatalf("expected folder outside workspace to be refused")
	}

	// Path traversal in files is refused.
	if _, err := ValidateComposeRegistration(ComposeRegistration{WorkDir: projDir, Files: []string{"../secret.yml"}}, workspaces); err == nil {
		t.Fatalf("expected file outside project dir to be refused")
	}

	fd := &fakeDocker{state1: "running", state2: "running", health2: "healthy"}
	_, connect := startFakeDocker(t, fd)
	c, err := connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	runner := func(_ context.Context, _ string, argv ...string) ([]byte, error) {
		return []byte(`{"services":{"api":{"image":"ghcr.io/acme/api:2.0","ports":[{"target":8080,"published":"8080","protocol":"tcp"}]},"worker":{"image":"ghcr.io/acme/worker:1.0"}}}`), nil
	}

	prev, err := PreviewCompose(context.Background(), runner, c, ComposeRegistration{Project: "billing", WorkDir: projDir}, workspaces)
	if err != nil || !prev.Valid {
		t.Fatalf("expected valid compose preview, err=%v prev=%+v", err, prev)
	}
	if len(prev.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(prev.Services))
	}
	// api has running image 1.0 vs compose 2.0 -> recreate; worker is new -> create.
	if prev.Services[0].Service != "api" || prev.Services[0].Action != "recreate" {
		t.Fatalf("expected api recreate, got %+v", prev.Services[0])
	}
	if prev.Services[1].Service != "worker" || prev.Services[1].Action != "create" {
		t.Fatalf("expected worker create, got %+v", prev.Services[1])
	}
}

func TestPlanDriftInboxDedupeAndTools(t *testing.T) {
	inboxCount := 0
	hostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ext/v1/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []map[string]any{{"id": "ag-1", "name": "OpsBot"}}})
		case "/api/ext/v1/inbox":
			inboxCount++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "inb-1"})
		case "/api/ext/v1/events":
			w.WriteHeader(http.StatusNoContent)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []any{}})
		}
	}))
	defer hostSrv.Close()

	st, err := OpenStore(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	fd := &fakeDocker{state1: "exited", state2: "running", health2: "unhealthy"}
	_, connect := startFakeDocker(t, fd)

	svc := NewService(st, &HostClient{BaseURL: hostSrv.URL, Token: "tok", HTTP: hostSrv.Client()})
	svc.Connect = connect

	ctx := context.Background()

	// 1. Agent start executes directly with lock + post-check.
	resStart := svc.ExecuteTool(ctx, ToolRequest{
		Protocol: ToolProtocol, Capability: "operate", Tool: "manage",
		Arguments: json.RawMessage(`{"id":"` + testID1 + `","action":"start"}`),
		Context:   ToolContext{Agent: "ag-1", Workspace: "ws-1"},
	})
	if resStart.IsError {
		t.Fatalf("expected start to succeed, got %+v", resStart)
	}

	// 2. Agent stop creates a 5-min plan and files exactly 1 Inbox item even if execute-plan is called again.
	resStop := svc.ExecuteTool(ctx, ToolRequest{
		Protocol: ToolProtocol, Capability: "operate", Tool: "manage",
		Arguments: json.RawMessage(`{"id":"` + testID1 + `","action":"stop"}`),
		Context:   ToolContext{Agent: "ag-1", Workspace: "ws-1"},
	})
	if resStop.IsError || resStop.StructuredContent["requiresApproval"] != true {
		t.Fatalf("expected stop to require approval, got %+v", resStop)
	}
	planObj := resStop.StructuredContent["plan"].(Plan)
	_ = svc.ExecuteTool(ctx, ToolRequest{
		Protocol: ToolProtocol, Capability: "maintain", Tool: "execute-plan",
		Arguments: json.RawMessage(`{"planId":"` + planObj.ID + `"}`),
		Context:   ToolContext{Agent: "ag-1", Workspace: "ws-1"},
	})
	if inboxCount != 1 {
		t.Fatalf("expected deduplicated single Inbox item, got %d", inboxCount)
	}

	// 3. State drift before owner executes plan fails safely without mutating drifted target!
	fd.mu.Lock()
	fd.state1 = "exited"
	fd.mu.Unlock()
	if _, err := svc.ExecutePlan(ctx, planObj.ID, "rk-drift", "owner", ""); err == nil || !strings.Contains(err.Error(), "changed state") {
		t.Fatalf("expected drift failure, got %v", err)
	}

	// 4. Single-item unused network removal plan succeeds and verifies absence.
	netPlan, err := svc.PreviewPlan(ctx, PlanInput{Kind: "resource", ResourceKind: "network", ResourceID: testNet, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.ExecutePlan(ctx, netPlan.ID, "rk-net", "owner", "")
	if err != nil || job.State != "succeeded" {
		t.Fatalf("expected unused network removal to succeed, got job=%+v err=%v", job, err)
	}
}
