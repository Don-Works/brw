package siteconsent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// TerminalPrompter asks the person running brwd, on the terminal it was started from.
type TerminalPrompter struct {
	mu     sync.Mutex
	reader *bufio.Reader
	out    io.Writer
}

// NewTerminalPrompter reads answers from in and writes questions to out.
func NewTerminalPrompter(in io.Reader, out io.Writer) *TerminalPrompter {
	return &TerminalPrompter{reader: bufio.NewReader(in), out: out}
}

// AskSite asks whether brw may read or act on an origin.
func (p *TerminalPrompter) AskSite(origin string, scope Scope) (bool, error) {
	action := "read"
	if scope == ScopeAct {
		action = "read AND change things on"
	}
	lines := []string{
		fmt.Sprintf("brw wants to %s %s.", action, origin),
		"This answer is recorded and applies until you revoke it (brwctl grants revoke).",
	}
	return p.ask(strings.Join(lines, "\n"))
}

// ConfirmAction asks whether one high-risk action may go ahead.
func (p *TerminalPrompter) ConfirmAction(request ActionRequest, risks []Risk) (bool, error) {
	target := request.Label
	if target == "" {
		target = request.Text
	}
	question := fmt.Sprintf("brw is about to run %s on %s", request.Tool, request.Origin)
	if target != "" {
		question += fmt.Sprintf(" targeting %q", target)
	}
	question += ".\nThis looks like: " + Summary(risks) + "\nThis answer is NOT recorded; you will be asked again."
	return p.ask(question)
}

func (p *TerminalPrompter) ask(question string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintln(p.out, question)
	fmt.Fprint(p.out, "Allow? [y/N]: ")
	line, err := p.reader.ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(p.out, "no answer (input closed); refusing")
			return false, ErrPromptUnanswerable
		}
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	allowed := answer == "y" || answer == "yes"
	if allowed {
		fmt.Fprintln(p.out, "allowed")
	} else {
		fmt.Fprintln(p.out, "refused")
	}
	return allowed, nil
}
