package optmem

import (
	"context"
	"fmt"

	"github.com/spiiritual/steel/internal/tool"
)

// Tools returns the model's way into m: tools.memory.note, .recall, .zoom
// and .rewrite.
func Tools(m *Memory) []tool.Tool {
	type noteArgs struct {
		Text string `json:"text" desc:"The memory: one line"`
	}
	type recallArgs struct {
		Regex string `json:"regex" desc:"A regular expression (RE2), matched case-insensitively against each \"#id date text\" line"`
	}
	type zoomArgs struct {
		Block string `json:"block" desc:"A node's id as your memory shows it, e.g. \"16-31\""`
	}
	return []tool.Tool{
		tool.New("memory.note",
			fmt.Sprintf("Record one memory, forever: one line, at most %d bytes. Returns its id, e.g. \"Saved as #41.\"", m.EntryChars()),
			func(_ context.Context, a noteArgs) (string, error) {
				id, err := m.Note(a.Text)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Saved as #%d.", id), nil
			}),
		tool.New("memory.recall",
			"Search every memory ever recorded, word for word. Returns the matching \"#id date text\" lines, oldest first (the newest that fit, if there are many).",
			func(_ context.Context, a recallArgs) (string, error) { return m.Recall(a.Regex) }),
		tool.New("memory.zoom",
			"Open a node of your memory's tree into its two halves: two summaries, or the raw memories once a half is one memory.",
			func(_ context.Context, a zoomArgs) (string, error) { return m.Zoom(a.Block) }),
		tool.New("memory.rewrite",
			"Write a wrong or blank summary again from what's below it, and every summary built on it. Takes a few seconds per level. Returns the new lines.",
			func(ctx context.Context, a zoomArgs) (string, error) { return m.Rewrite(ctx, a.Block) }),
	}
}
