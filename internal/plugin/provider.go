package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/credential"
)

// defaultCredentialTimeout applies when a manifest sets none. A vault CLI that
// has to unlock can take a few seconds; one that takes ten has a problem the
// recipe should surface rather than wait through.
const defaultCredentialTimeout = 10 * time.Second

// execWaitDelay is how long Wait may keep running after the context deadline
// killed the process. exec.CommandContext kills the child but still waits for
// every inherited writer to close the stdout pipe, so a provider that forks a
// grandchild and exits holds cmd.Run() open indefinitely. Without this the
// documented deadline is not a deadline.
const execWaitDelay = 2 * time.Second

func newCredentialProvider(spec CredentialProviderSpec) (credentialProvider, error) {
	timeout := defaultCredentialTimeout
	if spec.TimeoutMS > 0 {
		timeout = time.Duration(spec.TimeoutMS) * time.Millisecond
	}
	switch spec.Kind {
	case CredentialKindExec:
		command := append([]string(nil), spec.Command...)
		if len(command) == 0 {
			// Unreachable through Load, which validates the manifest first. Kept
			// so a future caller cannot reach the index below on an empty argv.
			return nil, errors.New("the exec credential kind requires a command")
		}
		if err := refuseUntrustedProgram(command[0]); err != nil {
			return nil, err
		}
		return &execProvider{command: command, timeout: timeout}, nil
	case CredentialKindFile:
		root, err := filepath.Abs(spec.Directory)
		if err != nil {
			return nil, fmt.Errorf("resolve credential directory: %w", err)
		}
		return &fileProvider{root: root, timeout: timeout}, nil
	default:
		return nil, fmt.Errorf("credential kind %q is not supported", spec.Kind)
	}
}

// refuseUntrustedProgram applies the plugin directory's own rule to the binary
// an exec provider runs. The manifest is 0600 and owner-checked, but that only
// fixes the argv: if another local user can write the program it names, or the
// directory holding it, they still choose what brwd executes. Checked at load
// so a misconfigured provider is a startup failure rather than a login that
// fails three steps into a recipe.
func refuseUntrustedProgram(program string) error {
	resolved, err := filepath.EvalSymlinks(program)
	if err != nil {
		return fmt.Errorf("resolve credential command program %q: %w", program, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("read credential command program %q: %w", program, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("credential command program %q is not a regular file", program)
	}
	what := fmt.Sprintf("credential command program %q", program)
	if err := refuseSharedWrite(info.Mode().Perm(), what); err != nil {
		return err
	}
	if err := refuseForeignOwner(info, what); err != nil {
		return err
	}
	return refuseWritableAncestors(resolved, what)
}

// execProvider runs a fixed argv and reads one value from its stdout.
type execProvider struct {
	command []string
	timeout time.Duration
}

func (p *execProvider) resolve(ctx context.Context, reference string) (credential.Secret, error) {
	argv := substituteReference(p.command, reference)
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// No shell and no stdin. stderr is sent to the void rather than captured:
	// exec.ExitError would otherwise retain it, and a provider that prints the
	// value on its failure path would put it in a retained buffer.
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = execWaitDelay
	// One byte over the cap so Validate can tell "at the limit" from "truncated".
	stdout := &boundedBuffer{limit: credential.MaxValueBytes + 1}
	cmd.Stdout = stdout
	err := cmd.Run()
	// Checked before the run error so an expired deadline reads as a timeout on
	// every path, including the race where Run returns nil as the deadline
	// passes: output from a call that outran its deadline is not an answer.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return credential.Secret{}, fmt.Errorf("credential provider timed out after %s", p.timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Deliberately without the provider's stderr: see above.
			return credential.Secret{}, fmt.Errorf("credential provider exited with status %d", exitErr.ExitCode())
		}
		return credential.Secret{}, fmt.Errorf("credential provider could not be run: %w", err)
	}
	secret, err := credential.Validate(stdout.bytes())
	if err != nil {
		return credential.Secret{}, fmt.Errorf("credential provider: %w", err)
	}
	return secret, nil
}

// boundedBuffer stops a runaway provider filling the daemon's heap. It never
// reports a write error, because killing the pipe mid-write would turn "printed
// too much" into a confusing broken-pipe failure instead of a size refusal.
type boundedBuffer struct {
	limit int
	data  []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.data); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.data = append(b.data, p[:room]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) bytes() []byte { return b.data }

// fileProvider reads <root>/<reference>. It is the reference implementation the
// test suite uses, so the credential path is exercised on a machine with no
// vault CLI installed at all.
type fileProvider struct {
	root    string
	timeout time.Duration
}

func (p *fileProvider) resolve(ctx context.Context, reference string) (credential.Secret, error) {
	if err := ctx.Err(); err != nil {
		return credential.Secret{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	type outcome struct {
		secret credential.Secret
		err    error
	}
	// os.Stat and os.ReadFile take no context, so the deadline docs/plugins.md
	// promises can only be kept by waiting on them from the outside. The read
	// goroutine outlives a timeout on purpose: abandoning a read wedged on a
	// dead network mount or a named pipe with no writer is the whole point, and
	// the buffered channel lets it finish and be collected either way.
	done := make(chan outcome, 1)
	go func() {
		secret, err := p.read(reference)
		done <- outcome{secret: secret, err: err}
	}()
	select {
	case result := <-done:
		return result.secret, result.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return credential.Secret{}, fmt.Errorf("credential provider timed out after %s", p.timeout)
		}
		return credential.Secret{}, ctx.Err()
	}
}

func (p *fileProvider) read(reference string) (credential.Secret, error) {
	path, err := p.resolvePath(reference)
	if err != nil {
		return credential.Secret{}, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return credential.Secret{}, fmt.Errorf("%w: %q", credential.ErrReferenceNotFound, reference)
	}
	if err != nil {
		return credential.Secret{}, fmt.Errorf("read credential %q: %w", reference, err)
	}
	if info.IsDir() {
		return credential.Secret{}, fmt.Errorf("%w: %q names a directory", credential.ErrReferenceNotFound, reference)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return credential.Secret{}, fmt.Errorf("credential file for %q is readable by group or other; chmod 600 it", reference)
	}
	if info.Size() > credential.MaxValueBytes {
		return credential.Secret{}, fmt.Errorf("credential file for %q exceeds %d bytes", reference, credential.MaxValueBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return credential.Secret{}, fmt.Errorf("read credential %q: %w", reference, err)
	}
	secret, err := credential.Validate(raw)
	if err != nil {
		return credential.Secret{}, fmt.Errorf("credential %q: %w", reference, err)
	}
	return secret, nil
}

// resolvePath keeps the read inside the configured directory. The reference
// pattern already excludes "..", but a symlink inside the directory is a second
// way out, so the resolved path is re-checked after following links.
func (p *fileProvider) resolvePath(reference string) (string, error) {
	candidate := filepath.Join(p.root, filepath.FromSlash(reference))
	if err := containedIn(p.root, candidate); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
		return "", fmt.Errorf("resolve credential %q: %w", reference, err)
	}
	realRoot, err := filepath.EvalSymlinks(p.root)
	if err != nil {
		return "", fmt.Errorf("resolve credential directory: %w", err)
	}
	if err := containedIn(realRoot, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func containedIn(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("credential path escapes the configured credential directory")
	}
	return nil
}
