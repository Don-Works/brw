package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ManifestSchemaVersion is the version of the failure-bundle manifest document.
const ManifestSchemaVersion = 1

// ManifestEntry points at one stored artifact. It carries identity and
// retention only: a manifest that inlined even a truncated payload would put
// the evidence back into the response it exists to keep it out of.
type ManifestEntry struct {
	Role       string    `json:"role"`
	ArtifactID string    `json:"artifact_id"`
	Kind       string    `json:"kind"`
	MIMEType   string    `json:"mime_type"`
	SizeBytes  int64     `json:"size_bytes"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Manifest is the whole of what a failed run returns: a list of artifact IDs an
// agent can read on demand, so diagnosing a failure costs one small response
// plus exactly the reads the agent decides to make.
type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	CreatedAt     time.Time       `json:"created_at"`
	Reason        string          `json:"reason"`
	RecipeID      string          `json:"recipe_id,omitempty"`
	RecipeVersion string          `json:"recipe_version,omitempty"`
	FailedStep    string          `json:"failed_step,omitempty"`
	Entries       []ManifestEntry `json:"entries"`
	// Missing names the parts that could not be collected, each with the class of
	// failure and never its detail. A bundle that silently omits the screenshot
	// tells the reader the page had none.
	Missing []MissingPart `json:"missing,omitempty"`
}

type MissingPart struct {
	Role   string `json:"role"`
	Reason string `json:"reason"`
}

const maxManifestReasonBytes = 500

// PutManifest stores a manifest as its own artifact. The manifest expires with
// the parts it names, so a handle can never outlive the evidence it points at.
func (s *Store) PutManifest(ctx context.Context, manifest Manifest, ttl time.Duration) (Meta, error) {
	if len(manifest.Entries) == 0 && len(manifest.Missing) == 0 {
		return Meta{}, errors.New("failure manifest has no entries")
	}
	manifest.SchemaVersion = ManifestSchemaVersion
	data, err := json.Marshal(manifest)
	if err != nil {
		return Meta{}, err
	}
	return s.PutContext(ctx, PutOptions{
		Kind: "manifest", MIMEType: "application/json", TTL: ttl,
		Redaction: "failure-bundle",
	}, bytes.NewReader(data))
}
