package engine

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type fakeInfo struct {
	name string
	mode fs.FileMode
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

const sock = fs.ModeSocket | 0o660

// fakeSystem is a computer described by its files, PATH and engines.
type fakeSystem struct {
	env     map[string]string
	files   map[string]fs.FileMode
	links   map[string]string
	path    map[string]string
	context string
	engines map[string]error // socket path → Ping's answer
}

func (f fakeSystem) sys() System {
	return System{
		Getenv: func(k string) string { return f.env[k] },
		Stat: func(p string) (fs.FileInfo, error) {
			if m, ok := f.files[p]; ok {
				return fakeInfo{filepath.Base(p), m}, nil
			}
			return nil, fs.ErrNotExist
		},
		Readlink: func(p string) (string, error) {
			if l, ok := f.links[p]; ok {
				return l, nil
			}
			return "", fs.ErrNotExist
		},
		LookPath: func(n string) (string, error) {
			if p, ok := f.path[n]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		ContextHost: func(context.Context) (string, error) {
			if f.context == "" {
				return "", errNoContext
			}
			return f.context, nil
		},
		Ping: func(_ context.Context, p string) error {
			if err, ok := f.engines[p]; ok {
				return err
			}
			return syscall.ECONNREFUSED
		},
		Distro: func(context.Context) string { return "COGNIX" },
		UID:    1000,
	}
}

// TestDiagnose is the decision table in docs/architecture/software.md,
// "Docker: installed and usable": one row per case.
func TestDiagnose(t *testing.T) {
	native := map[string]string{"docker": "/usr/bin/docker", "dockerd": "/usr/bin/dockerd"}
	for _, tc := range []struct {
		name     string
		f        fakeSystem
		want     State
		endpoint string
	}{
		{"engine answers on the usual socket",
			fakeSystem{path: native, files: map[string]fs.FileMode{"/var/run/docker.sock": sock}, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateReady, "unix:///var/run/docker.sock"},
		{"rootless socket in XDG_RUNTIME_DIR",
			fakeSystem{env: map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}, path: map[string]string{"docker": "/home/u/bin/docker", "dockerd-rootless.sh": "/home/u/bin/dockerd-rootless.sh"},
				files: map[string]fs.FileMode{"/run/user/1000/docker.sock": sock}, engines: map[string]error{"/run/user/1000/docker.sock": nil}},
			StateReady, "unix:///run/user/1000/docker.sock"},
		{"rootless socket without XDG_RUNTIME_DIR (a systemd service)",
			fakeSystem{files: map[string]fs.FileMode{"/run/user/1000/docker.sock": sock}, engines: map[string]error{"/run/user/1000/docker.sock": nil}},
			StateReady, "unix:///run/user/1000/docker.sock"},
		{"Docker Desktop's shared WSL socket, integration off",
			fakeSystem{path: map[string]string{"docker": "/mnt/c/Program Files/Docker/Docker/resources/bin/docker"},
				files:   map[string]fs.FileMode{desktopMount: fs.ModeDir, desktopSockets[0]: sock},
				engines: map[string]error{desktopSockets[0]: nil}},
			StateReady, "unix://" + desktopSockets[0]},
		{"stale socket file beside a working Desktop socket",
			fakeSystem{files: map[string]fs.FileMode{"/var/run/docker.sock": sock, desktopSockets[0]: sock}, engines: map[string]error{desktopSockets[0]: nil}},
			StateReady, "unix://" + desktopSockets[0]},
		{"DOCKER_HOST names a reachable socket",
			fakeSystem{env: map[string]string{"DOCKER_HOST": "unix:///tmp/own.sock"}, files: map[string]fs.FileMode{"/tmp/own.sock": sock}, engines: map[string]error{"/tmp/own.sock": nil}},
			StateReady, "unix:///tmp/own.sock"},
		{"DOCKER_HOST pins a dead socket: no fallback to another engine",
			fakeSystem{env: map[string]string{"DOCKER_HOST": "unix:///tmp/gone.sock"}, path: native,
				files: map[string]fs.FileMode{"/var/run/docker.sock": sock}, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateStopped, ""},
		{"DOCKER_HOST over tcp",
			fakeSystem{env: map[string]string{"DOCKER_HOST": "tcp://10.0.0.5:2375"}, path: native},
			StateRemote, "tcp://10.0.0.5:2375"},
		{"selected context over ssh",
			fakeSystem{context: "ssh://me@server", path: native, files: map[string]fs.FileMode{"/var/run/docker.sock": sock}, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateRemote, "ssh://me@server"},
		{"default context keeps the fallbacks",
			fakeSystem{context: "unix:///var/run/docker.sock", files: map[string]fs.FileMode{"/run/user/1000/docker.sock": sock}, engines: map[string]error{"/run/user/1000/docker.sock": nil}},
			StateReady, "unix:///run/user/1000/docker.sock"},
		{"socket refuses the account",
			fakeSystem{path: native, files: map[string]fs.FileMode{"/var/run/docker.sock": sock}, engines: map[string]error{"/var/run/docker.sock": syscall.EACCES}},
			StateDenied, "unix:///var/run/docker.sock"},
		{"engine installed, socket missing (service not started)",
			fakeSystem{path: native},
			StateStopped, ""},
		{"engine installed, stale socket refuses connections",
			fakeSystem{path: map[string]string{"docker": "/usr/bin/docker"}, files: map[string]fs.FileMode{"/var/run/docker.sock": sock, "/usr/sbin/dockerd": 0o755}},
			StateStopped, ""},
		{"Docker Desktop's stand-in on PATH, integration off, Desktop stopped",
			fakeSystem{path: map[string]string{"docker": "/mnt/c/Program Files/Docker/Docker/resources/bin/docker"}},
			StateDesktop, ""},
		{"integration on, Desktop stopped: its link dangles",
			fakeSystem{links: map[string]string{desktopCLILink: "/mnt/wsl/docker-desktop/cli-tools/usr/bin/docker"}},
			StateDesktop, ""},
		{"Desktop installed on Windows, nothing in this Linux",
			fakeSystem{files: map[string]fs.FileMode{desktopWindows: fs.ModeDir}},
			StateDesktop, ""},
		{"Desktop running, its shared socket refuses this distro",
			fakeSystem{files: map[string]fs.FileMode{desktopMount: fs.ModeDir, desktopSockets[0]: sock}, engines: map[string]error{desktopSockets[0]: syscall.ECONNREFUSED}},
			StateDesktop, ""},
		{"docker command only (client package)",
			fakeSystem{path: map[string]string{"docker": "/usr/bin/docker"}},
			StateNoEngine, ""},
		{"nothing of Docker",
			fakeSystem{},
			StateAbsent, ""},
		{"engine too old",
			fakeSystem{path: native, files: map[string]fs.FileMode{"/var/run/docker.sock": sock}, engines: map[string]error{"/var/run/docker.sock": ErrIncompatible}},
			StateIncompatible, "unix:///var/run/docker.sock"},
		{"socket answers garbage",
			fakeSystem{path: native, files: map[string]fs.FileMode{"/var/run/docker.sock": sock}, engines: map[string]error{"/var/run/docker.sock": errors.New("Docker returned 500")}},
			StateSilent, "unix:///var/run/docker.sock"},
		{"a regular file where the socket should be",
			fakeSystem{path: native, files: map[string]fs.FileMode{"/var/run/docker.sock": 0o644}},
			StateStopped, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Diagnose(context.Background(), tc.f.sys())
			if got.State != tc.want || got.Endpoint != tc.endpoint {
				t.Fatalf("got %s %q, want %s %q (%s)", got.State, got.Endpoint, tc.want, tc.endpoint, got.Detail)
			}
			if got.Distro != "COGNIX" {
				t.Fatalf("distro %q", got.Distro)
			}
		})
	}
}

// TestDiagnoseRealSockets reproduces the cases on real Unix sockets in a
// temporary folder, never the computer's own engine.
func TestDiagnoseRealSockets(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores socket permissions")
	}
	dir := t.TempDir()
	serve := func(name, api string) string {
		p := filepath.Join(dir, name)
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ApiVersion":"` + api + `","MinAPIVersion":"1.24"}`))
		})}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() { _ = srv.Close() })
		return p
	}
	ok := serve("ok.sock", "1.47")
	old := serve("old.sock", "1.41")
	denied := serve("denied.sock", "1.47")
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale.sock")
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()

	for _, tc := range []struct {
		host string
		want State
	}{
		{"unix://" + ok, StateReady},
		{"unix://" + old, StateIncompatible},
		{"unix://" + denied, StateDenied},
		{"unix://" + stale, StateAbsent},
		{"unix://" + filepath.Join(dir, "missing.sock"), StateAbsent},
	} {
		sys := LocalSystem()
		sys.Getenv = func(k string) string {
			if k == "PICODE_DOCKER_HOST" {
				return tc.host
			}
			return ""
		}
		// Only the socket under test: nothing installed is looked up.
		sys.LookPath = func(string) (string, error) { return "", errors.New("not found") }
		sys.Readlink = func(string) (string, error) { return "", fs.ErrNotExist }
		real := sys.Stat
		sys.Stat = func(p string) (fs.FileInfo, error) {
			if filepath.Dir(p) != dir {
				return nil, fs.ErrNotExist
			}
			return real(p)
		}
		sys.Distro = func(context.Context) string { return "" }
		if got := Diagnose(context.Background(), sys); got.State != tc.want {
			t.Errorf("%s: got %s (%s), want %s", filepath.Base(tc.host), got.State, got.Detail, tc.want)
		}
	}
}

func TestDistroFromUNC(t *testing.T) {
	for in, want := range map[string]string{
		`\\wsl.localhost\Ubuntu\` + "\n": "Ubuntu",
		`\\wsl$\COGNIX-WS\`:              "COGNIX-WS",
		`C:\`:                            "",
		"":                               "",
	} {
		if got := DistroFromUNC(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
