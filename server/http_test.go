package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cfpperche/picode-docker/engine"
	"github.com/cfpperche/picode-docker/sysadmin"
)

const srvContainerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func setupTestServer(t *testing.T) (http.Handler, *sysadmin.Store) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "dksrv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	state := "running"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.45"})
	})
	mux.HandleFunc("GET /v1.44/containers/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Id": srvContainerID, "Names": []string{"/web"}, "Image": "nginx:alpine", "State": state, "Status": state},
		})
	})
	mux.HandleFunc("GET /v1.44/containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": srvContainerID, "Name": "/web", "Image": "sha256:1111",
			"Config": map[string]any{"Image": "nginx:alpine", "Tty": true},
			"State":  map[string]any{"Status": state},
		})
	})
	mux.HandleFunc("POST /v1.44/containers/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("action") == "stop" {
			state = "exited"
		} else {
			state = "running"
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	st, err := sysadmin.OpenStore(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := sysadmin.NewService(st, &sysadmin.HostClient{})
	svc.Connect = func(ctx context.Context) (*engine.Client, error) {
		return engine.NewClient("unix://" + sock)
	}
	return NewHandler(Config{ProxySecret: "proxy-secret-1", ToolKey: "tool-key-1", Service: svc}), st
}

func TestServerProxyAndConfirmAuth(t *testing.T) {
	h, _ := setupTestServer(t)

	// 1. Missing X-PiCode-Proxy-Secret is rejected 403.
	req := httptest.NewRequest(http.MethodGet, "/overview", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without proxy secret, got %d", rec.Code)
	}

	// 2. Prepare a stop confirmation via /confirmations.
	body := bytes.NewBufferString(`{"kind":"container","containerId":"` + srvContainerID + `","action":"stop"}`)
	req = httptest.NewRequest(http.MethodPost, "/confirmations", body)
	req.Header.Set("X-PiCode-Proxy-Secret", "proxy-secret-1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /confirmations, got %d: %s", rec.Code, rec.Body.String())
	}
	var prep struct {
		OperationID string `json:"operationId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &prep)

	// 3. Host reads GET /operations/{id} to display native confirm dialog.
	req = httptest.NewRequest(http.MethodGet, "/operations/"+prep.OperationID, nil)
	req.Header.Set("X-PiCode-Proxy-Secret", "proxy-secret-1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /operations/{id}, got %d", rec.Code)
	}
	var opView struct {
		Confirmation struct {
			Revision     string `json:"revision"`
			ConfirmLabel string `json:"confirmLabel"`
			Danger       bool   `json:"danger"`
		} `json:"confirmation"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &opView)
	if len(opView.Confirmation.Revision) != 64 || !opView.Confirmation.Danger {
		t.Fatalf("unexpected confirmation view: %+v", opView.Confirmation)
	}

	// 4. Calling /__picode/confirm WITHOUT X-PiCode-Owner-Confirmed (as if bypassed via /frame/process) is rejected 403!
	cfmBody, _ := json.Marshal(map[string]string{"operationId": prep.OperationID, "revision": opView.Confirmation.Revision})
	req = httptest.NewRequest(http.MethodPost, "/__picode/confirm", bytes.NewReader(cfmBody))
	req.Header.Set("X-PiCode-Proxy-Secret", "proxy-secret-1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without X-PiCode-Owner-Confirmed, got %d", rec.Code)
	}

	// 5. Calling /__picode/confirm WITH matching X-PiCode-Owner-Confirmed executes and stops the container!
	req = httptest.NewRequest(http.MethodPost, "/__picode/confirm", bytes.NewReader(cfmBody))
	req.Header.Set("X-PiCode-Proxy-Secret", "proxy-secret-1")
	req.Header.Set("X-PiCode-Owner-Confirmed", opView.Confirmation.Revision)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with owner confirmation header, got %d: %s", rec.Code, rec.Body.String())
	}

	// 6. Tool IPC endpoint rejects wrong X-Docker-Tool-Key and accepts right one.
	toolPayload := `{"protocol":"picode-tools/1","capability":"inspect","tool":"open-panel","arguments":{}}`
	req = httptest.NewRequest(http.MethodPost, "/tool", bytes.NewBufferString(toolPayload))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without tool key, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/tool", bytes.NewBufferString(toolPayload))
	req.Header.Set("X-Docker-Tool-Key", "tool-key-1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with tool key, got %d: %s", rec.Code, rec.Body.String())
	}
}
