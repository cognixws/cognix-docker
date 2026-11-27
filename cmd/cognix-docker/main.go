// Command cognix-docker runs both the extension's supervised background
// server (`cognix-docker server`) and its picode-tools/1 tool executor
// (`cognix-docker exec`).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cfpperche/picode-docker/engine"
	"github.com/cfpperche/picode-docker/server"
	"github.com/cfpperche/picode-docker/sysadmin"
)

type processBeacon struct {
	Port int    `json:"port"`
	Key  string `json:"key"`
	PID  int    `json:"pid"`
}

func main() {
	// The docker this process runs (docker compose) is the one the
	// diagnosis looked up: the same search path, ~/bin and the system
	// folders a systemd service's PATH lacks included.
	home, _ := os.UserHomeDir()
	_ = os.Setenv("PATH", engine.SearchPath(os.Getenv("PATH"), home))
	mode := "server"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "server":
		if err := runServer(); err != nil {
			log.Fatal(err)
		}
	case "exec":
		if err := runExec(); err != nil {
			out, _ := json.Marshal(sysadmin.ToolResult{
				Content: []sysadmin.ToolContent{{Type: "text", Text: err.Error()}},
				IsError: true,
			})
			_, _ = os.Stdout.Write(append(out, '\n'))
		}
	default:
		fmt.Fprintf(os.Stderr, "usage: cognix-docker [server|exec]\n")
		os.Exit(2)
	}
}

func runServer() error {
	portStr := strings.TrimSpace(os.Getenv("PICODE_EXT_PORT"))
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return errors.New("PICODE_EXT_PORT is required")
	}
	dataDir := strings.TrimSpace(os.Getenv("PICODE_EXT_DATA"))
	if dataDir == "" {
		dataDir = filepath.Join(os.TempDir(), "cognix-docker")
	}
	dbPath := strings.TrimSpace(os.Getenv("PICODE_EXT_DB"))
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "data.db")
	}

	st, err := sysadmin.OpenStore(dbPath)
	if err != nil {
		return fmt.Errorf("open sysadmin store: %w", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	host := sysadmin.NewHostClientFromEnv()
	svc := sysadmin.NewService(st, host)
	if err := svc.StartBackground(ctx); err != nil {
		return fmt.Errorf("start background tasks: %w", err)
	}

	var keyBytes [24]byte
	_, _ = rand.Read(keyBytes[:])
	toolKey := hex.EncodeToString(keyBytes[:])

	beaconPath := filepath.Join(dataDir, "process.json")
	if err := writeBeacon(beaconPath, processBeacon{Port: port, Key: toolKey, PID: os.Getpid()}); err != nil {
		return fmt.Errorf("write process beacon: %w", err)
	}
	defer func() { _ = os.Remove(beaconPath) }()

	srv := &http.Server{
		Addr:              "127.0.0.1:" + strconv.Itoa(port),
		Handler:           server.NewHandler(server.Config{ProxySecret: os.Getenv("PICODE_EXT_PROXY_SECRET"), ToolKey: toolKey, Service: svc}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("cognix-docker v2 server listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errCh:
		return err
	}
}

func writeBeacon(path string, b processBeacon) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func runExec() error {
	rawReq, err := io.ReadAll(io.LimitReader(os.Stdin, 512*1024))
	if err != nil {
		return err
	}
	var req sysadmin.ToolRequest
	if err := json.Unmarshal(rawReq, &req); err != nil {
		return fmt.Errorf("invalid tool input: %w", err)
	}

	dataDir := strings.TrimSpace(req.Context.DataDir)
	if dataDir == "" {
		dataDir = strings.TrimSpace(os.Getenv("PICODE_EXT_DATA"))
	}

	// Forward to the running extension background process when alive so only
	// one process writes data.db (Door E2 single-writer contract).
	if dataDir != "" {
		if res, ok := forwardToProcess(dataDir, rawReq); ok {
			_, err := os.Stdout.Write(append(res, '\n'))
			return err
		}
	}

	// Fallback when invoked while the process is not yet up: open data.db
	// briefly with busy_timeout, run read/plan tool, and exit cleanly.
	if dataDir == "" {
		return errors.New("Turn the Docker extension on in this workspace first.")
	}
	dbPath := filepath.Join(dataDir, "data.db")
	st, err := sysadmin.OpenStore(dbPath)
	if err != nil {
		return fmt.Errorf("open Docker extension store: %w", err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	svc := sysadmin.NewService(st, sysadmin.NewHostClientFromEnv())
	res := svc.ExecuteTool(ctx, req)
	out, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(out, '\n'))
	return err
}

func forwardToProcess(dataDir string, rawReq []byte) ([]byte, bool) {
	rawBeacon, err := os.ReadFile(filepath.Join(dataDir, "process.json"))
	if err != nil {
		return nil, false
	}
	var b processBeacon
	if err := json.Unmarshal(rawBeacon, &b); err != nil || b.Port <= 0 || b.Key == "" || b.PID <= 0 {
		return nil, false
	}
	if err := syscall.Kill(b.PID, 0); err != nil {
		return nil, false
	}
	client := &http.Client{Timeout: 55 * time.Second}
	httpReq, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/tool", b.Port), bytes.NewReader(rawReq))
	if err != nil {
		return nil, false
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Docker-Tool-Key", b.Key)
	res, err := client.Do(httpReq)
	if err != nil {
		return nil, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, false
	}
	return bytes.TrimSpace(body), true
}
