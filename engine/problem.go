package engine

import (
	"context"
	"fmt"
	"time"
)

// Problem is what the panel and an agent's tool say when Docker cannot be
// used: the case, a title, one sentence and one action. COGNIX[WS]'s
// Software row says the same per case (web/shared/domain/software.js,
// engineProblem).
type Problem struct {
	State    State  `json:"state"`
	Title    string `json:"title"`
	Sentence string `json:"sentence"`
	Action   Action `json:"action"`
	Endpoint string `json:"endpoint,omitempty"`
	Distro   string `json:"distro,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Action is the one thing the panel offers: copy a command, or check again
// after a step the person takes elsewhere.
type Action struct {
	Kind    string `json:"kind"` // copy | recheck | link
	Label   string `json:"label"`
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
}

// InstallURL is Docker's own install guide.
const InstallURL = "https://docs.docker.com/engine/install/"

// NotReadyError is Connect's answer when Diagnose found no usable engine.
type NotReadyError struct{ Diagnosis Diagnosis }

func (e *NotReadyError) Error() string {
	p := ProblemOf(e.Diagnosis)
	return p.Title + ": " + p.Sentence
}

// ProblemOf words a diagnosis that is not ready.
func ProblemOf(d Diagnosis) Problem {
	recheck := Action{Kind: "recheck", Label: "Check again"}
	copyCmd := func(c string) Action { return Action{Kind: "copy", Label: "Copy command", Command: c} }
	p := Problem{State: d.State, Endpoint: d.Endpoint, Distro: d.Distro, Detail: d.Detail, Action: recheck}
	chosen := d.Source != "" && d.Source != SourceDefault
	switch d.State {
	case StateStopped:
		cmd := "sudo service docker start"
		if d.Rootless {
			// Used only through its context: start it and switch.
			cmd = "systemctl --user start docker && docker context use rootless"
		} else if d.Systemd {
			cmd = "sudo systemctl start docker"
		}
		p.Title = "Docker is installed but not running"
		p.Sentence = "Start it with this command in a COGNIX[WS] terminal, then check again."
		p.Action = copyCmd(cmd)
	case StateDesktop:
		p.Title = "Docker Desktop is installed but not connected to COGNIX[WS]"
		if d.Distro != "" {
			p.Sentence = fmt.Sprintf("Start Docker Desktop and turn on Settings › Resources › WSL integration › %s, then check again.", d.Distro)
		} else {
			p.Sentence = "Start Docker Desktop and turn on its WSL integration for COGNIX[WS]'s Linux, then check again."
		}
	case StateDenied:
		p.Title = "No permission to use Docker"
		if chosen {
			p.Sentence = fmt.Sprintf("The connection Docker is set to use, %s, doesn't let this account in. Give this account access to that socket, then check again.", d.Endpoint)
		} else {
			p.Sentence = "Docker is running, but this account isn't allowed to use it. In the COGNIX[WS] app, open Software › Docker Engine and choose Give access, then check again."
		}
	case StateRemote:
		p.Title = "Docker points to another computer"
		if d.Source == SourceContext {
			p.Sentence = fmt.Sprintf("Docker is set to reach %s, and this extension only manages Docker on this computer. Switch back with this command.", d.Endpoint)
			p.Action = copyCmd("docker context use default")
		} else {
			p.Sentence = fmt.Sprintf("DOCKER_HOST sends Docker to %s, and this extension only manages Docker on this computer. Remove the DOCKER_HOST setting, then check again.", d.Endpoint)
		}
	case StateNoEngine:
		p.Title = "Docker's engine isn't installed"
		p.Sentence = "Only the docker command is here, not the engine that runs containers. Docker's install guide shows how to add it."
		p.Action = Action{Kind: "link", Label: "How to install", URL: InstallURL}
	case StateAbsent:
		p.Title = "Docker isn't installed"
		p.Sentence = "Install Docker Engine from COGNIX[WS]'s Software page, or Docker Desktop on Windows, then check again."
	case StateIncompatible:
		p.Title = "This Docker is too old"
		p.Sentence = "This extension needs Docker 25 or later. Update Docker, then check again."
	default:
		p.State = StateSilent
		p.Title = "Docker doesn't answer"
		if chosen {
			p.Sentence = fmt.Sprintf("The connection Docker is set to use, %s, doesn't answer. Start that engine, then check again.", d.Endpoint)
		} else {
			p.Sentence = "Docker's connection is there but doesn't answer. Restart Docker, then check again."
		}
	}
	return p
}

var connectCache = &Cache{TTL: 2 * time.Second}

// Connect reaches the first engine Diagnose finds; otherwise a
// *NotReadyError says which case it is.
func Connect(ctx context.Context) (*Client, error) {
	// Kept two seconds: the panel reads several routes at once, and the
	// engine watcher retries every few seconds while Docker is down.
	d := connectCache.Get(ctx, "", func(ctx context.Context) Diagnosis { return Diagnose(ctx, LocalSystem()) })
	if d.State != StateReady {
		return nil, &NotReadyError{Diagnosis: d}
	}
	return NewClient(d.Endpoint)
}
