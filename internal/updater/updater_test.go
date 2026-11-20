package updater

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const repo = "ghcr.io/spiiritual/steel"

// fakeReleases always reports one release.
type fakeReleases struct {
	version, digest string
	err             error
}

func (r *fakeReleases) Latest(context.Context) (string, string, error) {
	return r.version, r.digest, r.err
}

// imageEnv is what every steel image sets; a container inherits it.
var imageEnv = []any{"PATH=/usr/bin", "STEEL_CONTAINER=1"}

// world is a compose project running steel v1.0.0 beside the updater, with
// v2.0.0 published.
type world struct {
	d        *fakeDocker
	u        *Updater
	releases *fakeReleases
	old      *fakeImage
	next     *fakeImage
}

func newWorld(t *testing.T, nextLabels map[string]any) *world {
	t.Helper()
	d := newFakeDocker(t)
	w := &world{d: d}
	w.old = d.addImage("v1.0.0", imageEnv, nil, repo+":latest", repo+":v1.0.0")
	w.next = d.addImage("v2.0.0", append(slices.Clone(imageEnv), "NEW=1"), nextLabels)
	d.publish(w.next, repo, "sha256:feed")
	w.releases = &fakeReleases{version: "v2.0.0", digest: "sha256:feed"}

	d.addContainer(&fakeContainer{ID: "self0000000000000000", Name: "proj-updater-1", Running: true,
		Config: map[string]any{"Labels": map[string]any{"com.docker.compose.project": "proj", "com.docker.compose.service": "updater"}}})
	steelID := strings.Repeat("ab", 32)
	d.addContainer(&fakeContainer{
		ID: steelID, Name: "proj-steel-1", Image: w.old.ID, Running: true,
		Config: map[string]any{
			"Image":       repo + ":latest",
			"Hostname":    "steel",
			"Env":         append(slices.Clone(imageEnv), "OPENAI_MODEL=m"),
			"Entrypoint":  []any{"tini", "--", "steel-entrypoint"},
			"User":        "steel",
			"StopTimeout": 90,
			"Labels": map[string]any{
				VersionLabel:                 "v1.0.0",
				"com.docker.compose.project": "proj",
				"com.docker.compose.service": "steel",
				composeImageLabel:            w.old.ID,
			},
		},
		HostConfig: []byte(`{"NetworkMode":"proj_default","Binds":["proj_home:/home/steel"]}`),
		Networks: map[string]endpoint{
			"proj_default": {Aliases: []string{"proj-steel-1", "steel", steelID[:12]}},
			"proj_extra":   {Aliases: []string{"steel"}},
		},
	})

	u, err := New(Options{Docker: d.server.URL, Self: "self0000", Health: 50 * time.Millisecond, Releases: w.releases})
	if err != nil {
		t.Fatal(err)
	}
	u.poll = 5 * time.Millisecond
	w.u = u
	return w
}

// run starts action and waits for it to finish.
func (w *world) run(t *testing.T, action string) Result {
	t.Helper()
	if _, err := w.u.Start(context.Background(), action); err != nil {
		t.Fatalf("Start(%s): %v", action, err)
	}
	w.u.mu.Lock()
	done := w.u.finished
	w.u.mu.Unlock()
	<-done
	s, err := w.u.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Busy || s.Last == nil {
		t.Fatalf("after %s: busy %v, last %v", action, s.Busy, s.Last)
	}
	return *s.Last
}

func TestUpdateReplacesSteelsContainer(t *testing.T) {
	w := newWorld(t, nil)
	res := w.run(t, "update")
	if res.Error != "" || res.From != "v1.0.0" || res.To != "v2.0.0" {
		t.Fatalf("result: %+v", res)
	}

	c := w.d.byName("proj-steel-1")
	if c == nil || c.Image != w.next.ID || !c.Running {
		t.Fatalf("steel's container after the update: %+v", c)
	}
	if w.d.byName("proj-steel-1"+replacedSuffix) != nil {
		t.Error("the old container is still there")
	}
	if got := w.d.stops; !slices.Equal(got, []int{90}) {
		t.Errorf("stop timeouts %v, want the container's own 90s, so steel can drain", got)
	}

	// Settings that came from the old image give way to the new image's;
	// the ones compose set stay.
	if env := c.Config["Env"]; !slices.Equal(env.([]any), []any{"OPENAI_MODEL=m"}) {
		t.Errorf("Env = %v, want only compose's", env)
	}
	if _, ok := c.Config["Entrypoint"]; ok {
		t.Error("the old image's entrypoint was copied")
	}
	labels := c.Config["Labels"].(map[string]any)
	if _, ok := labels[VersionLabel]; ok {
		t.Error("the old image's version label was copied")
	}
	if labels["com.docker.compose.project"] != "proj" || labels[composeImageLabel] != w.next.ID {
		t.Errorf("labels = %v: compose's must stay, its image label must name the new image", labels)
	}
	if c.Config["Hostname"] != "steel" || c.Config["Image"] != repo+":latest" || !strings.Contains(string(c.HostConfig), "proj_home") {
		t.Errorf("settings lost: %v %s", c.Config, c.HostConfig)
	}
	if a := c.Networks["proj_default"].Aliases; !slices.Equal(a, []string{"proj-steel-1", "steel"}) {
		t.Errorf("aliases on the default network: %v", a)
	}
	if _, ok := c.Networks["proj_extra"]; !ok {
		t.Error("not reconnected to the second network")
	}

	// `docker compose up` sees :latest on the new image; the old is kept.
	if w.d.tagged(repo+":latest") != w.next.ID || w.d.tagged(repo+":v2.0.0") != w.next.ID || w.d.tagged(repo+":previous") != w.old.ID {
		t.Errorf("tags: %v", w.d.tags)
	}
	s, _ := w.u.Status(context.Background())
	if s.Current != "v2.0.0" || s.Previous != "v1.0.0" || s.Latest != "v2.0.0" {
		t.Errorf("status: %+v", s)
	}
}

func TestUpdateThatDoesntStartIsUndone(t *testing.T) {
	w := newWorld(t, map[string]any{"test.crash": "true"})
	old := w.d.byName("proj-steel-1")
	res := w.run(t, "update")
	if res.Error == "" || !res.Restored {
		t.Fatalf("result: %+v, want a failure with steel restored", res)
	}
	c := w.d.byName("proj-steel-1")
	if c == nil || c.ID != old.ID || !c.Running {
		t.Fatalf("steel's container: %+v, want the old one running again", c)
	}
	if len(w.d.containers) != 2 {
		t.Errorf("%d containers, want the failed one removed", len(w.d.containers))
	}
	if w.d.tagged(repo+":latest") != w.old.ID || w.d.tagged(repo+":previous") != "" {
		t.Errorf("tags: %v, want :latest back on the old image and no rollback target", w.d.tags)
	}
}

func TestRollbackAndRollForward(t *testing.T) {
	w := newWorld(t, nil)
	w.run(t, "update")
	res := w.run(t, "rollback")
	if res.Error != "" || res.From != "v2.0.0" || res.To != "v1.0.0" {
		t.Fatalf("rollback: %+v", res)
	}
	if c := w.d.byName("proj-steel-1"); c.Image != w.old.ID {
		t.Fatalf("after rollback steel runs %s", c.Image)
	}
	s, _ := w.u.Status(context.Background())
	if s.Current != "v1.0.0" || s.Previous != "v2.0.0" {
		t.Errorf("status after rollback: %+v", s)
	}
	// Rolling back again returns to v2, like `steel rollback`.
	if res := w.run(t, "rollback"); res.To != "v2.0.0" || res.Error != "" {
		t.Errorf("second rollback: %+v", res)
	}
}

func TestNothingToDo(t *testing.T) {
	w := newWorld(t, nil)
	if _, err := w.u.Start(context.Background(), "rollback"); err == nil || !strings.Contains(err.Error(), "no previous version") {
		t.Errorf("rollback with nothing to go back to: %v", err)
	}
	w.releases.version = "v1.0.0"
	if _, err := w.u.Start(context.Background(), "update"); !errors.Is(err, ErrUpToDate) {
		t.Errorf("update when up to date: %v", err)
	}
	w.releases.err = errors.New("checksums.txt is not signed by steel's release key")
	if _, err := w.u.Start(context.Background(), "update"); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Errorf("update of an unverified release: %v", err)
	}
	// None of that touched steel or left the updater busy.
	if c := w.d.byName("proj-steel-1"); c.Image != w.old.ID || !c.Running || len(w.d.stops) != 0 {
		t.Errorf("steel was touched: %+v", c)
	}
	w.releases.err, w.releases.version = nil, "v2.0.0"
	if res := w.run(t, "update"); res.Error != "" {
		t.Errorf("update after refusals: %+v", res)
	}
}

func TestPinnedImageIsntUpdated(t *testing.T) {
	w := newWorld(t, nil)
	w.d.containers[strings.Repeat("ab", 32)].Config["Image"] = repo + ":v1.0.0"
	_, err := w.u.Start(context.Background(), "update")
	if err == nil || !strings.Contains(err.Error(), "pins steel") {
		t.Errorf("update of a pinned container: %v", err)
	}
}

func TestClientOverHTTP(t *testing.T) {
	w := newWorld(t, nil)
	w.u.opts.Health = time.Second // long enough to see it busy
	srv := httptest.NewServer(w.u.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		w.u.mu.Lock()
		done := w.u.finished
		w.u.mu.Unlock()
		if done != nil {
			<-done
		}
	})
	c := &Client{URL: srv.URL}
	ctx := context.Background()

	s, err := c.Status(ctx)
	if err != nil || s.Current != "v1.0.0" || s.Latest != "v2.0.0" || s.Busy {
		t.Fatalf("Status = %+v, %v", s, err)
	}
	res, err := c.Start(ctx, "update")
	if err != nil || res.From != "v1.0.0" || res.To != "v2.0.0" {
		t.Fatalf("Start = %+v, %v", res, err)
	}
	if _, err := c.Start(ctx, "rollback"); !errors.Is(err, ErrBusy) {
		t.Errorf("second Start while busy: %v", err)
	}
	if s, _ := c.Status(ctx); !s.Busy || s.Last == nil || s.Last.To != "v2.0.0" {
		t.Errorf("Status while updating: %+v", s)
	}
}

func TestVersionNegotiation(t *testing.T) {
	for _, tc := range []struct{ max, min, want string }{
		{"1.54", "1.40", apiVersion},
		{"1.41", "1.12", "1.41"}, // Docker 20.10
		{"1.60", "1.50", "1.50"}, // a future engine that dropped 1.44
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{"ApiVersion": tc.max, "MinAPIVersion": tc.min})
		}))
		d, _ := newDocker(srv.URL)
		if got, err := d.negotiate(context.Background()); err != nil || got != tc.want {
			t.Errorf("engine %s–%s: negotiated %q, %v; want %s", tc.min, tc.max, got, err, tc.want)
		}
		srv.Close()
	}
}

func TestClientUpToDate(t *testing.T) {
	w := newWorld(t, nil)
	w.releases.version = "v1.0.0"
	srv := httptest.NewServer(w.u.Handler())
	t.Cleanup(srv.Close)
	res, err := (&Client{URL: srv.URL}).Start(context.Background(), "update")
	if !errors.Is(err, ErrUpToDate) || res.From != "v1.0.0" {
		t.Errorf("Start = %+v, %v", res, err)
	}
}
