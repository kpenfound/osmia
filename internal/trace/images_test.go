package trace

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io/fs"
	"sync"
	"testing"
)

func imageBytes(t *testing.T, kind string) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var err error
	if kind == "image/png" {
		err = png.Encode(&b, img)
	} else {
		err = jpeg.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestImageRecords(t *testing.T) {
	t.Parallel()
	r, root, p := create(t)
	pngBytes, jpegBytes := imageBytes(t, "image/png"), imageBytes(t, "image/jpeg")
	if err := ValidateImage("image/png", append(bytes.Clone(pngBytes), make([]byte, MaxImageBytes-len(pngBytes))...)); err != nil {
		t.Fatalf("5 MiB boundary: %v", err)
	}
	h := Header{Project: p.ID, Workstream: streamID, At: at, Actor: owner, Cause: "image-test"}
	for _, c := range []struct {
		name, kind string
		data       []byte
	}{
		{"png", "image/png", pngBytes}, {"jpeg", "image/jpeg", jpegBytes}, {"svg", "image/svg+xml", []byte(`<svg/>`)}, {"html", "text/html", []byte(`<html/>`)}, {"text", "text/plain", []byte("hello")}, {"png-jpeg", "image/png", jpegBytes}, {"jpeg-png", "image/jpeg", pngBytes}, {"large", "image/png", append(pngBytes, make([]byte, MaxImageBytes)...)}} {
		t.Run(c.name, func(t *testing.T) {
			v, err := r.RecordImage(context.Background(), h, c.name, c.kind, c.data, "turn")
			valid := c.name == "png" || c.name == "jpeg"
			if (err == nil) != valid {
				t.Fatalf("record: %v", err)
			}
			if valid && (!bytes.Equal(v.Data, c.data) || v.ContentType != c.kind) {
				t.Fatal("bytes changed")
			}
		})
	}
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			v, err := r.RecordImage(context.Background(), h, "concurrent", "image/png", pngBytes, "turn")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- v.ID
		})
	}
	wg.Wait()
	close(ids)
	var id string
	for found := range ids {
		if id != "" && found != id {
			t.Fatal("duplicate identity")
		}
		id = found
	}
	retry, err := r.RecordImage(context.Background(), h, "concurrent", "invalid", nil, "other")
	if err != nil || retry.ID != id {
		t.Fatalf("retry: %v %v", retry, err)
	}
	images, err := Read[Image](r, streamID)
	if err != nil || len(images) != 3 {
		t.Fatalf("images %d: %v", len(images), err)
	}
	// Returning a record never exposes the cached byte slice to mutation.
	images[0].Data[0] = 0
	saved, err := r.ImageByKey(streamID, "png")
	if err != nil || !bytes.Equal(saved.Data, pngBytes) {
		t.Fatal("cached bytes mutated")
	}
	if _, err := r.ImageByKey(streamID, "unknown"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	mustClose := func() {
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
	mustClose()
	r, err = Open(root, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	saved, err = r.StoredImage(streamID, id)
	if err != nil || !bytes.Equal(saved.Data, pngBytes) {
		t.Fatalf("restart lost bytes: %v", err)
	}
	if _, err := r.MarkWorkstreamTraceForDeletion(streamID); err != nil {
		t.Fatal(err)
	}
	if err := FinishWorkstreamTraceDeletion(root, p.ID, streamID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.StoredImage(streamID, id); err == nil {
		t.Fatal("deleted image still served")
	}
}

func TestImageReferencesAreIdempotent(t *testing.T) {
	t.Parallel()
	r, _, p := create(t)
	h := Header{Project: p.ID, Workstream: streamID, At: at, Actor: owner, Cause: "test"}
	for range 2 {
		if _, err := r.PublishImageReference(context.Background(), h, streamID, "image-example", "turn"); err != nil {
			t.Fatal(err)
		}
	}
	refs, err := Read[ImageReference](r, streamID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("references %d: %v", len(refs), err)
	}
}
