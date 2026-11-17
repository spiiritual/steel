package optmem

import (
	"fmt"
	"strings"
)

// napTask asks for block [lo,hi)'s summary. It's memo's nap prompt, with
// the reply as the line instead of a command to run.
func napTask(lo, hi, max int, body string) string {
	return fmt.Sprintf("Compress memories #%s into one line of at most %d bytes.\n"+
		"Keep what has lasting effect, drop what does not. Invent nothing.\n"+
		"Reply with the line alone.\n\n%s", blockName(lo, hi), max, body)
}

// SystemPrompt is the system prompt with memory on: user's, then how to use
// the memory. It's OptMem's prompt, minus what steel does for the model
// (waking, summarizing) and what doesn't apply (subagents).
func SystemPrompt(user string, entryChars int) string {
	p := strings.ReplaceAll(memoryPrompt, "{chars}", fmt.Sprint(entryChars))
	if user = strings.TrimSpace(user); user != "" {
		return user + "\n\n" + p
	}
	return p
}

const memoryPrompt = `## Memory

Your memory is OptMem. It outlives every conversation, model and vendor
change. Without it you do not know who you are, or what was decided and
tried.

Each conversation starts with your memory, oldest first: "#i date text" is
one memory, "#a-b text" a one-line summary of memories a to b.

### While working: register memories (mandatory)

Call tools.memory.note({text: "<1 line, max {chars} bytes>"}) whenever you learn
something new, or something worth keeping happens. That covers a task
worth real effort, a fact or insight the user teaches you, anything you
learn about their life (even indirectly), any event of lasting effect.

Do not register redundant memories.

### When you need an old memory: search, or navigate

tools.memory.recall({regex: "<regex>"}) searches every memory, word for word.

Your memories also form a binary tree: #0-1, #2-3 ... exist as one-line
summaries, pairs of those as #0-3, and so on -- every "#a-b" line of your
memory is one node of it. tools.memory.zoom({block: "a-b"}) opens a node into its
two halves, down to the raw memories. If a summary is wrong or blank (what
zoom shows below it says otherwise), tools.memory.rewrite({block: "a-b"})
writes it, and the summaries above it, again.`
