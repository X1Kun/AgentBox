package batch

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"os"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
)

var artifactName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}\.png$`)

func validArtifact(name string) bool { return artifactName.MatchString(name) }

type attemptResult struct {
	Version       int    `json:"schema_version"`
	DatasetSHA256 string `json:"dataset_sha256"`
	RequestSHA256 string `json:"request_sha256"`
	JobUID        string `json:"job_uid"`
	PodUID        string `json:"pod_uid"`
	Passes        int    `json:"passes"`
	Files         []File `json:"files"`
}

func verifyPNG(data []byte, file, expected File) error {
	if int64(len(data)) != file.Size || file.Size < 1 || file.Size > MaxFileBytes || hash(data) != file.SHA256 || file.Name != expected.Name || file.Width != expected.Width || file.Height != expected.Height || file.PixelSHA256 != expected.PixelSHA256 {
		return fail("artifact_invalid")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != expected.Width || config.Height != expected.Height {
		return fail("artifact_invalid")
	}
	image, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return fail("artifact_invalid")
	}
	pixels := make([]byte, 0, expected.Width*expected.Height*3)
	for y := 0; y < expected.Height; y++ {
		for x := 0; x < expected.Width; x++ {
			r, g, b, a := image.At(x, y).RGBA()
			if a != 65535 {
				return fail("artifact_invalid")
			}
			pixels = append(pixels, byte(r>>8), byte(g>>8), byte(b>>8))
		}
	}
	if hash(pixels) != expected.PixelSHA256 {
		return fail("artifact_invalid")
	}
	return nil
}
func (s *Service) publish(r *Record, pod *corev1.Pod) (resultErr error) {
	started := time.Now()
	defer func() {
		outcome := "complete"
		if resultErr != nil {
			outcome = "error"
			if Code(resultErr) == "artifact_invalid" {
				outcome = "invalid"
			}
		}
		s.metrics.Operation("publication", outcome, time.Since(started))
	}()
	if !uid.MatchString(r.JobUID) || !uid.MatchString(string(pod.UID)) {
		return fail("artifact_invalid")
	}
	attempt, err := s.store.root.OpenRoot("attempts/" + r.JobUID + "/" + string(pod.UID))
	if err != nil {
		return fail("artifact_invalid")
	}
	defer attempt.Close()
	data, err := boundedFile(attempt, "result.json", 64<<10)
	if err != nil {
		return fail("artifact_invalid")
	}
	var result attemptResult
	if decodeOne(data, &result) != nil || result.Version != 1 || result.DatasetSHA256 != s.contract.DatasetSHA256 || result.RequestSHA256 != r.Digest || result.JobUID != r.JobUID || result.PodUID != string(pod.UID) || result.Passes != r.Request.Passes || len(result.Files) != len(s.contract.Outputs) {
		return fail("artifact_invalid")
	}
	expected := map[string]File{}
	for _, f := range s.contract.Outputs {
		expected[f.Name] = f
	}
	seen := map[string]bool{}
	contents := map[string][]byte{}
	total := int64(0)
	for _, file := range result.Files {
		want, ok := expected[file.Name]
		if !ok || seen[file.Name] || !validArtifact(file.Name) {
			return fail("artifact_invalid")
		}
		seen[file.Name] = true
		payload, e := boundedFile(attempt, file.Name, MaxFileBytes)
		if e != nil {
			return fail("artifact_invalid")
		}
		if e = verifyPNG(payload, file, want); e != nil {
			return e
		}
		total += int64(len(payload))
		if total > MaxOutputBytes {
			return fail("artifact_invalid")
		}
		contents[file.Name] = payload
	}
	// Publish as one immutable directory. A crash after rename is recovered by
	// validating the exact same attempt, then committing the durable record.
	temp := "published/" + r.Name + ".pending"
	target := "published/" + r.Name
	if err = s.store.root.MkdirAll(temp, 0700); err != nil {
		return err
	}
	for name, payload := range contents {
		if err = atomicWrite(s.store.root, temp+"/"+name, payload); err != nil {
			return err
		}
	}
	if err = s.store.root.Rename(temp, target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			if _, e := s.store.root.Stat(target); e != nil {
				return err
			}
		}
		published, e := s.store.root.OpenRoot(target)
		if e != nil {
			return e
		}
		for name, payload := range contents {
			old, e := boundedFile(published, name, MaxFileBytes)
			if e != nil || !bytes.Equal(old, payload) {
				published.Close()
				return fail("artifact_invalid")
			}
		}
		published.Close()
		_ = s.store.root.RemoveAll(temp)
	}
	d, err := s.store.root.Open("published")
	if err != nil {
		return err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return err
	}
	r.Artifacts = result.Files
	r.PodUID = string(pod.UID)
	return nil
}
func (s *Service) Download(ctx context.Context, principal, id, name string) (payload []byte, resultErr error) {
	started := time.Now()
	defer func() {
		outcome := "complete"
		if resultErr != nil {
			outcome = "error"
		}
		s.metrics.Operation("artifact_read", outcome, time.Since(started))
	}()
	if !validArtifact(name) {
		return nil, fail("not_found")
	}
	r, unlock, err := s.lookup(ctx, principal, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if r.State != "succeeded" {
		return nil, fail("artifact_not_ready")
	}
	expires, err := time.Parse(time.RFC3339, r.ExpiresAt)
	if err != nil {
		return nil, fail("record_invalid")
	}
	if !s.now().Before(expires) {
		return nil, fail("artifact_expired")
	}
	var file, want *File
	for i := range r.Artifacts {
		if r.Artifacts[i].Name == name {
			file = &r.Artifacts[i]
		}
	}
	for i := range s.contract.Outputs {
		if s.contract.Outputs[i].Name == name {
			want = &s.contract.Outputs[i]
		}
	}
	if file == nil || want == nil {
		return nil, fail("not_found")
	}
	published, err := s.store.root.OpenRoot("published/" + r.Name)
	if err != nil {
		return nil, err
	}
	defer published.Close()
	data, err := boundedFile(published, name, MaxFileBytes)
	if err != nil {
		return nil, err
	}
	if err = verifyPNG(data, *file, *want); err != nil {
		return nil, err
	}
	return data, nil
}
func (s *Service) expire(ctx context.Context, r *Record) error {
	if !s.gone(ctx, r) {
		return nil
	}
	if err := s.store.root.RemoveAll("published/" + r.Name + ".pending"); err != nil {
		return err
	}
	if r.State != "succeeded" {
		if err := s.store.root.RemoveAll("published/" + r.Name); err != nil {
			return err
		}
	}
	if uid.MatchString(r.JobUID) {
		if err := s.store.root.RemoveAll("attempts/" + r.JobUID); err != nil {
			return err
		}
	}
	if r.State == "succeeded" {
		expiry, err := time.Parse(time.RFC3339, r.ExpiresAt)
		if err != nil {
			return fail("record_invalid")
		}
		if !s.now().Before(expiry) {
			return s.store.root.RemoveAll("published/" + r.Name)
		}
	}
	return nil
}
