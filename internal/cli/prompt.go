package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// stdinReader caches one buffered reader over deps.Stdin so successive
// prompts do not lose buffered input.
type stdinReader struct {
	reader *bufio.Reader
}

func newStdinReader(r io.Reader) *stdinReader {
	if r == nil {
		r = strings.NewReader("")
	}
	return &stdinReader{reader: bufio.NewReader(r)}
}

func (s *stdinReader) readLine() (string, error) {
	line, err := s.reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// prompter reads interactive input through the injected prompt hooks and
// falls back to deps.Stdin.
//
// One prompter serves a whole command: a fresh bufio.Reader per prompt
// would swallow the lines still buffered for the prompts that follow,
// which breaks every non-interactive (piped) invocation.
type prompter struct {
	deps Deps
	in   *stdinReader
}

func newPrompter(deps Deps) *prompter {
	return &prompter{deps: deps, in: newStdinReader(deps.Stdin)}
}

// line asks for a line of visible input.
func (p *prompter) line(prompt string) (string, error) {
	if p.deps.PromptLine != nil {
		return p.deps.PromptLine(prompt)
	}
	fmt.Fprint(p.out(), prompt)
	value, err := p.in.readLine()
	if err != nil {
		return "", fmt.Errorf("read %s: %w", promptLabel(prompt), err)
	}
	return strings.TrimSpace(value), nil
}

// secret asks for a value that must not be echoed.
func (p *prompter) secret(prompt string) (string, error) {
	if p.deps.PromptPassword != nil {
		return p.deps.PromptPassword(prompt)
	}
	return readPasswordFrom(p.terminalFD(), p.in, prompt, p.out())
}

// otp asks for a one-time verification code.
func (p *prompter) otp() (string, error) {
	if p.deps.PromptOTP != nil {
		return p.deps.PromptOTP()
	}
	return readPasswordFrom(p.terminalFD(), p.in, "MFA verification code: ", p.out())
}

// confirm asks a yes/no question; an empty answer means no, matching the
// reference CLI's confirmation default.
func (p *prompter) confirm(prompt string) (bool, error) {
	if p.deps.Confirm != nil {
		return p.deps.Confirm(prompt)
	}
	fmt.Fprintf(p.out(), "%s [y/N]: ", prompt)
	answer, err := p.in.readLine()
	if err != nil {
		return false, fmt.Errorf("read %s: %w", promptLabel(prompt), err)
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func (p *prompter) out() io.Writer {
	if p.deps.Out == nil {
		return io.Discard
	}
	return p.deps.Out
}

// terminalFD returns the stdin descriptor only when deps.Stdin really is
// the terminal: a redirected or injected stream must never be read
// through the terminal API, or a test would block on the real console.
func (p *prompter) terminalFD() int {
	if p.deps.Stdin == os.Stdin {
		return int(os.Stdin.Fd())
	}
	return -1
}

// promptLabel trims a prompt into something usable inside an error.
func promptLabel(prompt string) string {
	label := strings.TrimSpace(prompt)
	label = strings.TrimRight(label, ": ")
	if label == "" {
		return "input"
	}
	return label
}

// readPasswordFrom reads one line without echo.
//
// It reads the terminal directly when fd is a terminal, and otherwise
// consumes the shared buffered reader so that piped input survives across
// the successive prompts of one command.
func readPasswordFrom(fd int, in *stdinReader, prompt string, out io.Writer) (string, error) {
	if out != nil {
		fmt.Fprint(out, prompt)
	}
	if fd >= 0 && term.IsTerminal(fd) {
		data, err := term.ReadPassword(fd)
		if out != nil {
			fmt.Fprintln(out)
		}
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
	line, err := in.readLine()
	if err != nil {
		return "", err
	}
	return line, nil
}

// ReadPassword reads a password from fd without echo when fd is a
// terminal, and from the fallback reader otherwise.
//
// The terminal path uses golang.org/x/term; the fallback exists so that
// `jms config add` works in scripts and CI.
func ReadPassword(fd int, fallback io.Reader, prompt string, out io.Writer) (string, error) {
	return readPasswordFrom(fd, newStdinReader(fallback), prompt, out)
}
