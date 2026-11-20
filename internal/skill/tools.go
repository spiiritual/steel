package skill

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spiiritual/steel/internal/tool"
)

// Prompt is the system prompt's part about skills, kept in dir.
func Prompt(dir string) string {
	return strings.ReplaceAll(skillsPrompt, "{dir}", dir)
}

const skillsPrompt = `## Skills

Skills are how you remember how to do things. A skill is a folder in {dir} holding SKILL.md: a name, a description of what it does and when to use it, and instructions, beside any scripts or files they use. Each conversation starts with a list of your skills. Before a task one of them describes, read it with tools.skills.read and follow it.

When you've worked out how to do something you may be asked to do again, save how with tools.skills.write, or improve the skill you used: when it took several tries, needed facts you had to find, or the user corrected how you did it. Write what works for a future you who remembers nothing of this conversation: the steps, commands, where things are, the mistakes to avoid. Memory notes are for facts; skills are for how to do things. Put scripts and templates a skill uses in its folder with bash.

To install a skill the user gives you (a link, files, text), put its folder in {dir}.`

// Reflect is what the model is asked when a conversation it worked in
// goes quiet (agent.Config.Reflect). It must think about skills, not
// write one: most work teaches nothing worth keeping.
const Reflect = `This conversation has gone quiet. The user doesn't see this message or your reply. Before moving on, look back over the work in it: did you work out how to do something you may be asked to do again, follow a skill that turned out wrong or incomplete, or get corrected by the user on how to do something? If so, save or fix the skill with tools.skills.write now. If not, change nothing: most work teaches nothing worth keeping. Either way, end with one line saying what you decided and why.`

// Tools returns the model's way into s: tools.skills.list, .read, .write
// and .delete. notify, if set, tells the user when a skill is saved or
// deleted: skills change what steel will do later, so changes are never
// silent.
func Tools(s *Store, notify func(ctx context.Context, text string)) []tool.Tool {
	type nameArgs struct {
		Name string `json:"name" desc:"The skill's name, e.g. \"deploy-blog\""`
	}
	type writeArgs struct {
		Name         string `json:"name" desc:"Lowercase letters, digits and hyphens, e.g. \"deploy-blog\"; an existing skill's name updates it"`
		Description  string `json:"description" desc:"One or two sentences: what it does and when to use it, e.g. \"Deploy the blog to Netlify. Use when the user asks to publish or update the blog.\""`
		Instructions string `json:"instructions" desc:"Markdown: the steps, commands, file locations and pitfalls; replaces the old instructions whole"`
	}
	type listed struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	type listResult struct {
		Skills   []listed `json:"skills"`
		Problems []string `json:"problems,omitempty"`
	}
	type readResult struct {
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		Dir          string   `json:"dir"`
		Instructions string   `json:"instructions"`
		Files        []string `json:"files"`
	}
	tell := func(ctx context.Context, text string) {
		if notify != nil {
			notify(ctx, text)
		}
	}
	return []tool.Tool{
		tool.New("skills.list",
			"Every skill, by name, with what it does; and why any folder in the skills directory isn't a usable skill.",
			func(context.Context, struct{}) (listResult, error) {
				skills, problems := s.List()
				out := listResult{Skills: []listed{}, Problems: problems}
				for _, sk := range skills {
					out.Skills = append(out.Skills, listed(sk))
				}
				return out, nil
			}),
		tool.New("skills.read",
			"A skill's instructions, its folder, and the other files in it (paths relative to the folder). Follow the instructions; read or run the files with bash.",
			func(_ context.Context, a nameArgs) (readResult, error) {
				d, err := s.Read(a.Name)
				if err != nil {
					return readResult{}, err
				}
				return readResult{d.Name, d.Description, d.Dir, d.Instructions, d.Files}, nil
			}),
		tool.New("skills.write",
			"Save how to do something as a skill, or update one: its description and instructions. Other files in its folder stay. Returns the folder.",
			func(ctx context.Context, a writeArgs) (string, error) {
				created, err := s.Write(a.Name, a.Description, a.Instructions)
				if err != nil {
					return "", err
				}
				what := "Updated the skill"
				if created {
					what = "Learned a new skill"
				}
				tell(ctx, fmt.Sprintf("%s %s: %s", what, a.Name, cut(strings.Join(strings.Fields(a.Description), " "), indexDescription)))
				return fmt.Sprintf("Saved %s in %s.", a.Name, filepath.Join(s.Dir(), a.Name)), nil
			}),
		tool.New("skills.delete",
			"Delete a skill and everything in its folder, for good.",
			func(ctx context.Context, a nameArgs) (string, error) {
				if err := s.Delete(a.Name); err != nil {
					return "", err
				}
				tell(ctx, "Deleted the skill "+a.Name+".")
				return "Deleted " + a.Name + ".", nil
			}),
	}
}
