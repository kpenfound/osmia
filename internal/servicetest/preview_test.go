package servicetest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

type previewImage struct {
	*PreviewJourney
	attachment api.FeedImage
}

func newCapturedPreview(t *testing.T) previewImage {
	t.Helper()
	f := NewPreviewJourney(t)
	return previewImage{PreviewJourney: f, attachment: f.Capture(t)}
}

func (f previewImage) groupFeed(t *testing.T) api.WorkgroupFeedResponse {
	t.Helper()
	var out api.WorkgroupFeedResponse
	fixturekit.Must(t, f.C.Do(context.Background(), http.MethodGet, api.Prefix+"/workgroups/"+string(f.Group.ID)+"/feed", nil, &out))
	return out
}

func (f previewImage) assertGroupImage(t *testing.T, removed bool) {
	t.Helper()
	n := 0
	for _, entry := range f.groupFeed(t).Entries {
		if entry.Kind != "image" {
			continue
		}
		n++
		if entry.Workstream != f.Member || entry.Repo != f.ID || entry.Image == nil || entry.Image.ID != f.attachment.ID {
			t.Fatalf("shared image attribution: %+v", entry)
		}
		if removed {
			if entry.Image.URL != "" || entry.Image.Placeholder != "image removed when "+string(f.Member)+" was archived" {
				t.Fatalf("archived image: %+v", entry.Image)
			}
		} else if *entry.Image != f.attachment {
			t.Fatalf("shared and member images differ: %+v %+v", entry.Image, f.attachment)
		}
	}
	if n != 1 {
		t.Fatalf("shared image count: %d", n)
	}
}

func TestManagerPreviewImageIsServedAndSharedWithTheWorkgroup(t *testing.T) {
	f := newCapturedPreview(t)
	f.assertGroupImage(t, false)
	response, err := http.Get("http://" + f.S.WebAddr() + f.attachment.URL)
	fixturekit.Must(t, err)
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	fixturekit.Must(t, err)
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/png" || response.Header.Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(data, f.PNG) {
		t.Fatalf("served image: %d %v", response.StatusCode, response.Header)
	}
	repo := f.Repository(t)
	images, err := trace.Read[trace.Image](repo, f.Member)
	fixturekit.Must(t, err)
	refs, err := trace.Read[trace.ImageReference](repo, f.Sibling)
	fixturekit.Must(t, err)
	if len(images) != 1 || !bytes.Equal(images[0].Data, f.PNG) || len(refs) != 1 || refs[0].Member != f.Member || refs[0].ImageID != f.attachment.ID {
		t.Fatalf("member receipt and shared reference: images=%d refs=%+v", len(images), refs)
	}
	runs, err := os.ReadFile(f.CLIs.Runs)
	fixturekit.Must(t, err)
	if strings.Count(string(runs), "capture\n") != 1 {
		t.Fatalf("capture count: %q", runs)
	}
}

func TestArchivingAMemberRemovesItsPreviewImageFromEveryRecord(t *testing.T) {
	f := newCapturedPreview(t)
	repo := f.Repository(t)
	f.ArchiveMember(t)
	f.assertGroupImage(t, true)
	wait, release := ServiceChanges(t, f.S)
	defer release()
	fixturekit.Until(t, "the archived member's trace cleanup", func() error { wait(); return nil }, func() bool {
		state, _ := service.InspectForTest(f.S).LifecycleController().Store().Snapshot()
		for _, archive := range state.Archived {
			if archive.Workstream == f.Member && !archive.CleanedAt.IsZero() {
				return true
			}
		}
		return false
	})
	root := service.InspectForTest(f.S).Config().Root
	gone, err := trace.WorkstreamTraceGone(root, f.ID, f.Member)
	fixturekit.Must(t, err)
	if !gone {
		t.Fatal("archived image trace remains on disk")
	}
	if _, err := repo.StoredImage(f.Member, f.attachment.ID); err == nil {
		t.Fatal("archive retained image bytes")
	}
	response, err := http.Get("http://" + f.S.WebAddr() + f.attachment.URL)
	fixturekit.Must(t, err)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("archived endpoint: %d", response.StatusCode)
	}
	streams, err := repo.Workstreams()
	fixturekit.Must(t, err)
	for _, stream := range streams {
		images, err := trace.Read[trace.Image](repo, stream)
		fixturekit.Must(t, err)
		if len(images) != 0 {
			t.Fatalf("image bytes copied into trace %s", stream)
		}
		docs, err := trace.Read[trace.Document](repo, stream)
		fixturekit.Must(t, err)
		assertNoPreviewBytes(t, f.PNG, docs)
	}
	state, _ := service.InspectForTest(f.S).LifecycleController().Store().Snapshot()
	assertNoPreviewBytes(t, f.PNG, state.Archived)
	assertNoPreviewBytes(t, f.PNG, f.groupFeed(t))
}

func assertNoPreviewBytes(t *testing.T, png []byte, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	fixturekit.Must(t, err)
	if bytes.Contains(data, png) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(png))) {
		t.Fatal("image bytes in retained metadata")
	}
}
