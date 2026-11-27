package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cfpperche/picode-docker/engine"
	"github.com/cfpperche/picode-docker/sysadmin"
)

// TestOverviewSaysWhichCase: when Docker cannot be used, /overview names the
// case (the same table as COGNIX[WS]'s Software row) with one sentence and
// one action, and an agent's tool gets the same sentence.
func TestOverviewSaysWhichCase(t *testing.T) {
	for _, tc := range []struct {
		d       engine.Diagnosis
		title   string
		action  string
		command string
		has     string
	}{
		{engine.Diagnosis{State: engine.StateStopped, Systemd: true}, "Docker is installed but not running", "copy", "sudo systemctl start docker", "Start it"},
		{engine.Diagnosis{State: engine.StateStopped}, "Docker is installed but not running", "copy", "sudo service docker start", "Start it"},
		{engine.Diagnosis{State: engine.StateDesktop, Distro: "COGNIX"}, "Docker Desktop is installed but not connected to COGNIX[WS]", "recheck", "", "WSL integration › COGNIX"},
		{engine.Diagnosis{State: engine.StateDenied, Endpoint: "unix:///var/run/docker.sock"}, "No permission to use Docker", "recheck", "", "Give access"},
		{engine.Diagnosis{State: engine.StateRemote, Endpoint: "ssh://me@server"}, "Docker points to another computer", "copy", "docker context use default", "ssh://me@server"},
		{engine.Diagnosis{State: engine.StateNoEngine}, "Docker's engine isn't installed", "recheck", "", "Software"},
		{engine.Diagnosis{State: engine.StateAbsent}, "Docker isn't installed", "recheck", "", "Software"},
		{engine.Diagnosis{State: engine.StateIncompatible}, "This Docker is too old", "recheck", "", "Docker 25"},
		{engine.Diagnosis{State: engine.StateSilent}, "Docker doesn't answer", "recheck", "", "Restart Docker"},
	} {
		t.Run(string(tc.d.State), func(t *testing.T) {
			st, err := sysadmin.OpenStore(filepath.Join(t.TempDir(), "data.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			svc := sysadmin.NewService(st, &sysadmin.HostClient{})
			d := tc.d
			svc.Connect = func(context.Context) (*engine.Client, error) { return nil, &engine.NotReadyError{Diagnosis: d} }
			h := NewHandler(Config{ProxySecret: "s", ToolKey: "k", Service: svc})

			req := httptest.NewRequest(http.MethodGet, "/overview", nil)
			req.Header.Set("X-PiCode-Proxy-Secret", "s")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			var ov struct {
				Reachable bool
				Error     string
				Problem   *engine.Problem `json:"problem"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil || rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			p := ov.Problem
			if ov.Reachable || p == nil || p.State != tc.d.State || p.Title != tc.title || p.Action.Kind != tc.action || p.Action.Command != tc.command || !strings.Contains(p.Sentence, tc.has) {
				t.Fatalf("overview = %s", rec.Body.String())
			}
			if n := strings.Count(strings.TrimSpace(p.Sentence), ". ") + 1; n > 2 {
				t.Errorf("%d sentences: %s", n, p.Sentence)
			}
			if ov.Error != p.Title+": "+p.Sentence {
				t.Errorf("error = %q", ov.Error)
			}

			req = httptest.NewRequest(http.MethodPost, "/tool", strings.NewReader(`{"protocol":"picode-tools/1","capability":"inspect","tool":"containers","arguments":{}}`))
			req.Header.Set("X-Docker-Tool-Key", "k")
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if !strings.Contains(rec.Body.String(), tc.title) {
				t.Errorf("tool answer = %s", rec.Body.String())
			}
		})
	}
}
