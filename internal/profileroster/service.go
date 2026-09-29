package profileroster

import (
	"context"
	"errors"

	"github.com/Don-Works/brw/internal/profilepolicy"
)

// Service is the roster as the daemon's /profiles routes drive it.
type Service struct {
	// BRWDPath is the brwd binary the start commands for a new profile name.
	BRWDPath string
}

// Board is LoadBoard.
func (Service) Board(ctx context.Context, policyPath string) (any, error) {
	return LoadBoard(ctx, policyPath)
}

// Create is Create for one named profile.
func (s Service) Create(policyPath, name, account, browserName string) (any, error) {
	return Create(CreateRequest{Name: name, Account: account, Browser: browserName, PolicyPath: policyPath, BRWDPath: s.BRWDPath})
}

// Copy is CopyDomain against the policy at policyPath.
func (Service) Copy(ctx context.Context, policyPath, from, to, domain, mode string) (any, error) {
	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return nil, err
	}
	return CopyDomain(ctx, policy, from, to, domain, mode)
}

// Pin is AddPin.
func (Service) Pin(policyPath, profile, origin, account, label string) error {
	return AddPin(policyPath, profile, origin, account, label)
}

// Open is Open against the policy at policyPath.
func (Service) Open(ctx context.Context, policyPath, profile, rawURL string) (any, error) {
	policy, err := profilepolicy.Load(policyPath)
	if err != nil {
		return nil, err
	}
	return Open(ctx, policy, profile, rawURL)
}

// Refused reports whether err is a refusal rather than a failure.
func (Service) Refused(err error) bool { return errors.Is(err, ErrRefused) }
