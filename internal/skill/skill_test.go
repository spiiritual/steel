package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// put writes a file into the skills directory, as a user would.
func put(t *testing.T, s *Store, rel, content string) {
	t.Helper()
	path := filepath.Join(s.Dir(), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A skill the model writes can be read back, listed, and woken with.
func TestWriteReadList(t *testing.T) {
	s := open(t)
	created, err := s.Write("deploy-blog", "Deploy the blog: use when asked to publish.", "1. Run `make deploy`.\n2. Check the site.")
	if err != nil || !created {
		t.Fatalf("Write: created %v, err %v", created, err)
	}
	d, err := s.Read("deploy-blog")
	if err != nil {
		t.Fatal(err)
	}
	if d.Description != "Deploy the blog: use when asked to publish." || !strings.HasPrefix(d.Instructions, "1. Run `make deploy`.") {
		t.Errorf("Read: %+v", d)
	}
	skills, problems := s.List()
	if len(skills) != 1 || skills[0].Name != "deploy-blog" || len(problems) > 0 {
		t.Errorf("List: %v, %v", skills, problems)
	}
	msgs, err := s.Wake(context.Background(), "chat")
	if err != nil || len(msgs) != 1 || !strings.Contains(msgs[0].Content, "deploy-blog: Deploy the blog: use when asked to publish.\n") {
		t.Errorf("Wake: %v, %v", msgs, err)
	}
}

// Skills from elsewhere, in the Agent Skills format, work as they are:
// block and quoted descriptions, extra frontmatter, supporting files.
func TestUserSkill(t *testing.T) {
	s := open(t)
	put(t, s, "pdf/SKILL.md", "---\nname: pdf\ndescription: >\n  Fill and merge PDFs.\n  Use for any PDF task.\nlicense: MIT\nmetadata:\n  author: someone\n---\n# PDF\n\nRun scripts/fill.py.\n")
	put(t, s, "pdf/scripts/fill.py", "print(1)")
	put(t, s, "pdf/.git/HEAD", "ref")
	put(t, s, "quoted/SKILL.md", "---\r\nname: quoted\r\ndescription: \"Say \\\"hi\\\": politely\"\r\n---\r\nBody\r\n")
	d, err := s.Read("pdf")
	if err != nil {
		t.Fatal(err)
	}
	if d.Description != "Fill and merge PDFs. Use for any PDF task." || d.Instructions != "# PDF\n\nRun scripts/fill.py." {
		t.Errorf("Read: %+v", d)
	}
	if strings.Join(d.Files, ",") != "scripts/fill.py" {
		t.Errorf("Files: %q", d.Files)
	}
	if d, err := s.Read("quoted"); err != nil || d.Description != `Say "hi": politely` || d.Instructions != "Body" {
		t.Errorf("Read quoted: %+v, %v", d, err)
	}

	// Rewriting keeps the rest of the frontmatter and the folder.
	if created, err := s.Write("pdf", "Fill PDFs.", "New steps."); err != nil || created {
		t.Fatalf("Write: created %v, err %v", created, err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Dir(), "pdf", File))
	if !strings.Contains(string(raw), "license: MIT\nmetadata:\n  author: someone\n") {
		t.Errorf("frontmatter lost:\n%s", raw)
	}
	if d, _ := s.Read("pdf"); d.Description != "Fill PDFs." || d.Instructions != "New steps." || len(d.Files) != 1 {
		t.Errorf("after rewrite: %+v", d)
	}
}

// Broken folders are reported, not listed, and don't hide the rest.
func TestProblems(t *testing.T) {
	s := open(t)
	put(t, s, "good/SKILL.md", "---\nname: good\ndescription: Works.\n---\nx")
	put(t, s, "empty/notes.txt", "x")
	put(t, s, "nofront/SKILL.md", "Just text")
	put(t, s, "nodesc/SKILL.md", "---\nname: nodesc\n---\nx")
	put(t, s, "stray.md", "x")
	skills, problems := s.List()
	if len(skills) != 1 || skills[0].Name != "good" {
		t.Errorf("skills: %v", skills)
	}
	if len(problems) != 3 {
		t.Errorf("problems: %q", problems)
	}
}

// Names that could reach outside the skills directory, or break the
// format, are refused.
func TestNames(t *testing.T) {
	s := open(t)
	for _, name := range []string{"../x", "a/b", "Deploy", "two--hyphens", "-x", "", strings.Repeat("a", 65)} {
		if _, err := s.Write(name, "d", "i"); err == nil {
			t.Errorf("Write %q: no error", name)
		}
	}
	for _, name := range []string{"..", "../x", ".hidden", "a/b"} {
		if _, err := s.Read(name); err == nil {
			t.Errorf("Read %q: no error", name)
		}
		if err := s.Delete(name); err == nil {
			t.Errorf("Delete %q: no error", name)
		}
	}
	if _, err := s.Write("ok", " ", "i"); err == nil {
		t.Error("Write with no description: no error")
	}
	if _, err := s.Read("missing"); err == nil || !strings.Contains(err.Error(), `no skill "missing"`) {
		t.Errorf("Read missing: %v", err)
	}
}

// Deleting a linked skill removes the link, not what it points to.
func TestDelete(t *testing.T) {
	s := open(t)
	if _, err := s.Write("mine", "d", "i"); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, File), []byte("---\ndescription: linked\n---\nx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(s.Dir(), "linked")); err != nil {
		t.Fatal(err)
	}
	if skills, _ := s.List(); len(skills) != 2 {
		t.Fatalf("List: %v", skills)
	}
	for _, name := range []string{"mine", "linked"} {
		if err := s.Delete(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(elsewhere, File)); err != nil {
		t.Errorf("linked skill's files: %v", err)
	}
	if skills, _ := s.List(); len(skills) != 0 {
		t.Errorf("after delete: %v", skills)
	}
	if err := s.Delete("mine"); err == nil {
		t.Error("deleting twice: no error")
	}
}

// A new conversation with no skills hears nothing about them; with very
// many, the list stops at its cap and points to skills.list.
func TestWakeCap(t *testing.T) {
	s := open(t)
	if msgs, err := s.Wake(context.Background(), "c"); err != nil || len(msgs) != 0 {
		t.Errorf("empty Wake: %v, %v", msgs, err)
	}
	for i := range 100 {
		if _, err := s.Write("skill-"+string(rune('a'+i/26))+string(rune('a'+i%26)), strings.Repeat("word ", 200), "i"); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := s.Wake(context.Background(), "c")
	if len(msgs[0].Content) > indexBytes+200 || !strings.Contains(msgs[0].Content, "more: tools.skills.list()") {
		t.Errorf("Wake: %d bytes, tail %q", len(msgs[0].Content), msgs[0].Content[len(msgs[0].Content)-80:])
	}
}

// Saving or deleting a skill through the model's tools tells the user.
func TestToolsNotify(t *testing.T) {
	s := open(t)
	var told []string
	tools := Tools(s, func(_ context.Context, text string) { told = append(told, text) })
	run := func(name, args string) {
		t.Helper()
		for _, tl := range tools {
			if tl.Name == name {
				if _, err := tl.Run(context.Background(), []byte(args)); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return
			}
		}
		t.Fatalf("no tool %s", name)
	}
	run("skills.write", `{"name":"deploy-blog","description":"Deploy the blog.","instructions":"make deploy"}`)
	run("skills.write", `{"name":"deploy-blog","description":"Deploy the blog.","instructions":"make deploy-prod"}`)
	run("skills.delete", `{"name":"deploy-blog"}`)
	want := []string{"Learned a new skill deploy-blog: Deploy the blog.", "Updated the skill deploy-blog: Deploy the blog.", "Deleted the skill deploy-blog."}
	if strings.Join(told, "\n") != strings.Join(want, "\n") {
		t.Errorf("told %q, want %q", told, want)
	}
}
