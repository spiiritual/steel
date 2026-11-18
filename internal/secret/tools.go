package secret

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spiiritual/steel/internal/tool"
)

type nameArgs struct {
	Name string `json:"name" desc:"Upper case, like GITHUB_TOKEN"`
}

type addArgs struct {
	Name    string `json:"name" desc:"Upper case, like GITHUB_TOKEN"`
	Purpose string `json:"purpose" desc:"What it's for, shown to the user, e.g. \"GitHub API calls\""`
}

// Tools returns the tools.secrets namespace for s.
func Tools(s *Store) []tool.Tool {
	return []tool.Tool{
		tool.New("secrets.list",
			"Names of the user's saved secrets (API tokens, passwords).",
			func(ctx context.Context, _ struct{}) ([]string, error) {
				return s.Names()
			}),
		tool.New("secrets.get",
			"A secret's placeholder, \"{{secret:NAME}}\", which stands for the value. Put it in the arguments of tools.shell.bash or an MCP tool, e.g. inside a command or header string, and steel swaps in the value when the call runs, after the user approves it. Values in anything tools return, including files that hold secrets such as ~/.steel/secrets.env, come back as placeholders. Other tools (memory, schedule) get the placeholder as written. Throws if there's no such secret.",
			func(ctx context.Context, a nameArgs) (string, error) {
				ok, err := s.Has(a.Name)
				if err != nil {
					return "", err
				}
				if !ok {
					return "", errNoSecret(a.Name)
				}
				return Placeholder(a.Name), nil
			}),
		tool.New("secrets.add",
			fmt.Sprintf("Ask the user to enter a secret; the value goes straight into steel's store, not the chat. Use this rather than asking the user to paste a secret into a message. Replaces a secret that already has the name. Waits up to %s for the user; returns the new secret's placeholder, as tools.secrets.get does.", tool.SecretTimeout),
			func(ctx context.Context, a addArgs) (string, error) {
				if err := s.CheckName(a.Name); err != nil {
					return "", err
				}
				if strings.TrimSpace(a.Purpose) == "" {
					return "", errors.New("give a purpose: the user sees it, to know which value you need")
				}
				save := func(v string) error { return s.Set(a.Name, v) }
				if err := tool.AskSecret(ctx, a.Name, a.Purpose, save); err != nil {
					return "", err
				}
				return Placeholder(a.Name), nil
			}),
		tool.New("secrets.remove",
			"Delete a secret for good, once the user approves. Returns whether it existed.",
			func(ctx context.Context, a nameArgs) (bool, error) {
				if err := s.CheckName(a.Name); err != nil {
					return false, err
				}
				ok, err := s.Has(a.Name)
				if err != nil || !ok {
					return false, err
				}
				if err := tool.Approve(ctx, tool.Approval{Tool: "secrets.remove", Action: "Delete the secret " + a.Name, Reasons: []string{"can't be undone"}}); err != nil {
					return false, err
				}
				return s.Remove(a.Name)
			}),
	}
}
