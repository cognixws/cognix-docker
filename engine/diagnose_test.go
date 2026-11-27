package engine

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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
	ctxErr  error
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
		EvalSymlinks: func(p string) (string, error) {
			if l, ok := f.links[p]; ok {
				return l, nil
			}
			return p, nil
		},
		LookPath: func(n string) (string, error) {
			if p, ok := f.path[n]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		ContextHost: func(context.Context) (string, error) {
			if f.ctxErr != nil {
				return "", f.ctxErr
			}
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
	up := map[string]fs.FileMode{"/var/run/docker.sock": sock}
	shim := "/mnt/c/Program Files/Docker/Docker/resources/bin/docker"
	desktopSock := "/mnt/wsl/docker-desktop/shared-sockets/guest-services/docker.sock"
	for _, tc := range []struct {
		name     string
		f        fakeSystem
		want     State
		source   string
		rootless bool
	}{
		{"a rootless engine runs but the docker command uses the default socket",
			fakeSystem{env: map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}, path: map[string]string{"docker": "/usr/bin/docker", "dockerd-rootless.sh": "/usr/bin/dockerd-rootless.sh"},
				files: map[string]fs.FileMode{"/run/user/1000/docker.sock": sock}},
			StateStopped, SourceDefault, true},
		{"engine answers on the default socket",
			fakeSystem{path: native, files: up, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateReady, SourceDefault, false},
		{"rootless context answers",
			fakeSystem{context: "unix:///run/user/1000/docker.sock", path: map[string]string{"docker": "/usr/bin/docker", "dockerd-rootless.sh": "/usr/bin/dockerd-rootless.sh"},
				files: map[string]fs.FileMode{"/run/user/1000/docker.sock": sock}, engines: map[string]error{"/run/user/1000/docker.sock": nil}},
			StateReady, SourceContext, false},
		{"DOCKER_HOST names a reachable socket",
			fakeSystem{env: map[string]string{"DOCKER_HOST": "unix:///tmp/own.sock"}, files: map[string]fs.FileMode{"/tmp/own.sock": sock}, engines: map[string]error{"/tmp/own.sock": nil}},
			StateReady, SourceEnv, false},
		{"PICODE_DOCKER_HOST is boxes' override, not the docker command's",
			fakeSystem{env: map[string]string{"PICODE_DOCKER_HOST": "tcp://10.0.0.5:2375"}, path: native, files: up, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateReady, SourceDefault, false},
		// docker's own order: DOCKER_HOST outranks DOCKER_CONTEXT.
		{"DOCKER_HOST outranks DOCKER_CONTEXT",
			fakeSystem{env: map[string]string{"DOCKER_CONTEXT": "work", "DOCKER_HOST": "unix:///tmp/own.sock"}, context: "ssh://me@server", files: map[string]fs.FileMode{"/tmp/own.sock": sock}, engines: map[string]error{"/tmp/own.sock": nil}},
			StateReady, SourceEnv, false},
		{"Desktop's own CLI (linked into /mnt/wsl) with an ssh context",
			fakeSystem{context: "ssh://me@server", path: map[string]string{"docker": "/usr/bin/docker"},
				links: map[string]string{"/usr/bin/docker": "/mnt/wsl/docker-desktop/cli-tools/usr/bin/docker"}, files: up, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateRemote, SourceContext, false},
		{"a static rootless install in ~/bin, default socket",
			fakeSystem{path: map[string]string{"docker": "/home/u/bin/docker", "dockerd": "/home/u/bin/dockerd", "dockerd-rootless.sh": "/home/u/bin/dockerd-rootless.sh"}},
			StateStopped, SourceDefault, true},
		// Never another engine than the docker command's: a rootless or a
		// Desktop socket that answers does not make the default one ready.
		{"default socket missing, a rootless socket answers",
			fakeSystem{path: map[string]string{"docker": "/usr/bin/docker", "dockerd-rootless.sh": "/usr/bin/dockerd-rootless.sh"},
				files: map[string]fs.FileMode{"/run/user/1000/docker.sock": sock}, engines: map[string]error{"/run/user/1000/docker.sock": nil}},
			StateStopped, SourceDefault, true},
		{"integration off, a socket in the shared /mnt/wsl answers",
			fakeSystem{path: map[string]string{"docker": shim}, files: map[string]fs.FileMode{desktopMount: fs.ModeDir, desktopSock: sock}, engines: map[string]error{desktopSock: nil}},
			StateDesktop, SourceDefault, false},
		{"DOCKER_HOST pins a dead socket while the default answers",
			fakeSystem{env: map[string]string{"DOCKER_HOST": "unix:///tmp/gone.sock"}, path: native, files: up, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateSilent, SourceEnv, false},
		{"DOCKER_HOST over tcp",
			fakeSystem{env: map[string]string{"DOCKER_HOST": "tcp://10.0.0.5:2375"}, path: native},
			StateRemote, SourceEnv, false},
		{"selected context over ssh",
			fakeSystem{context: "ssh://me@server", path: native, files: up, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateRemote, SourceContext, false},
		{"DOCKER_CONTEXT names a context the command cannot read",
			fakeSystem{env: map[string]string{"DOCKER_CONTEXT": "gone"}, ctxErr: errors.New("context not found"), path: native, files: up, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateSilent, SourceContext, false},
		// The docker command fails the same way: never ready on a guess.
		{"the selected context cannot be read (deleted, or the lookup timed out)",
			fakeSystem{ctxErr: errors.New("context not found"), path: native, files: up, engines: map[string]error{"/var/run/docker.sock": nil}},
			StateSilent, SourceContext, false},
		{"socket refuses the account",
			fakeSystem{path: native, files: up, engines: map[string]error{"/var/run/docker.sock": syscall.EACCES}},
			StateDenied, SourceDefault, false},
		{"engine installed, socket missing (service not started)",
			fakeSystem{path: native},
			StateStopped, SourceDefault, false},
		{"engine installed, stale socket refuses connections, dockerd off PATH",
			fakeSystem{path: map[string]string{"docker": "/usr/bin/docker"}, files: map[string]fs.FileMode{"/var/run/docker.sock": sock, "/usr/sbin/dockerd": 0o755}},
			StateStopped, SourceDefault, false},
		{"a regular file where the socket should be",
			fakeSystem{path: native, files: map[string]fs.FileMode{"/var/run/docker.sock": 0o644}},
			StateStopped, SourceDefault, false},
		{"Docker Desktop's stand-in on PATH, integration off",
			fakeSystem{path: map[string]string{"docker": shim}},
			StateDesktop, SourceDefault, false},
		{"integration on, Desktop stopped: its link dangles",
			fakeSystem{links: map[string]string{desktopCLILink: "/mnt/wsl/docker-desktop/cli-tools/usr/bin/docker"}},
			StateDesktop, SourceDefault, false},
		{"Desktop installed on Windows, nothing in this Linux",
			fakeSystem{files: map[string]fs.FileMode{desktopWindows: fs.ModeDir}},
			StateDesktop, SourceDefault, false},
		{"docker command only (client package)",
			fakeSystem{path: map[string]string{"docker": "/usr/bin/docker"}},
			StateNoEngine, SourceDefault, false},
		{"nothing of Docker",
			fakeSystem{},
			StateAbsent, SourceDefault, false},
		{"engine too old",
			fakeSystem{path: native, files: up, engines: map[string]error{"/var/run/docker.sock": ErrIncompatible}},
			StateIncompatible, SourceDefault, false},
		{"socket answers garbage",
			fakeSystem{path: native, files: up, engines: map[string]error{"/var/run/docker.sock": errors.New("Docker returned 500")}},
			StateSilent, SourceDefault, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Diagnose(context.Background(), tc.f.sys())
			if (got.State == StateSilent && tc.source == SourceContext && tc.f.ctxErr != nil) != got.ContextUnreadable {
				t.Fatalf("contextUnreadable = %v", got.ContextUnreadable)
			}
			if got.RootlessUp != (got.Rootless && len(tc.f.files) > 0 && tc.f.files["/run/user/1000/docker.sock"] != 0) {
				t.Fatalf("rootlessUp = %v", got.RootlessUp)
			}
			if got.State != tc.want || got.Source != tc.source || got.Rootless != tc.rootless {
				t.Fatalf("got %s/%s rootless=%v, want %s/%s rootless=%v (%s)", got.State, got.Source, got.Rootless, tc.want, tc.source, tc.rootless, got.Detail)
			}
			// The distribution is read only where the copy names it.
			if (got.Distro == "COGNIX") != (got.State == StateDesktop) {
				t.Fatalf("distro %q for %s", got.Distro, got.State)
			}
		})
	}
}

func TestCacheKeepsADiagnosisForItsTTL(t *testing.T) {
	ctx := context.Background()
	c := &Cache{TTL: time.Hour}
	var mu sync.Mutex
	n := 0
	release := make(chan struct{})
	run := func(context.Context) Diagnosis {
		<-release
		mu.Lock()
		n++
		mu.Unlock()
		return Diagnosis{State: StateReady}
	}
	// Three routes of one page read at once: one run.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Get(ctx, "a", run) }()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	c.Get(ctx, "a", run)
	if n != 1 {
		t.Fatalf("ran %d times", n)
	}
	c.Get(ctx, "b", run) // another key is another question
	if n != 2 {
		t.Fatalf("ran %d times", n)
	}
}

func TestCacheIgnoresACallersCancellation(t *testing.T) {
	c := &Cache{TTL: time.Hour}
	started := make(chan struct{})
	finish := make(chan struct{})
	run := func(ctx context.Context) Diagnosis {
		close(started)
		<-finish
		if ctx.Err() != nil {
			return Diagnosis{State: StateSilent, Detail: ctx.Err().Error()}
		}
		return Diagnosis{State: StateReady}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	if d := c.Get(ctx, "a", run); d.State != StateSilent {
		t.Fatalf("the cancelled caller got %s", d.State)
	}
	close(finish)
	time.Sleep(20 * time.Millisecond)
	if d := c.Get(context.Background(), "a", run); d.State != StateReady {
		t.Fatalf("the next caller got %s: a cancellation was kept", d.State)
	}
}

func TestSearchPathAndWindowsDrive(t *testing.T) {
	got := SearchPath("/usr/bin:relative::/opt/x", "/home/u")
	want := "/usr/bin:/opt/x:/home/u/.local/bin:/home/u/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/bin"
	if got != want {
		t.Fatalf("SearchPath = %s", got)
	}
	sys := fakeSystem{links: map[string]string{"/usr/bin/docker": "/mnt/wsl/docker-desktop/cli-tools/usr/bin/docker", "/usr/local/bin/docker": "/mnt/c/Program Files/Docker/Docker/resources/bin/docker"}}.sys()
	for p, want := range map[string]bool{
		"/mnt/c/Program Files/Docker/Docker/resources/bin/docker": true,
		"/usr/local/bin/docker":                                   true,  // a link onto the Windows drive
		"/usr/bin/docker":                                         false, // Desktop's real CLI
		"/mnt/wsl/shared/docker":                                  false,
	} {
		if sys.onWindowsDrive(p) != want {
			t.Errorf("%s: onWindowsDrive = %v", p, !want)
		}
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
		{"unix://" + stale, StateSilent}, // a chosen connection that doesn't answer
		{"unix://" + filepath.Join(dir, "missing.sock"), StateSilent},
	} {
		sys := LocalSystem()
		sys.Getenv = func(k string) string {
			if k == "DOCKER_HOST" {
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
