// Package updater is steel's update sidecar: a small service beside a
// containerized steel that replaces steel's container with a newer release
// image, or the previous one, when asked over HTTP.
//
// steel itself can't do this: it would need the Docker socket, which is
// root on the host, and the agent's shell runs as steel. So the power
// lives here, in a process no model drives, and its API takes no
// arguments: anything that can reach it (the agent included) can only
// move steel to the newest signed release, or back to the one before.
//
// An update pulls the image by the digest in the release's signed
// checksums, so it's covered by update.ReleaseKey like a binary install.
// steel's container is stopped (SIGTERM: steel finishes running work),
// recreated from the new image with the same settings, and watched; if it
// doesn't stay up, the old container comes back.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"

	"github.com/spiiritual/steel/internal/update"
)

// Releases finds the newest release.
type Releases interface {
	// Latest returns the newest release's version and the digest of its
	// image, both verified against the release key.
	Latest(ctx context.Context) (version, digest string, err error)
}

// SignedReleases is Releases from steel's signed GitHub releases.
type SignedReleases struct{ *update.Updater }

func (s SignedReleases) Latest(ctx context.Context) (string, string, error) {
	r, err := s.Updater.Latest(ctx)
	if err != nil {
		return "", "", err
	}
	d, err := s.ImageDigest(ctx, r)
	return r.Version, d, err
}

// Options configure an Updater.
type Options struct {
	// Docker is the engine's address, as in DOCKER_HOST
	// (default unix:///var/run/docker.sock).
	Docker string
	// Image is the repository releases are pulled from
	// (default ghcr.io/spiiritual/steel).
	Image string
	// Container is steel's container. Empty means the compose service
	// Service in the updater's own compose project.
	Container string
	Service   string // default "steel"
	// Self is the updater's own container, found by its hostname when
	// empty: compose names the project on it.
	Self string
	// Health is how long a new container must stay up to count as
	// working (default one minute).
	Health   time.Duration
	Releases Releases
	Logf     func(format string, args ...any)
}

// VersionLabel is the image label holding its steel version.
const VersionLabel = "org.opencontainers.image.version"

// composeImageLabel holds the image ID compose created a container from;
// `docker compose up` recreates a container whose label disagrees with
// what its image tag points to now.
const composeImageLabel = "com.docker.compose.image"

// replacedSuffix names a container being replaced, kept until its
// replacement proves itself.
const replacedSuffix = "-replaced"

// Status is what the updater reports.
type Status struct {
	Current string `json:"current"` // steel's running version
	// Latest is the newest release, or LatestError why it's unknown.
	Latest      string `json:"latest,omitempty"`
	LatestError string `json:"latestError,omitempty"`
	// Previous is the version a rollback restores; empty if there's none.
	Previous string  `json:"previous,omitempty"`
	Busy     bool    `json:"busy"`
	Last     *Result `json:"last,omitempty"` // the last update or rollback since the updater started
}

// Result is one update or rollback.
type Result struct {
	Action   string    `json:"action"` // "update" or "rollback"
	From     string    `json:"from"`
	To       string    `json:"to"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	// Error is why it failed; empty means steel now runs To.
	Error string `json:"error,omitempty"`
	// Restored means To failed to start and steel was put back on From.
	Restored bool `json:"restored,omitempty"`
}

// Errors Start returns before doing anything.
var (
	ErrBusy     = errors.New("an update or rollback is already running")
	ErrUpToDate = errors.New("steel is already up to date")
)

// Updater replaces steel's container. Its methods are safe for concurrent
// use; one update or rollback runs at a time.
type Updater struct {
	d        *docker
	repo     string
	opts     Options
	poll     time.Duration // how often the health watch looks; tests shorten it
	mu       sync.Mutex
	busy     bool
	last     *Result
	finished chan struct{} // closed when the running action ends; tests wait on it
}

// New returns an Updater.
func New(o Options) (*Updater, error) {
	if o.Docker == "" {
		o.Docker = "unix:///var/run/docker.sock"
	}
	if o.Image == "" {
		o.Image = "ghcr.io/spiiritual/steel"
	}
	if o.Service == "" {
		o.Service = "steel"
	}
	if o.Health <= 0 {
		o.Health = time.Minute
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Releases == nil {
		return nil, errors.New("updater: no Releases")
	}
	d, err := newDocker(o.Docker)
	if err != nil {
		return nil, err
	}
	return &Updater{d: d, repo: o.Image, opts: o, poll: 2 * time.Second}, nil
}

// Status reports versions and the last result. It asks GitHub for the
// newest release, so it takes a moment.
func (u *Updater) Status(ctx context.Context) (Status, error) {
	u.mu.Lock()
	s := Status{Busy: u.busy, Last: u.last}
	u.mu.Unlock()
	if s.Last != nil {
		last := *s.Last
		s.Last = &last
	}
	c, err := u.target(ctx)
	if err != nil {
		return s, err
	}
	if s.Current, err = u.versionOf(ctx, c.Image); err != nil {
		return s, err
	}
	if s.Previous, err = u.versionOf(ctx, u.previousRef()); errors.Is(err, errNoSuchObject) {
		s.Previous = ""
	} else if err != nil {
		return s, err
	}
	if v, _, err := u.opts.Releases.Latest(ctx); err != nil {
		s.LatestError = err.Error()
	} else {
		s.Latest = v
	}
	return s, nil
}

// Start begins an update to the newest release or, for "rollback", a
// return to the previous version, and returns at once with what it's
// doing. The work, which stops and replaces steel's container, finishes in
// the background; Status reports how it went.
func (u *Updater) Start(ctx context.Context, action string) (Result, error) {
	if action != "update" && action != "rollback" {
		return Result{}, fmt.Errorf("unknown action %q", action)
	}
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return Result{}, ErrBusy
	}
	u.busy = true
	u.mu.Unlock()
	started := false
	defer func() {
		if !started {
			u.mu.Lock()
			u.busy = false
			u.mu.Unlock()
		}
	}()

	c, err := u.target(ctx)
	if err != nil {
		return Result{}, err
	}
	if _, err := u.refFor(c); err != nil {
		return Result{}, err
	}
	r := Result{Action: action, Started: time.Now()}
	if r.From, err = u.versionOf(ctx, c.Image); err != nil {
		return Result{}, err
	}
	var work func(context.Context) (bool, error)
	switch action {
	case "update":
		version, digest, err := u.opts.Releases.Latest(ctx)
		if err != nil {
			return Result{}, err
		}
		r.To = version
		if !update.Newer(r.From, version) {
			return r, ErrUpToDate
		}
		work = func(ctx context.Context) (bool, error) {
			if err := u.d.pull(ctx, u.repo, digest); err != nil {
				return false, err
			}
			img, err := u.d.inspectImage(ctx, u.repo+"@"+digest)
			if err != nil {
				return false, err
			}
			// Named for its version, so `docker images` says what it is.
			if err := u.d.tag(ctx, img.ID, u.repo, version); err != nil {
				return false, err
			}
			return u.swap(ctx, img.ID)
		}
	case "rollback":
		prev, err := u.d.inspectImage(ctx, u.previousRef())
		if errors.Is(err, errNoSuchObject) {
			return Result{}, errors.New("there's no previous version to roll back to")
		}
		if err != nil {
			return Result{}, err
		}
		if r.To, err = u.versionOf(ctx, prev.ID); err != nil {
			return Result{}, err
		}
		work = func(ctx context.Context) (bool, error) { return u.swap(ctx, prev.ID) }
	}

	started = true
	done := make(chan struct{})
	u.mu.Lock()
	u.last = &r
	u.finished = done
	u.mu.Unlock()
	u.opts.Logf("%s: %s → %s", action, r.From, r.To)
	go func() {
		defer close(done)
		// Not the request's context: the work outlives the request.
		restored, err := work(context.Background())
		res := r
		res.Finished = time.Now()
		res.Restored = restored
		if err != nil {
			res.Error = err.Error()
			u.opts.Logf("%s to %s failed: %v", action, r.To, err)
		} else {
			u.opts.Logf("%s: steel runs %s", action, r.To)
		}
		u.mu.Lock()
		u.last = &res
		u.busy = false
		u.mu.Unlock()
	}()
	return r, nil
}

// previousTag is the local tag kept on the image a rollback returns to.
const previousTag = "previous"

func (u *Updater) previousRef() string { return u.repo + ":" + previousTag }

// refFor is the image reference steel's container is created from, which
// the updater moves to each new image. It must be the release repository
// at :latest (or untagged): a container pinned to a version would be put
// back on it by the next `docker compose up`.
func (u *Updater) refFor(c container) (string, error) {
	cfg, err := c.config()
	if err != nil {
		return "", err
	}
	ref, _ := cfg["Image"].(string)
	repo, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	switch {
	case repo != u.repo || strings.Contains(ref, "@"):
		return "", fmt.Errorf("steel's container runs %q, not a release image from %s; set image: %s:latest in compose.yaml to update it this way", ref, u.repo, u.repo)
	case tag != "latest":
		return "", fmt.Errorf("compose.yaml pins steel to %q, and the next `docker compose up` would go back to it; set image: %s:latest to update this way", ref, u.repo)
	}
	return repo + ":latest", nil
}

// versionOf reads steel's version from an image: its version label, else a
// version tag on it.
func (u *Updater) versionOf(ctx context.Context, ref string) (string, error) {
	img, err := u.d.inspectImage(ctx, ref)
	if err != nil {
		return "", err
	}
	cfg, err := img.config()
	if err != nil {
		return "", err
	}
	if labels, ok := cfg["Labels"].(map[string]any); ok {
		if v, ok := labels[VersionLabel].(string); ok && semver.IsValid(v) {
			return v, nil
		}
	}
	for _, t := range img.RepoTags {
		if i := strings.LastIndex(t, ":"); i >= 0 && t[:i] == u.repo && semver.IsValid(t[i+1:]) {
			return t[i+1:], nil
		}
	}
	return "unknown", nil
}

// target finds steel's container.
func (u *Updater) target(ctx context.Context) (container, error) {
	if u.opts.Container != "" {
		return u.d.inspectContainer(ctx, u.opts.Container)
	}
	self := u.opts.Self
	if self == "" {
		self, _ = os.Hostname()
	}
	me, err := u.d.inspectContainer(ctx, self)
	if err != nil {
		return container{}, fmt.Errorf("finding the updater's own container (%s) to learn its compose project: %w; pass -container", self, err)
	}
	var labels struct{ Labels map[string]string }
	if err := json.Unmarshal(me.Config, &labels); err != nil {
		return container{}, err
	}
	project := labels.Labels["com.docker.compose.project"]
	if project == "" {
		return container{}, errors.New("the updater isn't running under docker compose; pass -container")
	}
	ids, err := u.d.listContainers(ctx, "com.docker.compose.project="+project, "com.docker.compose.service="+u.opts.Service)
	if err != nil {
		return container{}, err
	}
	var found []container
	for _, id := range ids {
		c, err := u.d.inspectContainer(ctx, id)
		if err != nil {
			return container{}, err
		}
		if orig, ok := strings.CutSuffix(c.name(), replacedSuffix); ok {
			return container{}, fmt.Errorf("container %s is left over from an interrupted update: if %s is missing, rename it back to that; else remove it", c.name(), orig)
		}
		found = append(found, c)
	}
	if len(found) != 1 {
		return container{}, fmt.Errorf("compose project %s has %d %q containers; the updater handles exactly one", project, len(found), u.opts.Service)
	}
	return found[0], nil
}

// swap replaces steel's container with one from image newID, keeping
// everything else. If the new one doesn't stay up for the health window,
// the old one is put back and restored is true. On success the old image
// is kept as previousRef, for rollback.
func (u *Updater) swap(ctx context.Context, newID string) (restored bool, err error) {
	old, err := u.target(ctx)
	if err != nil {
		return false, err
	}
	ref, err := u.refFor(old)
	if err != nil {
		return false, err
	}
	body, err := u.replacement(ctx, old, newID, ref)
	if err != nil {
		return false, err
	}
	name := old.name()
	cfg, _ := old.config()
	timeout := 90
	if t, ok := cfg["StopTimeout"].(float64); ok && t > 0 {
		timeout = int(t)
	}

	// From here, failures put the old container back, undoing each step
	// that happened.
	var renamed, retagged bool
	var newCID string
	restore := func(cause error) (bool, error) {
		var errs []error
		if newCID != "" {
			errs = append(errs, u.d.remove(ctx, newCID))
		}
		if renamed {
			errs = append(errs, u.d.rename(ctx, old.ID, name))
		}
		if retagged {
			errs = append(errs, u.d.tag(ctx, old.Image, u.repo, "latest"))
		}
		// A no-op if stopping failed and it's still running.
		errs = append(errs, u.d.start(ctx, old.ID))
		if err := errors.Join(errs...); err != nil {
			return false, fmt.Errorf("%w; putting the old container back also failed: %w", cause, err)
		}
		return true, cause
	}

	stopCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout+30)*time.Second)
	err = u.d.stop(stopCtx, old.ID, timeout)
	cancel()
	if err != nil {
		return restore(fmt.Errorf("stopping steel: %w", err))
	}
	if err := u.d.rename(ctx, old.ID, name+replacedSuffix); err != nil {
		return restore(fmt.Errorf("renaming the old container: %w", err))
	}
	renamed = true
	// The tag the container names moves too, so `docker compose up` sees
	// nothing to change.
	if err := u.d.tag(ctx, newID, u.repo, "latest"); err != nil {
		return restore(fmt.Errorf("tagging the new image: %w", err))
	}
	retagged = true
	if newCID, err = u.d.create(ctx, name, body); err != nil {
		return restore(fmt.Errorf("creating the new container: %w", err))
	}
	for net, e := range old.NetworkSettings.Networks {
		if _, primary := body.NetworkingConfig.EndpointsConfig[net]; primary {
			continue
		}
		if err := u.d.connect(ctx, net, newCID, cleanEndpoint(e, old.ID)); err != nil {
			return restore(fmt.Errorf("connecting the new container to %s: %w", net, err))
		}
	}
	if err := u.d.start(ctx, newCID); err != nil {
		return restore(fmt.Errorf("starting the new container: %w", err))
	}
	if err := u.watch(ctx, newCID); err != nil {
		return restore(err)
	}
	if err := u.d.remove(ctx, old.ID); err != nil {
		u.opts.Logf("removing the old container: %v", err)
	}
	if err := u.d.tag(ctx, old.Image, u.repo, previousTag); err != nil {
		u.opts.Logf("keeping the old image for rollback: %v", err)
	}
	return false, nil
}

// watch waits out the health window, failing as soon as the container
// stops, restarts or reports itself unhealthy.
func (u *Updater) watch(ctx context.Context, id string) error {
	deadline := time.Now().Add(u.opts.Health)
	for {
		c, err := u.d.inspectContainer(ctx, id)
		if err != nil {
			return fmt.Errorf("watching the new container: %w", err)
		}
		switch {
		case c.RestartCount > 0, !c.State.Running:
			why := fmt.Sprintf("exited with status %d", c.State.ExitCode)
			if c.State.Error != "" {
				why += ": " + c.State.Error
			}
			return fmt.Errorf("the new version didn't stay up: it %s", why)
		case c.State.Health != nil && c.State.Health.Status == "unhealthy":
			return errors.New("the new version reported itself unhealthy")
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(u.poll, time.Until(deadline))):
		}
	}
}

// replacement is the create request for old's replacement from image
// newID. Settings old has only because its image had them (environment,
// labels, entrypoint and the like) are dropped, so the new image's own
// apply; settings compose or the user gave are kept.
func (u *Updater) replacement(ctx context.Context, old container, newID, ref string) (createBody, error) {
	cfg, err := old.config()
	if err != nil {
		return createBody{}, err
	}
	oldImg, err := u.d.inspectImage(ctx, old.Image)
	if err != nil {
		return createBody{}, err
	}
	imgCfg, err := oldImg.config()
	if err != nil {
		return createBody{}, err
	}
	for k, v := range imgCfg {
		switch k {
		case "Env":
			cfg[k] = without(cfg[k], v)
		case "Labels":
			labels, _ := cfg[k].(map[string]any)
			imgLabels, _ := v.(map[string]any)
			for lk, lv := range imgLabels {
				if reflect.DeepEqual(labels[lk], lv) {
					delete(labels, lk)
				}
			}
		case "Image", "Hostname":
		default:
			if reflect.DeepEqual(cfg[k], v) {
				delete(cfg, k)
			}
		}
	}
	// An unset hostname is the container's short ID; the new container
	// gets its own.
	if h, _ := cfg["Hostname"].(string); h != "" && strings.HasPrefix(old.ID, h) {
		delete(cfg, "Hostname")
	}
	cfg["Image"] = ref
	if labels, ok := cfg["Labels"].(map[string]any); ok {
		if _, ok := labels[composeImageLabel]; ok {
			labels[composeImageLabel] = newID
		}
	}

	body := createBody{config: cfg, HostConfig: old.HostConfig}
	var host struct{ NetworkMode string }
	json.Unmarshal(old.HostConfig, &host)
	if e, ok := old.NetworkSettings.Networks[host.NetworkMode]; ok {
		body.NetworkingConfig = &struct{ EndpointsConfig map[string]endpoint }{
			EndpointsConfig: map[string]endpoint{host.NetworkMode: cleanEndpoint(e, old.ID)},
		}
	} else {
		body.NetworkingConfig = &struct{ EndpointsConfig map[string]endpoint }{}
	}
	return body, nil
}

// cleanEndpoint drops the alias Docker adds for a container's short ID,
// which would point at the old container's name.
func cleanEndpoint(e endpoint, oldID string) endpoint {
	e.Aliases = slices.DeleteFunc(slices.Clone(e.Aliases), func(a string) bool {
		return len(a) >= 12 && strings.HasPrefix(oldID, a)
	})
	return e
}

// without returns list's entries that aren't in drop. Both are JSON
// arrays of strings, as decoded.
func without(list, drop any) any {
	l, _ := list.([]any)
	d, _ := drop.([]any)
	var out []any
	for _, x := range l {
		if !slices.Contains(d, x) {
			out = append(out, x)
		}
	}
	return out
}
