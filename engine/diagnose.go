package engine

// Diagnose says whether Docker can be used from this Linux, and when it
// cannot, which of the cases a person can act on it is. This is a copy of
// COGNIX[WS]'s internal/docker/diagnose.go (its Software page's Docker
// Engine row answers the same table), so the row's "installed" and this
// panel never disagree without saying why. Keep the two in step; the table
// is in docs/architecture/software.md, "Docker: installed and usable", and
// in TestDiagnose.

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
	"strings"
	"syscall"
	"time"
)

// State is one row of the diagnosis.
type State string

const (
	// StateReady: an engine answers on Endpoint.
	StateReady State = "ready"
	// StateRemote: DOCKER_HOST or the selected context points to tcp://,
	// ssh:// or a Windows pipe, which neither COGNIX[WS] nor the extension
	// reaches.
	StateRemote State = "remote"
	// StateDenied: a socket is there and refuses this account (not in the
	// docker group, or added to it without a new session).
	StateDenied State = "denied"
	// StateStopped: Docker Engine is installed in this Linux and no socket
	// answers (the service is not started; no systemd starts it).
	StateStopped State = "stopped"
	// StateDesktop: Docker Desktop is on this Windows and this Linux does not
	// see its engine: Desktop is not running or its WSL integration is off
	// for this distribution.
	StateDesktop State = "desktop"
	// StateNoEngine: the docker command is here and no engine is installed
	// (the client package alone).
	StateNoEngine State = "no-engine"
	// StateAbsent: nothing of Docker on this computer.
	StateAbsent State = "absent"
	// StateIncompatible: the engine answers with an API older than 1.44.
	StateIncompatible State = "incompatible"
	// StateSilent: a socket is there and does not answer for another reason.
	StateSilent State = "silent"
)

// Diagnosis is what Diagnose found.
type Diagnosis struct {
	State State `json:"state"`
	// Endpoint is the socket that answered (ready), refused (denied) or the
	// remote address (remote).
	Endpoint string `json:"endpoint,omitempty"`
	// Distro is this WSL distribution's name, for "turn on WSL integration
	// for <distro>"; empty outside WSL.
	Distro string `json:"distro,omitempty"`
	// Systemd says whether systemd runs here: it picks the start command.
	Systemd bool `json:"systemd"`
	// Detail is the engine's own error, for a log or a tooltip.
	Detail string `json:"detail,omitempty"`
}

// Desktop's sockets as a WSL distribution sees them: its shared socket in
// the tmpfs every WSL 2 distribution mounts at /mnt/wsl, and the socket a
// manual shared dockerd commonly uses. Tried only after the usual ones.
var desktopSockets = []string{
	"/mnt/wsl/docker-desktop/shared-sockets/guest-services/docker.sock",
	"/mnt/wsl/shared-docker/docker.sock",
}

// Docker Desktop leaves these behind in a WSL distribution: its shared
// mount (while it runs), its install folder on the Windows drive, and the
// /usr/bin/docker link its integration writes.
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
	LookPath func(string) (string, error)
	// ContextHost asks the docker command for the selected context's host;
	// nil when there is no usable docker command.
	ContextHost func(ctx context.Context) (string, error)
	// Ping reaches the engine on a Unix socket path: nil when it answers
	// with a compatible API.
	Ping   func(ctx context.Context, path string) error
	Distro func(ctx context.Context) string
	UID    int
}

// ErrIncompatible is Ping's answer for an engine older than API 1.44.
var ErrIncompatible = errors.New("this Docker API version is too old (requires API 1.44)")

// LocalSystem reads this computer.
func LocalSystem() System {
	return System{
		Getenv:   os.Getenv,
		Stat:     os.Stat,
		Readlink: os.Readlink,
		LookPath: exec.LookPath,
		ContextHost: func(ctx context.Context) (string, error) {
			path, err := exec.LookPath("docker")
			if err != nil || isWindowsPath(path) {
				return "", errNoContext
			}
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			out, err := exec.CommandContext(cctx, path, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
			if err != nil {
				return "", errNoContext
			}
			return strings.TrimSpace(string(out)), nil
		},
		Ping:   pingSocket,
		Distro: wslDistro,
		UID:    os.Getuid(),
	}
}

var errNoContext = errors.New("no docker context")

// isWindowsPath says a command lives on a Windows drive: Docker Desktop's
// stand-in that only says WSL integration is off, or Windows' own PATH.
func isWindowsPath(p string) bool {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return strings.HasPrefix(filepath.ToSlash(p), "/mnt/")
}

// Diagnose walks the table: an explicit connection first (PICODE_DOCKER_HOST,
// DOCKER_HOST without DOCKER_CONTEXT, a selected context), then the sockets
// Docker uses on Linux, rootless and Docker Desktop; when none answers, what
// is installed says which case it is.
func Diagnose(ctx context.Context, sys System) Diagnosis {
	d := Diagnosis{}
	if sys.Distro != nil {
		d.Distro = sys.Distro(ctx)
	}
	if fi, err := sys.Stat("/run/systemd/system"); err == nil && fi.IsDir() {
		d.Systemd = true
	}
	explicit, pinned := explicitHost(ctx, sys)
	candidates := []string{}
	if explicit != "" {
		path, ok := unixPath(explicit)
		if !ok {
			d.State, d.Endpoint = StateRemote, explicit
			return d
		}
		candidates = append(candidates, path)
	}
	if !pinned {
		candidates = append(candidates, "/var/run/docker.sock")
		if x := strings.TrimSpace(sys.Getenv("XDG_RUNTIME_DIR")); filepath.IsAbs(x) {
			candidates = append(candidates, filepath.Join(x, "docker.sock"))
		}
		if sys.UID > 0 {
			candidates = append(candidates, fmt.Sprintf("/run/user/%d/docker.sock", sys.UID))
		}
		candidates = append(candidates, desktopSockets...)
	}
	var denied, silent string
	var deniedErr, silentErr error
	seen := map[string]bool{}
	for _, p := range candidates {
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		fi, err := sys.Stat(p)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) && denied == "" {
				denied, deniedErr = p, err
			}
			continue
		}
		if fi.Mode()&fs.ModeSocket == 0 {
			continue
		}
		err = sys.Ping(ctx, p)
		switch {
		case err == nil:
			d.State, d.Endpoint = StateReady, "unix://"+p
			return d
		case errors.Is(err, ErrIncompatible):
			d.State, d.Endpoint, d.Detail = StateIncompatible, "unix://"+p, err.Error()
			return d
		case isDenied(err):
			if denied == "" {
				denied, deniedErr = p, err
			}
		case isDead(err):
			// A socket file left behind by a stopped engine.
		default:
			if silent == "" {
				silent, silentErr = p, err
			}
		}
	}
	if denied != "" {
		d.State, d.Endpoint, d.Detail = StateDenied, "unix://"+denied, deniedErr.Error()
		return d
	}
	if silent != "" {
		d.State, d.Endpoint, d.Detail = StateSilent, "unix://"+silent, silentErr.Error()
		return d
	}
	cli, cliErr := sys.LookPath("docker")
	desktopCLI := cliErr == nil && isWindowsPath(cli)
	if link, err := sys.Readlink(desktopCLILink); err == nil && strings.Contains(link, "docker-desktop") {
		desktopCLI = true
	}
	switch {
	case hasEngine(sys):
		d.State = StateStopped
	case desktopCLI || exists(sys, desktopMount) || exists(sys, desktopWindows):
		d.State = StateDesktop
	case cliErr == nil:
		d.State = StateNoEngine
	default:
		d.State = StateAbsent
	}
	return d
}

// explicitHost is the connection the person chose; pinned says only that
// one counts (an environment variable), not the fallbacks.
func explicitHost(ctx context.Context, sys System) (host string, pinned bool) {
	if h := strings.TrimSpace(sys.Getenv("PICODE_DOCKER_HOST")); h != "" {
		return h, true
	}
	if sys.Getenv("DOCKER_CONTEXT") == "" {
		if h := strings.TrimSpace(sys.Getenv("DOCKER_HOST")); h != "" {
			return h, true
		}
	}
	if sys.ContextHost != nil {
		if h, err := sys.ContextHost(ctx); err == nil && h != "" {
			// The default context names /var/run/docker.sock: the
			// fallbacks still apply. Any other context is the person's
			// choice, and DOCKER_CONTEXT pins it.
			return h, sys.Getenv("DOCKER_CONTEXT") != "" || h != "unix:///var/run/docker.sock"
		}
	}
	return "", false
}

func unixPath(endpoint string) (string, bool) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || !filepath.IsAbs(u.Path) || u.Path == "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	return filepath.Clean(u.Path), true
}

func hasEngine(sys System) bool {
	for _, name := range []string{"dockerd", "dockerd-rootless.sh"} {
		if p, err := sys.LookPath(name); err == nil && !isWindowsPath(p) {
			return true
		}
	}
	// A systemd service's PATH may lack /usr/sbin.
	for _, p := range []string{"/usr/bin/dockerd", "/usr/sbin/dockerd", "/usr/local/bin/dockerd"} {
		if exists(sys, p) {
			return true
		}
	}
	return false
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
