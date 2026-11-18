package tool

import "testing"

// Providers come and go a namespace at a time; steel's own tools stay.
func TestRegistryNamespaces(t *testing.T) {
	r := NewRegistry(Tool{Name: "shell.bash"})
	if err := r.Add("shell", "", Tool{Name: "shell.other"}); err == nil {
		t.Error("a provider took steel's namespace")
	}
	if err := r.Add("gh", "", Tool{Name: "other.x"}); err == nil {
		t.Error("a provider added a tool outside its namespace")
	}
	if err := r.Add("gh", "Use issues.", Tool{Name: "gh.issues"}); err != nil {
		t.Fatal(err)
	}
	if r.Remove("shell") {
		t.Error("removed steel's own tools")
	}
	if !r.Remove("gh") || r.Taken("gh") || r.Notes()["gh"] != "" || len(r.All()) != 1 {
		t.Errorf("after Remove: %v", r.All())
	}
}
