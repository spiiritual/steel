package updater

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// apiVersion is the Docker Engine API version the updater asks for, so a
// newer engine keeps answering in the shapes below. An engine outside
// what it supports gets the nearest version it does: Docker 29 refuses
// anything older than 1.44, and Docker before 25 knows nothing newer.
const apiVersion = "1.44"

// docker is a client for the few Docker Engine API calls the updater needs.
type docker struct {
	base string // http://host, with no trailing slash
	http *http.Client

	mu      sync.Mutex
	version string // negotiated API version, once known
}

// newDocker connects to host, in DOCKER_HOST's form: unix:///path or
// http://host:port.
func newDocker(host string) (*docker, error) {
	if path, ok := strings.CutPrefix(host, "unix://"); ok {
		var d net.Dialer
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", path)
		}}
		return &docker{base: "http://docker", http: &http.Client{Transport: tr}}, nil
	}
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return &docker{base: strings.TrimSuffix(host, "/"), http: http.DefaultClient}, nil
	}
	return nil, fmt.Errorf("docker host %q: want unix:///path or http://host:port", host)
}

// errNoSuchObject is the engine's 404: no such container, image or network.
var errNoSuchObject = errors.New("not found")

// do sends a request and decodes a JSON response into out, if given.
func (d *docker) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := d.send(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// send sends a request and returns the response if it succeeded.
func (d *docker) send(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	v, err := d.negotiate(ctx)
	if err != nil {
		return nil, err
	}
	u := d.base + "/v" + v + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	defer resp.Body.Close()
	var e struct{ Message string }
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e) != nil || e.Message == "" {
		e.Message = strings.TrimSpace(string(b))
	}
	err = fmt.Errorf("docker: %s %s: %s", method, path, e.Message)
	if resp.StatusCode == http.StatusNotFound {
		err = fmt.Errorf("%w (%w)", err, errNoSuchObject)
	}
	return nil, err
}

// negotiate picks the API version: apiVersion, moved into the range the
// engine supports.
func (d *docker) negotiate(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.version != "" {
		return d.version, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("docker: %w", err)
	}
	defer resp.Body.Close()
	var v struct{ APIVersion, MinAPIVersion string }
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&v) != nil || v.APIVersion == "" {
		return "", fmt.Errorf("docker: GET /version: %s", resp.Status)
	}
	d.version = apiVersion
	if versionLess(v.APIVersion, d.version) {
		d.version = v.APIVersion
	}
	if v.MinAPIVersion != "" && versionLess(d.version, v.MinAPIVersion) {
		d.version = v.MinAPIVersion
	}
	return d.version, nil
}

// versionLess compares API versions like "1.44".
func versionLess(a, b string) bool {
	var amaj, amin, bmaj, bmin int
	fmt.Sscanf(a, "%d.%d", &amaj, &amin)
	fmt.Sscanf(b, "%d.%d", &bmaj, &bmin)
	return amaj < bmaj || amaj == bmaj && amin < bmin
}

// container is what the updater reads from a container's inspection.
// Config and HostConfig stay raw: they are copied into the replacement, so
// fields this code doesn't know about survive.
type container struct {
	ID           string `json:"Id"`
	Name         string // with a leading "/"
	Image        string // the image's ID
	RestartCount int
	State        struct {
		Running  bool
		ExitCode int
		Error    string
		Health   *struct{ Status string }
	}
	Config          json.RawMessage
	HostConfig      json.RawMessage
	NetworkSettings struct {
		Networks map[string]endpoint
	}
}

// endpoint is a container's attachment to a network: the parts a
// replacement needs to be reachable the same way.
type endpoint struct {
	Aliases    []string        `json:",omitempty"`
	Links      []string        `json:",omitempty"`
	IPAMConfig json.RawMessage `json:",omitempty"`
	DriverOpts json.RawMessage `json:",omitempty"`
}

func (c container) name() string { return strings.TrimPrefix(c.Name, "/") }

// config decodes the container's Config.
func (c container) config() (map[string]any, error) {
	var m map[string]any
	err := json.Unmarshal(c.Config, &m)
	return m, err
}

func (d *docker) inspectContainer(ctx context.Context, id string) (container, error) {
	var c container
	err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &c)
	return c, err
}

// listContainers returns the IDs of containers, running or not, that
// carry every label in labels ("key=value").
func (d *docker) listContainers(ctx context.Context, labels ...string) ([]string, error) {
	f, err := json.Marshal(map[string][]string{"label": labels})
	if err != nil {
		return nil, err
	}
	var out []struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"true"}, "filters": {string(f)}}, nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, len(out))
	for i, c := range out {
		ids[i] = c.ID
	}
	return ids, nil
}

// image is what the updater reads from an image's inspection.
type image struct {
	ID       string `json:"Id"`
	RepoTags []string
	Config   json.RawMessage
}

func (i image) config() (map[string]any, error) {
	m := map[string]any{}
	if len(i.Config) == 0 || string(i.Config) == "null" {
		return m, nil
	}
	err := json.Unmarshal(i.Config, &m)
	return m, err
}

func (d *docker) inspectImage(ctx context.Context, ref string) (image, error) {
	var i image
	err := d.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, &i)
	return i, err
}

// pull fetches repo@digest. The engine streams progress as JSON lines and
// reports failure in them, even after a 200.
func (d *docker) pull(ctx context.Context, repo, digest string) error {
	resp, err := d.send(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {repo}, "tag": {digest}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(bufio.NewReader(resp.Body))
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("docker: pulling %s@%s: %w", repo, digest, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("docker: pulling %s@%s: %s", repo, digest, msg.Error)
		}
	}
}

// tag points repo:tag at the image ref names.
func (d *docker) tag(ctx context.Context, ref, repo, tag string) error {
	return d.do(ctx, http.MethodPost, "/images/"+ref+"/tag", url.Values{"repo": {repo}, "tag": {tag}}, nil, nil)
}

// stop stops a container, giving it seconds to exit after SIGTERM.
func (d *docker) stop(ctx context.Context, id string, seconds int) error {
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", url.Values{"t": {fmt.Sprint(seconds)}}, nil, nil)
}

func (d *docker) start(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

func (d *docker) rename(ctx context.Context, id, name string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/rename", url.Values{"name": {name}}, nil, nil)
}

// remove deletes a container, stopping it first if needed. Its volumes stay.
func (d *docker) remove(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"true"}}, nil, nil)
}

// create makes a container named name and returns its ID.
func (d *docker) create(ctx context.Context, name string, body createBody) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := d.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &out)
	return out.ID, err
}

// createBody is a container's Config with HostConfig and the network it
// starts on; engines before API 1.44 take one network at create, so the
// rest are connected after.
type createBody struct {
	config           map[string]any
	HostConfig       json.RawMessage
	NetworkingConfig *struct {
		EndpointsConfig map[string]endpoint
	}
}

// MarshalJSON flattens config into the top level, where the API wants it.
func (b createBody) MarshalJSON() ([]byte, error) {
	m := maps.Clone(b.config)
	if m == nil {
		m = map[string]any{}
	}
	m["HostConfig"] = b.HostConfig
	if b.NetworkingConfig != nil {
		m["NetworkingConfig"] = b.NetworkingConfig
	}
	return json.Marshal(m)
}

func (d *docker) connect(ctx context.Context, network, id string, e endpoint) error {
	body := map[string]any{"Container": id, "EndpointConfig": e}
	return d.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil, body, nil)
}
