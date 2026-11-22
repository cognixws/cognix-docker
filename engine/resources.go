package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

type Consumer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Project string `json:"project,omitempty"`
	State   string `json:"state"`
}

type Resource struct {
	Kind           string     `json:"kind"` // image | volume | network
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Project        string     `json:"project,omitempty"`
	Tags           []string   `json:"tags,omitempty"`
	Driver         string     `json:"driver,omitempty"`
	Scope          string     `json:"scope,omitempty"`
	SizeBytes      *int64     `json:"sizeBytes,omitempty"`
	ReclaimableEst *int64     `json:"reclaimableBytes,omitempty"`
	Consumers      []Consumer `json:"consumers"`
	Removable      bool       `json:"removable"`
	Reason         string     `json:"reason,omitempty"`
}

type Resources struct {
	Endpoint  string     `json:"endpoint"`
	SampledAt string     `json:"sampledAt"`
	Items     []Resource `json:"items"`
}

func (c *Client) Resources(ctx context.Context) (Resources, error) {
	out := Resources{Endpoint: c.Endpoint, SampledAt: time.Now().UTC().Format(time.RFC3339), Items: []Resource{}}
	containers, err := c.Containers(ctx)
	if err != nil {
		return out, err
	}
	du, dfErr := c.diskUsage(ctx)
	volSizes := map[string]int64{}
	imgOwn := map[string]int64{}
	if dfErr == nil {
		for _, v := range du.Volumes {
			if v.UsageData.Size >= 0 {
				volSizes[v.Name] = v.UsageData.Size
			}
		}
		for _, im := range du.Images {
			imgOwn[im.ID] = ownSize(im.Size, im.SharedSize)
		}
	}

	var images []struct {
		ID       string `json:"Id"`
		RepoTags []string
		Size     *int64
	}
	if err = c.get(ctx, "/v"+apiVersion+"/images/json?all=true", &images); err != nil {
		return out, err
	}
	var volumes struct {
		Volumes []struct {
			Name, Driver, Scope string
			Labels              map[string]string
		}
	}
	if err = c.get(ctx, "/v"+apiVersion+"/volumes", &volumes); err != nil {
		return out, err
	}
	var networks []struct {
		ID                  string `json:"Id"`
		Name, Driver, Scope string
		Ingress             bool
		Labels              map[string]string
	}
	if err = c.get(ctx, "/v"+apiVersion+"/networks", &networks); err != nil {
		return out, err
	}

	for _, im := range images {
		name := shortID(im.ID)
		tags := []string{}
		for _, tag := range im.RepoTags {
			if tag != "<none>:<none>" {
				tags = append(tags, tag)
			}
		}
		sort.Strings(tags)
		if len(tags) > 0 {
			name = tags[0]
		}
		if im.Size != nil && *im.Size < 0 {
			im.Size = nil
		}
		var ownPtr *int64
		if own, ok := imgOwn[im.ID]; ok {
			v := own
			ownPtr = &v
		}
		r := Resource{Kind: "image", ID: im.ID, Name: name, Tags: tags, SizeBytes: im.Size, ReclaimableEst: ownPtr, Removable: imageID.MatchString(im.ID)}
		out.Items = append(out.Items, r)
	}

	for _, v := range volumes.Volumes {
		proj := v.Labels["com.docker.compose.project"]
		if proj == "" {
			proj = v.Labels["com.supabase.cli.project"]
		}
		var szPtr *int64
		if sz, ok := volSizes[v.Name]; ok {
			val := sz
			szPtr = &val
		}
		rem := volumeName.MatchString(v.Name) && v.Scope != "global"
		r := Resource{Kind: "volume", ID: v.Name, Name: v.Name, Project: proj, Driver: v.Driver, Scope: v.Scope, SizeBytes: szPtr, ReclaimableEst: szPtr, Removable: rem}
		if !rem {
			r.Reason = "Non-local or invalid volume name is protected."
		}
		out.Items = append(out.Items, r)
	}

	for _, n := range networks {
		proj := n.Labels["com.docker.compose.project"]
		removable := n.Name != "bridge" && n.Name != "host" && n.Name != "none" && !n.Ingress && n.Scope == "local" && fullID.MatchString(n.ID)
		r := Resource{Kind: "network", ID: n.ID, Name: n.Name, Project: proj, Driver: n.Driver, Scope: n.Scope, Removable: removable}
		if !removable {
			r.Reason = "Built-in, ingress and non-local networks are protected."
		}
		out.Items = append(out.Items, r)
	}

	for i := range out.Items {
		r := &out.Items[i]
		r.Consumers = []Consumer{}
		for _, ct := range containers {
			used := r.Kind == "image" && ct.ImageID == r.ID
			for _, m := range ct.Mounts {
				used = used || (r.Kind == "volume" && m.Type == "volume" && m.Name == r.ID)
			}
			for _, id := range ct.Networks {
				used = used || (r.Kind == "network" && id == r.ID)
			}
			if used {
				r.Consumers = append(r.Consumers, Consumer{ID: ct.ID, Name: ct.Name, Project: ct.Project, State: ct.State})
			}
		}
		sort.Slice(r.Consumers, func(i, j int) bool { return r.Consumers[i].ID < r.Consumers[j].ID })
		if len(r.Consumers) > 0 {
			r.Removable = false
			r.Reason = "Referenced by containers, including stopped consumers."
		}
		if !r.Removable && r.Reason == "" {
			r.Reason = "Docker did not report a removable resource ID."
		}
	}

	sort.Slice(out.Items, func(i, j int) bool {
		if out.Items[i].Kind != out.Items[j].Kind {
			return out.Items[i].Kind < out.Items[j].Kind
		}
		if out.Items[i].Name != out.Items[j].Name {
			return out.Items[i].Name < out.Items[j].Name
		}
		return out.Items[i].ID < out.Items[j].ID
	})
	return out, nil
}

func FindResource(inv Resources, kind, id string) (Resource, error) {
	for _, r := range inv.Items {
		if r.Kind == kind && r.ID == id {
			return r, nil
		}
	}
	return Resource{}, errors.New("Resource is no longer present. Refresh the inventory")
}

func ResourcePath(kind, id string) (string, error) {
	switch {
	case kind == "image" && imageID.MatchString(id):
		return "/v" + apiVersion + "/images/" + url.PathEscape(id), nil
	case kind == "network" && fullID.MatchString(id):
		return "/v" + apiVersion + "/networks/" + id, nil
	case kind == "volume" && volumeName.MatchString(id):
		return "/v" + apiVersion + "/volumes/" + url.PathEscape(id), nil
	default:
		return "", errors.New("Select an image, volume or custom local network using its exact ID")
	}
}

func (c *Client) RemoveResource(ctx context.Context, kind, id string) error {
	basePath, err := ResourcePath(kind, id)
	if err != nil {
		return err
	}
	delPath := basePath
	if kind == "image" {
		delPath += "?force=false&noprune=true"
	}
	res, err := c.request(ctx, http.MethodDelete, delPath)
	if err != nil {
		return err
	}
	_ = res.Body.Close()

	checkPath := basePath
	if kind == "image" {
		checkPath += "/json"
	}
	res, err = c.request(ctx, http.MethodGet, checkPath)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	if err == nil {
		_ = res.Body.Close()
	}
	return fmt.Errorf("Docker accepted removal, but absence could not be verified. Refresh the inventory")
}
