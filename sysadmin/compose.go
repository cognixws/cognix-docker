package sysadmin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cfpperche/picode-docker/engine"
)

// CommandRunner executes a fixed argv command inside dir without a shell.
type CommandRunner func(ctx context.Context, dir string, argv ...string) ([]byte, error)

func DefaultCommandRunner(ctx context.Context, dir string, argv ...string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("missing command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

var (
	composeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
	defaultFiles  = []string{"compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml"}
)

type ComposeServiceDiff struct {
	Service      string   `json:"service"`
	Image        string   `json:"image"`
	RunningImage string   `json:"runningImage,omitempty"`
	State        string   `json:"state,omitempty"`
	Action       string   `json:"action"` // create | recreate | start | unchanged
	Ports        []string `json:"ports,omitempty"`
}

type ComposePreview struct {
	Project     string               `json:"project"`
	WorkDir     string               `json:"workDir"`
	WorkspaceID string               `json:"workspaceId,omitempty"`
	Files       []string             `json:"files"`
	Profiles    []string             `json:"profiles,omitempty"`
	Valid       bool                 `json:"valid"`
	Error       string               `json:"error,omitempty"`
	Services    []ComposeServiceDiff `json:"services"`
	Fingerprint string               `json:"fingerprint"`
}

// ResolveComposePath inspects a file or folder path (for example from Open With)
// and builds a normalized ComposeRegistration.
func ResolveComposePath(targetPath string) (ComposeRegistration, error) {
	p := strings.TrimSpace(targetPath)
	if p == "" {
		return ComposeRegistration{}, errors.New("choose a Compose file or folder")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return ComposeRegistration{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return ComposeRegistration{}, fmt.Errorf("Compose path not found: %s", filepath.Base(abs))
	}
	var dir string
	var files []string
	if info.IsDir() {
		dir = abs
		for _, candidate := range defaultFiles {
			if st, e := os.Stat(filepath.Join(dir, candidate)); e == nil && st.Mode().IsRegular() {
				files = []string{candidate}
				break
			}
		}
		if len(files) == 0 {
			return ComposeRegistration{}, errors.New("no compose.yml or docker-compose.yml found in that folder")
		}
	} else {
		dir = filepath.Dir(abs)
		base := filepath.Base(abs)
		lower := strings.ToLower(base)
		if strings.Contains(lower, "compose") && (strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml")) {
			files = []string{base}
		} else {
			for _, candidate := range defaultFiles {
				if st, e := os.Stat(filepath.Join(dir, candidate)); e == nil && st.Mode().IsRegular() {
					files = []string{candidate}
					break
				}
			}
			if len(files) == 0 {
				return ComposeRegistration{}, errors.New("no Compose file found next to " + base)
			}
		}
	}
	proj := sanitizeProjectName(filepath.Base(dir))
	return ComposeRegistration{Project: proj, WorkDir: dir, Files: files}, nil
}

func sanitizeProjectName(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	res := strings.Trim(b.String(), "-_.")
	if res == "" {
		return "stack"
	}
	if len(res) > 63 {
		res = res[:63]
	}
	return res
}

// ValidateComposeRegistration enforces workspace containment and safe CLI flags.
func ValidateComposeRegistration(reg ComposeRegistration, workspaces []WorkspaceInfo) (ComposeRegistration, error) {
	if strings.TrimSpace(reg.WorkDir) == "" {
		return reg, errors.New("choose a project folder")
	}
	absDir, err := filepath.Abs(reg.WorkDir)
	if err != nil {
		return reg, errors.New("invalid project folder")
	}
	if realDir, e := filepath.EvalSymlinks(absDir); e == nil {
		absDir = realDir
	}
	st, err := os.Stat(absDir)
	if err != nil || !st.IsDir() {
		return reg, errors.New("project folder does not exist")
	}
	reg.WorkDir = absDir

	if len(workspaces) > 0 {
		matchedWS := ""
		for _, ws := range workspaces {
			if !ws.ExtensionOn || strings.TrimSpace(ws.Path) == "" {
				continue
			}
			wsAbs, e := filepath.Abs(ws.Path)
			if e != nil {
				continue
			}
			if realWS, e2 := filepath.EvalSymlinks(wsAbs); e2 == nil {
				wsAbs = realWS
			}
			if isWithinDir(absDir, wsAbs) {
				matchedWS = ws.ID
				break
			}
		}
		if matchedWS == "" {
			return reg, errors.New("Compose folders must live inside a workspace where Docker is enabled")
		}
		if reg.WorkspaceID == "" {
			reg.WorkspaceID = matchedWS
		}
	}

	if strings.TrimSpace(reg.Project) == "" {
		reg.Project = sanitizeProjectName(filepath.Base(absDir))
	}
	if !composeNameRe.MatchString(reg.Project) || strings.HasPrefix(reg.Project, "-") {
		return reg, errors.New("project name may use letters, digits, dashes and underscores")
	}

	if len(reg.Files) == 0 {
		for _, candidate := range defaultFiles {
			if fi, e := os.Stat(filepath.Join(absDir, candidate)); e == nil && fi.Mode().IsRegular() {
				reg.Files = []string{candidate}
				break
			}
		}
	}
	if len(reg.Files) == 0 || len(reg.Files) > 8 {
		return reg, errors.New("provide 1 to 8 Compose files inside the project folder")
	}

	cleanFiles := make([]string, 0, len(reg.Files))
	seenFiles := map[string]bool{}
	for _, f := range reg.Files {
		f = strings.TrimSpace(f)
		if f == "" || strings.HasPrefix(f, "-") {
			return reg, errors.New("invalid Compose file name")
		}
		target := f
		if !filepath.IsAbs(target) {
			target = filepath.Join(absDir, target)
		}
		target = filepath.Clean(target)
		if realTarget, e := filepath.EvalSymlinks(target); e == nil {
			target = realTarget
		}
		if !isWithinDir(target, absDir) {
			return reg, errors.New("Compose files must stay inside the project folder")
		}
		fi, e := os.Stat(target)
		if e != nil || !fi.Mode().IsRegular() {
			return reg, fmt.Errorf("Compose file %s does not exist", filepath.Base(f))
		}
		rel, e := filepath.Rel(absDir, target)
		if e != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "-") {
			return reg, errors.New("Compose files must stay inside the project folder")
		}
		relSlash := filepath.ToSlash(rel)
		if !seenFiles[relSlash] {
			seenFiles[relSlash] = true
			cleanFiles = append(cleanFiles, relSlash)
		}
	}
	reg.Files = cleanFiles

	cleanProfiles := make([]string, 0, len(reg.Profiles))
	seenProf := map[string]bool{}
	for _, p := range reg.Profiles {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !composeNameRe.MatchString(p) || strings.HasPrefix(p, "-") {
			return reg, errors.New("invalid Compose profile name")
		}
		if !seenProf[p] {
			seenProf[p] = true
			cleanProfiles = append(cleanProfiles, p)
		}
	}
	reg.Profiles = cleanProfiles
	return reg, nil
}

func isWithinDir(target, root string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func composeBaseArgv(reg ComposeRegistration) []string {
	argv := []string{"docker", "compose", "--project-name", reg.Project, "--project-directory", reg.WorkDir}
	for _, f := range reg.Files {
		argv = append(argv, "--file", filepath.Join(reg.WorkDir, filepath.FromSlash(f)))
	}
	for _, p := range reg.Profiles {
		argv = append(argv, "--profile", p)
	}
	return argv
}

func PreviewCompose(ctx context.Context, runner CommandRunner, c *engine.Client, reg ComposeRegistration, workspaces []WorkspaceInfo) (ComposePreview, error) {
	if runner == nil {
		runner = DefaultCommandRunner
	}
	validReg, err := ValidateComposeRegistration(reg, workspaces)
	if err != nil {
		return ComposePreview{}, err
	}
	out := ComposePreview{
		Project:     validReg.Project,
		WorkDir:     validReg.WorkDir,
		WorkspaceID: validReg.WorkspaceID,
		Files:       validReg.Files,
		Profiles:    validReg.Profiles,
		Services:    []ComposeServiceDiff{},
	}

	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	argv := append(composeBaseArgv(validReg), "config", "--format", "json")
	raw, runErr := runner(cctx, validReg.WorkDir, argv...)
	if runErr != nil {
		msg := strings.TrimSpace(engine.Redact(string(raw), nil))
		if msg == "" {
			msg = runErr.Error()
		}
		if len(msg) > 600 {
			msg = msg[:600] + "…"
		}
		out.Valid = false
		out.Error = msg
		return out, nil
	}

	var parsed struct {
		Services map[string]struct {
			Image string `json:"image"`
			Build any    `json:"build"`
			Ports []struct {
				Target    int    `json:"target"`
				Published string `json:"published"`
				Protocol  string `json:"protocol"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		out.Valid = false
		out.Error = "Docker Compose returned unreadable configuration JSON."
		return out, nil
	}

	liveBySvc := map[string]engine.Container{}
	if c != nil {
		if list, e := c.Containers(ctx); e == nil {
			for _, ct := range list {
				if ct.Project == validReg.Project && ct.Service != "" {
					existing, has := liveBySvc[ct.Service]
					if !has || (existing.State != "running" && ct.State == "running") {
						liveBySvc[ct.Service] = ct
					}
				}
			}
		}
	}

	names := make([]string, 0, len(parsed.Services))
	for name := range parsed.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%s|%s|", validReg.Project, validReg.WorkDir, strings.Join(validReg.Files, ","))
	for _, svcName := range names {
		spec := parsed.Services[svcName]
		img := strings.TrimSpace(spec.Image)
		if img == "" && spec.Build != nil {
			img = validReg.Project + "-" + svcName + " (build)"
		}
		ports := []string{}
		for _, p := range spec.Ports {
			proto := p.Protocol
			if proto == "" {
				proto = "tcp"
			}
			if p.Published != "" {
				ports = append(ports, fmt.Sprintf(":%s->%d/%s", p.Published, p.Target, proto))
			} else if p.Target > 0 {
				ports = append(ports, fmt.Sprintf("%d/%s", p.Target, proto))
			}
		}
		sort.Strings(ports)

		diff := ComposeServiceDiff{Service: svcName, Image: img, Ports: ports, Action: "create"}
		if live, ok := liveBySvc[svcName]; ok {
			diff.RunningImage = live.Image
			diff.State = live.State
			switch {
			case spec.Image != "" && live.Image != spec.Image:
				diff.Action = "recreate"
			case live.State != "running":
				diff.Action = "start"
			default:
				diff.Action = "unchanged"
			}
		}
		_, _ = fmt.Fprintf(h, "%s=%s:%s:%s;", svcName, diff.Image, diff.RunningImage, diff.Action)
		out.Services = append(out.Services, diff)
	}

	out.Valid = true
	out.Fingerprint = hex.EncodeToString(h.Sum(nil))
	return out, nil
}

// DeployCompose applies `docker compose up -d --remove-orphans` using a fixed
// argv slice. It never invokes a shell and never removes volumes (`down -v`).
func DeployCompose(ctx context.Context, runner CommandRunner, reg ComposeRegistration) (string, error) {
	if runner == nil {
		runner = DefaultCommandRunner
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	argv := append(composeBaseArgv(reg), "up", "-d", "--remove-orphans")
	raw, err := runner(cctx, reg.WorkDir, argv...)
	clean := strings.TrimSpace(engine.Redact(string(raw), nil))
	if len(clean) > 800 {
		clean = clean[len(clean)-800:]
	}
	if err != nil {
		if clean == "" {
			clean = err.Error()
		}
		return "", fmt.Errorf("Compose deploy failed: %s", clean)
	}
	if clean == "" {
		clean = "Stack reconciled."
	}
	return clean, nil
}
