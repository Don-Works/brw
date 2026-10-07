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
	"slices"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/credential"
)

const defaultCredentialTimeout = 10 * time.Second

const execWaitDelay = 2 * time.Second

func newCredentialProvider(spec CredentialProviderSpec) (credentialProvider, error) {
	timeout := defaultCredentialTimeout
	if spec.TimeoutMS > 0 {
		timeout = time.Duration(spec.TimeoutMS) * time.Millisecond
	}
	switch spec.Kind {
	case CredentialKindExec:
		command := slices.Clone(spec.Command)
		if len(command) == 0 {

			return nil, errors.New("the exec credential kind requires a command")
		}
		program, err := checkProgramTrust("credential command program", command[0])
		if err != nil {
			return nil, err
		}
		return &execProvider{command: command, program: program, timeout: timeout}, nil
	case CredentialKindFile:
		root, err := filepath.Abs(spec.Directory)
		if err != nil {
			return nil, fmt.Errorf("resolve credential directory: %w", err)
		}
		if err := checkCredentialDirectoryTrust(root); err != nil {
			return nil, err
		}
		return &fileProvider{root: root, timeout: timeout}, nil
	default:
		return nil, fmt.Errorf("credential kind %q is not supported", spec.Kind)
	}
}

type trustedProgram struct {
	declared string
	resolved string
}

func checkProgramTrust(kind, program string) (trustedProgram, error) {
	resolved, err := filepath.EvalSymlinks(program)
	if err != nil {
		return trustedProgram{}, fmt.Errorf("resolve %s %q: %w", kind, program, err)
	}
	what := fmt.Sprintf("%s %q", kind, program)
	if err := refuseWritableAncestors(program, what); err != nil {
		return trustedProgram{}, err
	}
	if err := checkProgramFileTrust(resolved, what); err != nil {
		return trustedProgram{}, err
	}
	return trustedProgram{declared: program, resolved: resolved}, nil
}

func checkProgramFileTrust(resolved, what string) error {
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("read %s: %w", what, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", what)
	}
	if err := refuseSharedWrite(info.Mode().Perm(), what); err != nil {
		return err
	}
	if err := refuseForeignOwner(info, what); err != nil {
		return err
	}
	return refuseWritableAncestors(resolved, what)
}

func checkCredentialDirectoryTrust(root string) error {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve credential directory %q: %w", root, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("read credential directory %q: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("credential directory %q is not a directory", root)
	}
	what := fmt.Sprintf("credential directory %q", root)
	if err := refuseSharedWrite(info.Mode().Perm(), what); err != nil {
		return err
	}
	if err := refuseForeignOwner(info, what); err != nil {
		return err
	}
	if err := refuseWritableAncestors(root, what); err != nil {
		return err
	}
	return refuseWritableAncestors(resolved, what)
}

type execProvider struct {
	command []string
	program trustedProgram
	timeout time.Duration
}

func (p *execProvider) resolve(ctx context.Context, reference string) (credential.Secret, error) {
	what := fmt.Sprintf("credential command program %q", p.program.declared)
	if err := checkProgramFileTrust(p.program.resolved, what); err != nil {
		return credential.Secret{}, err
	}
	argv := substituteReference(p.command, reference)

	argv[0] = p.program.resolved
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)

	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = execWaitDelay

	stdout := &boundedBuffer{limit: credential.MaxValueBytes + 1}
	cmd.Stdout = stdout
	err := cmd.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return credential.Secret{}, fmt.Errorf("credential provider timed out after %s", p.timeout)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {

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
