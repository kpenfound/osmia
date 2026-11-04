package trace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io/fs"
	"path/filepath"

	"github.com/kpenfound/osmia/internal/config"
)

const MaxImageBytes = 5 * 1024 * 1024

// Image holds an immutable attachment in the shown member's or session's trace.
// Key belongs to the caller and makes retries return the original attachment.
type Image struct {
	Header
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
	Turn        string `json:"turn,omitempty"`
}

func (Image) traceRecord() {}

// ImageReference holds no bytes: the shared conversation resolves its member's image.
type ImageReference struct {
	Header
	Member  config.WorkstreamID `json:"member"`
	ImageID string              `json:"image_id"`
	Turn    string              `json:"turn,omitempty"`
}

func (ImageReference) traceRecord() {}

// ValidateImage accepts PNG and JPEG only, with matching signatures and a readable image header.
func ValidateImage(contentType string, data []byte) error {
	if len(data) == 0 || len(data) > MaxImageBytes {
		return fmt.Errorf("image must contain at most 5 MiB")
	}
	valid := contentType == "image/png" && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) || contentType == "image/jpeg" && bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff})
	if !valid {
		return fmt.Errorf("only PNG and JPEG whose bytes match the content type are accepted")
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || contentType != "image/"+format {
		return fmt.Errorf("invalid %s image", contentType)
	}
	return nil
}

func imageIdentity(key string) string { return fmt.Sprintf("image-%x", sha256.Sum256([]byte(key))) }

// RecordImage records once under key. A repeated key returns the original image,
// even when the retry supplies different bytes. It never writes into a workgroup trace.
func (r *Repository) RecordImage(ctx context.Context, h Header, key, contentType string, data []byte, turn string) (Image, error) {
	target, err := r.scoped(h.Workstream)
	if err != nil {
		return Image{}, err
	}
	if !target.imageStorageScope() {
		return Image{}, fmt.Errorf("images belong only to member or session traces")
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if key == "" {
		return Image{}, fmt.Errorf("image key is required")
	}
	h.Schema, h.Version, h.ID, h.Revision = "osmia.trace.image", Version, imageIdentity(key), 1
	records, err := target.records(func(v Record) bool { _, ok := v.(Image); return ok && v.header().ID == h.ID })
	if err != nil {
		return Image{}, err
	}
	if len(records) > 0 {
		return records[0].(Image), nil
	}
	v := Image{Header: h, Key: key, ContentType: contentType, Data: bytes.Clone(data), Turn: turn}
	if err := target.append(ctx, v); err != nil {
		return Image{}, err
	}
	return v, nil
}

// ImageByKey permits durable callers to reconcile an interrupted publication before capturing again.
func (r *Repository) ImageByKey(stream config.WorkstreamID, key string) (Image, error) {
	return Get[Image](r, stream, imageIdentity(key), 1)
}

// PublishImageReference is idempotent for a member and image ID. h names the
// shared chief's host; the caller retains responsibility for reconciling effects.
func (r *Repository) PublishImageReference(ctx context.Context, h Header, member config.WorkstreamID, imageID, turn string) (ImageReference, error) {
	target, err := r.scoped(h.Workstream)
	if err != nil {
		return ImageReference{}, err
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	h.Schema, h.Version, h.ID, h.Revision = "osmia.trace.image-reference", Version, imageIdentity(string(member)+"/"+imageID), 1
	records, err := target.records(func(v Record) bool { _, ok := v.(ImageReference); return ok && v.header().ID == h.ID })
	if err != nil {
		return ImageReference{}, err
	}
	if len(records) > 0 {
		return records[0].(ImageReference), nil
	}
	v := ImageReference{Header: h, Member: member, ImageID: imageID, Turn: turn}
	if err := target.append(ctx, v); err != nil {
		return ImageReference{}, err
	}
	return v, nil
}

// StoredImage returns only a live attachment; deletion removes the owning trace.
func (r *Repository) StoredImage(stream config.WorkstreamID, id string) (Image, error) {
	if !key(id) {
		return Image{}, fs.ErrNotExist
	}
	return Get[Image](r, stream, id, 1)
}

// imageStorageScope accepts only the dedicated trace of a registered project member.
func (r *Repository) imageStorageScope() bool {
	if r.stream == "" || r.shared != nil || r.parent != nil && r.parent.shared != nil {
		return false
	}
	directory, err := r.root.WorkstreamTrace(r.project, r.stream)
	return err == nil && filepath.Clean(directory) == filepath.Clean(r.directory)
}
