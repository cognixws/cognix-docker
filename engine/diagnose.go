package engine

// Diagnose says whether Docker can be used from this Linux, and when it
// cannot, which of the cases a person can act on it is. This is a copy of
// COGNIX[WS]'s internal/docker/diagnose.go (its Software page's Docker
// Engine row answers the same table), so the row's "installed" and this
// panel never disagree without saying why. Keep the two in step; the table
// is in docs/architecture/software.md, "Docker: installed and usable", and
// in TestDiagnose.
//
// It only ever reaches the engine the docker command itself uses: the
// connection the person chose, or /var/run/docker.sock. Other sockets
// (rootless, Docker Desktop's in the shared /mnt/wsl, which any account
// of any distribution can write) are never connected to; what is
// installed only says which case it is.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// State is one row of the diagnosis.
type State string

const (
	// StateReady: the engine the docker command uses answers.
	StateReady State = "ready"
	// StateRemote: the chosen connection is tcp://, ssh:// or a Windows
	// pipe, which neither COGNIX[WS] nor the extension reaches.
	StateRemote State = "remote"
	// StateDenied: the socket is there and refuses this account (not in
	// the docker group, or added to it without a new session).
	StateDenied State = "denied"
	// StateStopped: Docker Engine is installed in this Linux and its
	// socket does not answer (the service is not started).
	StateStopped State = "stopped"
	// StateDesktop: Docker Desktop is on this Windows and this Linux has
	// no engine of its own: Desktop is not running or its WSL integration
	// is off for this distribution.
	StateDesktop State = "desktop"
	// StateNoEngine: the docker command is here and no engine is installed.
	StateNoEngine State = "no-engine"
	// StateAbsent: nothing of Docker on this computer.
	StateAbsent State = "absent"
	// StateIncompatible: the engine answers with an API older than 1.44.
	StateIncompatible State = "incompatible"
	// StateSilent: the socket is there and does not answer for another
	// reason, or a connection the person chose does not answer.
	StateSilent State = "silent"
)

// Where the connection came from.
const (
	SourceDefault = "default" // /var/run/docker.sock
	SourceEnv     = "env"     // DOCKER_HOST
	SourceContext = "context" // the docker command's selected context
)

const defaultSocket = "/var/run/docker.sock"

// Diagnosis is what Diagnose found.
type Diagnosis struct {
	State State `json:"state"`
	// Endpoint is the connection that was tried.
	Endpoint string `json:"endpoint,omitempty"`
	// Source says where Endpoint came from: default, env or context.
	Source string `json:"source,omitempty"`
	// Distro is this WSL distribution's name, read only for the desktop
	// state ("turn on WSL integration for <distro>").
	Distro string `json:"distro,omitempty"`
	// Systemd says whether systemd runs here; Rootless that the engine
	// installed is a rootless one only. Both pick the start command.
	Systemd  bool `json:"systemd"`
	Rootless bool `json:"rootless,omitempty"`
	// Detail is the engine's own error, for a log or a tooltip.
	Detail string `json:"detail,omitempty"`
}

// Docker Desktop leaves these behind in a WSL distribution: its shared
// mount (while it runs), its install folder on the Windows drive, and the
// /usr/bin/docker link its integration writes. Evidence only.
const (
	desktopMount   = "/mnt/wsl/docker-desktop"
	desktopWindows = "/mnt/c/Program Files/Docker/Docker"
	desktopCLILink = "/usr/bin/docker"
)

// System is what Diagnose reads; tests replace every field.
type System struct {
	Getenv   func(string) string
	Stat     func(string) (fs.FileInfo, error)
	Readlink func(string) (string, error)
	// EvalSymlinks resolves a command's links (Docker Desktop's CLI is one).
	EvalSymlinks func(string) (string, error)
	// LookPath finds a command on SearchPath.
	LookPath func(string) (string, error)
	// ContextHost asks the docker command for the selected context's host.
	ContextHost func(ctx context.Context) (string, error)
	// Ping reaches the engine on a Unix socket path: nil when it answers
	// with a compatible API.
	Ping   func(ctx context.Context, path string) error
	Distro func(ctx context.Context) string
}

// ErrIncompatible is Ping's answer for an engine older than API 1.44.
var ErrIncompatible = errors.New("this Docker API version is too old (requires API 1.44)")

var errNoContext = errors.New("no docker context")

// SearchPath is where Diagnose looks for docker, dockerd and
// dockerd-rootless.sh, the same in COGNIX[WS] and in the Docker extension:
// the process's PATH, then the person's ~/.local/bin and ~/bin (a rootless
// or static install) and the system folders a systemd service's PATH may
// lack. The extension runs with it as its PATH, so its docker compose is
// the docker diagnosed here.
func SearchPath(path, home string) string {
	parts := filepath.SplitList(path)
	if filepath.IsAbs(home) {
		parts = append(parts, filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"))
	}
	parts = append(parts, "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin")
	seen := map[string]bool{}
	out := []string{}
	for _, d := range parts {
		if d == "" || !filepath.IsAbs(d) || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return strings.Join(out, string(os.PathListSeparator))
}

func lookIn(path, name string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// LocalSystem reads this computer.
func LocalSystem() System {
	home, _ := os.UserHomeDir()
	path := SearchPath(os.Getenv("PATH"), home)
	look := func(name string) (string, error) { return lookIn(path, name) }
	sys := System{
		Getenv:       os.Getenv,
		Stat:         os.Stat,
		Readlink:     os.Readlink,
		EvalSymlinks: filepath.EvalSymlinks,
		LookPath:     look,
		Ping:         pingSocket,
		Distro:       wslDistro,
	}
	sys.ContextHost = func(ctx context.Context) (string, error) {
		cli, err := look("docker")
		if err != nil || sys.onWindowsDrive(cli) {
			return "", errNoContext
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, cli, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			return "", fmt.Errorf("the selected Docker context could not be read: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	return sys
}

var windowsDrive = regexp.MustCompile(`^/mnt/[a-zA-Z]/`)

// onWindowsDrive says a command lives on a Windows drive (/mnt/c/…):
// Docker Desktop's stand-in that only says WSL integration is off, or
// Windows' own PATH. Desktop's real CLI, linked into /mnt/wsl/…, is not.
func (sys System) onWindowsDrive(p string) bool {
	if sys.EvalSymlinks != nil {
		if real, err := sys.EvalSymlinks(p); err == nil {
			p = real
		}
	}
	return windowsDrive.MatchString(filepath.ToSlash(p))
}

// Diagnose reaches the engine the docker command uses and, when it does
// not answer, reads what is installed to say which case it is.
func Diagnose(ctx context.Context, sys System) Diagnosis {
	d := Diagnosis{}
	if fi, err := sys.Stat("/run/systemd/system"); err == nil && fi.IsDir() {
		d.Systemd = true
	}
	host, source, err := chosenHost(ctx, sys)
	d.Endpoint, d.Source = host, source
	if err != nil {
		d.State, d.Detail = StateSilent, err.Error()
		return d
	}
	path, ok := unixPath(host)
	if !ok {
		d.State = StateRemote
		return d
	}
	fi, err := sys.Stat(path)
	switch {
	case err != nil && errors.Is(err, fs.ErrPermission):
		d.State, d.Detail = StateDenied, err.Error()
		return d
	case err == nil && fi.Mode()&fs.ModeSocket != 0:
		err = sys.Ping(ctx, path)
		switch {
		case err == nil:
			d.State = StateReady
			return d
		case errors.Is(err, ErrIncompatible):
			d.State, d.Detail = StateIncompatible, err.Error()
			return d
		case isDenied(err):
			d.State, d.Detail = StateDenied, err.Error()
			return d
		case !isDead(err):
			d.State, d.Detail = StateSilent, err.Error()
			return d
		}
		// A socket file a stopped engine left behind: nothing answers.
	}
	if source != SourceDefault {
		// The connection the person chose does not answer; starting
		// another engine would not change what the docker command uses.
		d.State = StateSilent
		return d
	}
	cli, cliErr := sys.LookPath("docker")
	desktopCLI := cliErr == nil && sys.onWindowsDrive(cli)
	if link, err := sys.Readlink(desktopCLILink); err == nil && strings.Contains(link, "docker-desktop") {
		desktopCLI = true
	}
	switch {
	case hasSystemEngine(sys):
		d.State = StateStopped
	case hasCommand(sys, "dockerd-rootless.sh"):
		d.State, d.Rootless = StateStopped, true
	case desktopCLI || exists(sys, desktopMount) || exists(sys, desktopWindows):
		d.State = StateDesktop
		if sys.Distro != nil {
			d.Distro = sys.Distro(ctx)
		}
	case cliErr == nil:
		d.State = StateNoEngine
	default:
		d.State = StateAbsent
	}
	return d
}

// chosenHost is the connection the docker command uses, in docker's own
// order: DOCKER_HOST, then the context (DOCKER_CONTEXT or the selected
// one), then the default socket. PICODE_DOCKER_HOST (boxes' own override)
// is not read: the docker command and the extension's process, which never
// gets PICODE_ variables, do not see it either. A context the command
// cannot read is an error: the command would fail too.
func chosenHost(ctx context.Context, sys System) (host, source string, err error) {
	if h := strings.TrimSpace(sys.Getenv("DOCKER_HOST")); h != "" {
		return h, SourceEnv, nil
	}
	if sys.ContextHost != nil {
		h, err := sys.ContextHost(ctx)
		switch {
		case err == nil && h != "" && h != "unix://"+defaultSocket:
			return h, SourceContext, nil
		case err != nil && !errors.Is(err, errNoContext):
			return "", SourceContext, err
		}
	}
	return "unix://" + defaultSocket, SourceDefault, nil
}

func unixPath(endpoint string) (string, bool) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || !filepath.IsAbs(u.Path) || u.Path == "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	return filepath.Clean(u.Path), true
}

// hasSystemEngine: a dockerd the system installed (a package's service),
// not one in the person's own folders (a static or rootless install).
func hasSystemEngine(sys System) bool {
	if p, err := sys.LookPath("dockerd"); err == nil && systemDir(p) {
		return true
	}
	// A systemd service's PATH may lack /usr/sbin.
	for _, p := range []string{"/usr/bin/dockerd", "/usr/sbin/dockerd", "/usr/local/bin/dockerd"} {
		if exists(sys, p) {
			return true
		}
	}
	return false
}

func systemDir(p string) bool {
	for _, d := range []string{"/usr/", "/bin/", "/sbin/", "/opt/"} {
		if strings.HasPrefix(p, d) {
			return true
		}
	}
	return false
}

func hasCommand(sys System, name string) bool {
	p, err := sys.LookPath(name)
	return err == nil && !sys.onWindowsDrive(p)
}

func exists(sys System, p string) bool {
	_, err := sys.Stat(p)
	return err == nil
}

func isDenied(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) || errors.Is(err, fs.ErrPermission)
}

func isDead(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) || errors.Is(err, fs.ErrNotExist)
}

// pingSocket asks the engine on path for its version.
func pingSocket(ctx context.Context, path string) error {
	c, err := NewClient("unix://" + path)
	if err != nil {
		return err
	}
	defer c.Close()
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var v struct{ APIVersion, MinAPIVersion string }
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) {
			return op.Err
		}
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Docker returned %s", res.Status)
	}
	if err := decodeJSON(res, &v); err != nil {
		return err
	}
	if !apiCompatible(v.APIVersion, v.MinAPIVersion) {
		return ErrIncompatible
	}
	return nil
}

// wslDistro is this distribution's name: wslpath answers under systemd,
// where WSL_DISTRO_NAME is not set.
func wslDistro(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(cctx, "wslpath", "-w", "/").Output(); err == nil {
		if name := DistroFromUNC(string(out)); name != "" {
			return name
		}
	}
	return os.Getenv("WSL_DISTRO_NAME")
}

// DistroFromUNC reads the distribution from `wslpath -w /`:
// \\wsl.localhost\Ubuntu\ or \\wsl$\Ubuntu\.
func DistroFromUNC(s string) string {
	p := strings.Trim(strings.TrimSpace(s), `\`)
	if i := strings.IndexByte(p, '\\'); i > 0 {
		return p[i+1:]
	}
	return ""
}

// Cache keeps a diagnosis for TTL per key, so a page that reads several
// routes at once, or a loop, asks Docker once. Callers that arrive while a
// diagnosis runs wait for it (one run, not three), and the run does not
// carry a caller's cancellation: a request the person left must not leave
// "doesn't answer" behind for everyone else.
type Cache struct {
	TTL time.Duration
	mu  sync.Mutex
	key string
	at  time.Time
	d   Diagnosis
	run *flight
}

type flight struct {
	key  string
	done chan struct{}
	d    Diagnosis
}

// Get answers the kept diagnosis for key while it is fresh, else runs one.
func (c *Cache) Get(ctx context.Context, key string, run func(context.Context) Diagnosis) Diagnosis {
	c.mu.Lock()
	if c.key == key && !c.at.IsZero() && time.Since(c.at) < c.TTL {
		d := c.d
		c.mu.Unlock()
		return d
	}
	f := c.run
	if f == nil || f.key != key {
		f = &flight{key: key, done: make(chan struct{})}
		c.run = f
		go func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			f.d = run(rctx)
			c.mu.Lock()
			c.key, c.at, c.d = key, time.Now(), f.d
			if c.run == f {
				c.run = nil
			}
			c.mu.Unlock()
			close(f.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.d
	case <-ctx.Done():
		return Diagnosis{State: StateSilent, Detail: ctx.Err().Error()}
	}
}
