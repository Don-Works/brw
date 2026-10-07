package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Don-Works/brw/internal/approval"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/siteconsent"
)

type approvalOptions struct {
	enabled     bool
	tokenFile   string
	storePath   string
	mode        string
	modeSet     bool
	siteConsent bool
	prompt      bool
	httpAddr    string
	upstream    string
	policyPath  string
	artifactDir string
	identity    brwidentity.Identity
}

func validateApprovalOptions(opts approvalOptions) error {
	if !opts.enabled {
		if opts.tokenFile != "" || opts.storePath != "" || opts.modeSet || opts.mode != "risky" {
			return errors.New("approval token, store and mode options require --approvals")
		}
		return nil
	}
	if !opts.siteConsent {
		return errors.New("--approvals requires --site-consent")
	}
	if opts.prompt {
		return errors.New("--approvals is non-interactive and cannot use --site-consent-prompt")
	}
	if strings.TrimSpace(opts.upstream) != "" {
		return errors.New("--approvals cannot use --upstream-http; configure approvals on the browser-host daemon")
	}
	if opts.mode != "risky" && opts.mode != "all" {
		return errors.New("--approval-mode must be risky or all")
	}
	if strings.TrimSpace(opts.tokenFile) == "" {
		return errors.New("--approvals requires --approval-token-file")
	}
	_, port, err := net.SplitHostPort(opts.httpAddr)
	if err != nil || !httpBindIsLoopback(opts.httpAddr) {
		return errors.New("--approvals requires an enabled loopback --http listener")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("--approvals requires a fixed HTTP port between 1 and 65535")
	}
	return nil
}

func buildApprovalStore(opts approvalOptions) (*approval.Store, string, error) {
	if err := validateApprovalOptions(opts); err != nil {
		return nil, "", err
	}
	if !opts.enabled {
		return nil, "", nil
	}
	artifactRoot := strings.TrimSpace(opts.artifactDir)
	switch {
	case strings.EqualFold(artifactRoot, "off"):
		artifactRoot = ""
	case artifactRoot == "" || strings.EqualFold(artifactRoot, "auto"):
		var err error
		artifactRoot, err = defaultArtifactRoot(opts.identity)
		if err != nil {
			return nil, "", fmt.Errorf("resolve artifact directory for approvals: %w", err)
		}
	}
	token, err := readApprovalToken(opts.tokenFile, artifactRoot)
	if err != nil {
		return nil, "", err
	}
	storePath := strings.TrimSpace(opts.storePath)
	if storePath == "" {
		dir, err := siteconsent.DirForPolicy(opts.policyPath)
		if err != nil {
			return nil, "", err
		}
		storePath = filepath.Join(dir, "approvals", "requests.json")
	}
	if !filepath.IsAbs(storePath) {
		return nil, "", errors.New("--approval-store must be an absolute path")
	}
	if err := rejectApprovalArtifactPath(storePath, artifactRoot); err != nil {
		return nil, "", err
	}
	dir := filepath.Dir(storePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create approval directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, "", errors.New("approval store parent must be a non-symlink 0700 directory")
	}
	if info, err := os.Lstat(storePath); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, "", errors.New("approval store must be a non-symlink 0600 regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("inspect approval store: %w", err)
	}
	store, err := approval.Open(storePath)
	if err != nil {
		return nil, "", fmt.Errorf("open approval store: %w", err)
	}
	return store, token, nil
}

func readApprovalToken(path, artifactRoot string) (string, error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return "", errors.New("--approval-token-file must be an absolute path")
	}
	if err := rejectApprovalArtifactPath(path, artifactRoot); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("cannot inspect approval operator token file")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return "", errors.New("approval operator token must be a non-symlink 0600 regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open approval operator token file")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm() != 0o600 {
		return "", errors.New("approval operator token file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", errors.New("cannot read approval operator token file")
	}
	token := strings.TrimRight(string(raw), "\r\n")
	if len(token) < 32 || len(raw) > 4096 {
		return "", errors.New("approval operator token must contain at least 32 characters and fit in 4096 bytes")
	}
	for _, char := range token {
		if char < 0x21 || char > 0x7e {
			return "", errors.New("approval operator token must contain only printable ASCII without whitespace")
		}
	}
	return token, nil
}

func rejectApprovalArtifactPath(path, root string) error {
	if root == "" {
		return nil
	}
	resolvedPath, err := resolveApprovalPath(path)
	if err != nil {
		return err
	}
	resolvedRoot, err := resolveApprovalPath(root)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return err
	}
	if filepath.IsLocal(relative) {
		return errors.New("approval operator token and store must live outside the artifact directory")
	}
	return nil
}

func resolveApprovalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) || filepath.Dir(current) == current {
			return "", fmt.Errorf("resolve approval storage path: %w", err)
		}
		suffix = append(suffix, filepath.Base(current))
		current = filepath.Dir(current)
	}
}
