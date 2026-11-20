package updater

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeDocker is an in-memory Docker Engine for the calls the updater
// makes. A container whose image has the label "test.crash" exits as soon
// as it starts.
type fakeDocker struct {
	t      *testing.T
	server *httptest.Server

	mu         sync.Mutex
	images     map[string]*fakeImage     // ID → image
	tags       map[string]string         // "repo:tag" → image ID
	registry   map[string]*fakeImage     // "repo@digest" → image, pullable
	containers map[string]*fakeContainer // ID → container
	nextID     int
	stops      []int // timeouts stop was called with
}

type fakeImage struct {
	ID     string
	Config map[string]any
}

type fakeContainer struct {
	ID           string
	Name         string
	Image        string // image ID
	Config       map[string]any
	HostConfig   json.RawMessage
	Networks     map[string]endpoint
	Running      bool
	ExitCode     int
	RestartCount int
}

func newFakeDocker(t *testing.T) *fakeDocker {
	f := &fakeDocker{t: t, images: map[string]*fakeImage{}, tags: map[string]string{}, registry: map[string]*fakeImage{}, containers: map[string]*fakeContainer{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeDocker) id() string {
	f.nextID++
	return fmt.Sprintf("%064x", f.nextID)
}

// addImage makes an image with steel version v, tagged with tags.
func (f *fakeDocker) addImage(v string, env []any, extraLabels map[string]any, tags ...string) *fakeImage {
	f.mu.Lock()
	defer f.mu.Unlock()
	labels := map[string]any{VersionLabel: v}
	maps.Copy(labels, extraLabels)
	img := &fakeImage{ID: "sha256:" + f.id(), Config: map[string]any{
		"Env": env, "Labels": labels, "Entrypoint": []any{"tini", "--", "steel-entrypoint"}, "User": "steel",
	}}
	f.images[img.ID] = img
	for _, t := range tags {
		f.tags[t] = img.ID
	}
	return img
}

// publish makes img pullable as repo@digest, removing it locally.
func (f *fakeDocker) publish(img *fakeImage, repo, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, img.ID)
	f.registry[repo+"@"+digest] = img
}

func (f *fakeDocker) addContainer(c *fakeContainer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[c.ID] = c
}

// find resolves a container by ID, ID prefix or name.
func (f *fakeDocker) find(ref string) *fakeContainer {
	for _, c := range f.containers {
		if c.Name == ref || strings.HasPrefix(c.ID, ref) {
			return c
		}
	}
	return nil
}

// byName returns the container named name, or nil.
func (f *fakeDocker) byName(name string) *fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.Name == name {
			cc := *c
			return &cc
		}
	}
	return nil
}

// tagged returns the image ID ref points to.
func (f *fakeDocker) tagged(ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tags[ref]
}

func (f *fakeDocker) image(ref string) *fakeImage {
	if id, ok := f.tags[ref]; ok {
		ref = id
	}
	if img, ok := f.images[ref]; ok {
		return img
	}
	for name, img := range f.registry {
		if name == ref && f.images[img.ID] != nil {
			return img
		}
	}
	return nil
}

func (f *fakeDocker) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/version" {
		json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.54", "MinAPIVersion": "1.40"})
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v"+apiVersion)
	if !ok {
		http.Error(w, `{"message":"unversioned"}`, http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	notFound := func(what string) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "No such " + what})
	}
	reply := func(status int, v any) {
		w.WriteHeader(status)
		if v != nil {
			json.NewEncoder(w).Encode(v)
		}
	}

	switch {
	case r.Method == http.MethodGet && path == "/containers/json":
		var filters struct{ Label []string }
		json.Unmarshal([]byte(q.Get("filters")), &filters)
		var out []map[string]string
		for _, c := range f.containers {
			labels, _ := c.Config["Labels"].(map[string]any)
			if !slices.ContainsFunc(filters.Label, func(l string) bool {
				k, v, _ := strings.Cut(l, "=")
				return labels[k] != v
			}) {
				out = append(out, map[string]string{"Id": c.ID})
			}
		}
		reply(http.StatusOK, out)

	case r.Method == http.MethodPost && path == "/containers/create":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		name := q.Get("name")
		if f.find(name) != nil && f.find(name).Name == name {
			reply(http.StatusConflict, map[string]string{"message": "name in use"})
			return
		}
		ref, _ := body["Image"].(string)
		img := f.image(ref)
		if img == nil {
			notFound("image " + ref)
			return
		}
		host, _ := json.Marshal(body["HostConfig"])
		var nc struct{ EndpointsConfig map[string]endpoint }
		b, _ := json.Marshal(body["NetworkingConfig"])
		json.Unmarshal(b, &nc)
		if len(nc.EndpointsConfig) > 1 {
			reply(http.StatusBadRequest, map[string]string{"message": "one network at create"})
			return
		}
		delete(body, "HostConfig")
		delete(body, "NetworkingConfig")
		c := &fakeContainer{ID: f.id(), Name: name, Image: img.ID, Config: body, HostConfig: host, Networks: nc.EndpointsConfig}
		if c.Networks == nil {
			c.Networks = map[string]endpoint{}
		}
		f.containers[c.ID] = c
		reply(http.StatusCreated, map[string]string{"Id": c.ID})

	case strings.HasPrefix(path, "/containers/"):
		rest := strings.TrimPrefix(path, "/containers/")
		ref, op, _ := strings.Cut(rest, "/")
		c := f.find(ref)
		if c == nil {
			notFound("container " + ref)
			return
		}
		switch {
		case r.Method == http.MethodGet && op == "json":
			cfg, _ := json.Marshal(c.Config)
			reply(http.StatusOK, map[string]any{
				"Id": c.ID, "Name": "/" + c.Name, "Image": c.Image, "RestartCount": c.RestartCount,
				"State":           map[string]any{"Running": c.Running, "ExitCode": c.ExitCode},
				"Config":          json.RawMessage(cfg),
				"HostConfig":      c.HostConfig,
				"NetworkSettings": map[string]any{"Networks": c.Networks},
			})
		case r.Method == http.MethodPost && op == "stop":
			var t int
			fmt.Sscan(q.Get("t"), &t)
			f.stops = append(f.stops, t)
			c.Running = false
			reply(http.StatusNoContent, nil)
		case r.Method == http.MethodPost && op == "start":
			if c.Running {
				reply(http.StatusNotModified, nil)
				return
			}
			img := f.images[c.Image]
			labels, _ := img.Config["Labels"].(map[string]any)
			if labels["test.crash"] != nil {
				c.Running, c.ExitCode = false, 1
			} else {
				c.Running, c.ExitCode = true, 0
			}
			reply(http.StatusNoContent, nil)
		case r.Method == http.MethodPost && op == "rename":
			name := q.Get("name")
			if name == c.Name {
				reply(http.StatusConflict, map[string]string{"message": "same name"})
				return
			}
			if o := f.find(name); o != nil && o.Name == name {
				reply(http.StatusConflict, map[string]string{"message": "name in use"})
				return
			}
			c.Name = name
			reply(http.StatusNoContent, nil)
		case r.Method == http.MethodDelete && op == "":
			delete(f.containers, c.ID)
			reply(http.StatusNoContent, nil)
		default:
			http.NotFound(w, r)
		}

	case r.Method == http.MethodPost && path == "/images/create":
		ref := q.Get("fromImage") + "@" + q.Get("tag")
		img, ok := f.registry[ref]
		if !ok {
			reply(http.StatusOK, map[string]string{"error": "manifest unknown"}) // as the engine does
			return
		}
		f.images[img.ID] = img
		reply(http.StatusOK, map[string]string{"status": "Downloaded newer image"})

	case strings.HasPrefix(path, "/images/"):
		rest := strings.TrimPrefix(path, "/images/")
		if ref, ok := strings.CutSuffix(rest, "/json"); ok && r.Method == http.MethodGet {
			img := f.image(ref)
			if img == nil {
				notFound("image " + ref)
				return
			}
			var tags []string
			for t, id := range f.tags {
				if id == img.ID {
					tags = append(tags, t)
				}
			}
			reply(http.StatusOK, map[string]any{"Id": img.ID, "RepoTags": tags, "Config": img.Config})
			return
		}
		if ref, ok := strings.CutSuffix(rest, "/tag"); ok && r.Method == http.MethodPost {
			img := f.image(ref)
			if img == nil {
				notFound("image " + ref)
				return
			}
			f.tags[q.Get("repo")+":"+q.Get("tag")] = img.ID
			reply(http.StatusCreated, nil)
			return
		}
		http.NotFound(w, r)

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/networks/"):
		net := strings.TrimSuffix(strings.TrimPrefix(path, "/networks/"), "/connect")
		var body struct {
			Container      string
			EndpointConfig endpoint
		}
		json.NewDecoder(r.Body).Decode(&body)
		c := f.find(body.Container)
		if c == nil {
			notFound("container")
			return
		}
		c.Networks[net] = body.EndpointConfig
		reply(http.StatusOK, nil)

	default:
		f.t.Errorf("fake docker: unexpected %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}
