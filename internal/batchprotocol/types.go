// Package batchprotocol defines the fixed image API wire contract without Kubernetes or storage dependencies.
package batchprotocol

import "errors"

const MaxFileBytes = 256 << 10
const MaxOutputBytes = 2 << 20

type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeBackendUnavailable
}

type Request struct {
	RequestID string `json:"request_id"`
	DatasetID string `json:"dataset_id"`
	Passes    int    `json:"passes,omitempty"`
}
type File struct {
	Name        string `json:"name"`
	Size        int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	PixelSHA256 string `json:"pixel_sha256"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}
type Contract struct {
	Version       int    `json:"schema_version"`
	DatasetID     string `json:"dataset_id"`
	DatasetSHA256 string `json:"dataset_sha256"`
	Outputs       []File `json:"outputs"`
}
type View struct {
	ID                string `json:"batch_id"`
	State             State  `json:"state"`
	Reason            string `json:"reason,omitempty"`
	CleanupComplete   bool   `json:"cleanup_complete"`
	ArtifactExpiresAt string `json:"artifact_expires_at,omitempty"`
	Artifacts         []File `json:"artifacts,omitempty"`
}
