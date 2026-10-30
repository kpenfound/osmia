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
	"time"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/service"
	"github.com/kpenfound/osmia/internal/servicetest/fixturekit"
	"github.com/kpenfound/osmia/internal/trace"
)

func TestChiefPreviewFromConfigurationThroughMemberArchive(t *testing.T) {
	f := NewPreviewJourney(t)
	attachment := f.Capture(t)
	ctx := context.Background()
	groupFeed := func() api.WorkgroupFeedResponse {
		t.Helper()
		var out api.WorkgroupFeedResponse
		fixturekit.Must(t, f.C.Do(ctx, http.MethodGet, api.Prefix+"/workgroups/"+string(f.Group.ID)+"/feed", nil, &out))
		return out
	}
	assertGroupImage := func(removed bool) {
		t.Helper()
		n := 0
		for _, entry := range groupFeed().Entries {
			if entry.Kind != "image" {
				continue
			}
			n++
			if entry.Workstream != f.Member || entry.Repo != f.ID || entry.Image == nil || entry.Image.ID != attachment.ID {
				t.Fatalf("shared image attribution: %+v", entry)
			}
			if removed {
				if entry.Image.URL != "" || entry.Image.Placeholder != "image removed when "+string(f.Member)+" was archived" {
					t.Fatalf("archived image: %+v", entry.Image)
				}
			} else if *entry.Image != attachment {
				t.Fatalf("shared and member images differ: %+v %+v", entry.Image, attachment)
			}
		}
		if n != 1 {
			t.Fatalf("shared image count: %d", n)
		}
	}
	assertGroupImage(false)
	url := "http://" + f.S.WebAddr() + attachment.URL
	response, err := http.Get(url)
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
	if len(images) != 1 || !bytes.Equal(images[0].Data, f.PNG) || len(refs) != 1 || refs[0].Member != f.Member || refs[0].ImageID != attachment.ID {
		t.Fatalf("member receipt and shared reference: images=%d refs=%+v", len(images), refs)
	}
	f.ArchiveMember(t)
	assertGroupImage(true)
	// Wait for production cleanup, including its durable completion receipt.
	wait, release := ServiceChanges(t, f.S)
	defer release()
	deadline := time.Now().Add(fixturekit.DemoTimeout)
	for {
		state, _ := service.InspectForTest(f.S).LifecycleController().Store().Snapshot()
		clean := false
		for _, archive := range state.Archived {
			if archive.Workstream == f.Member && !archive.CleanedAt.IsZero() {
				clean = true
			}
		}
		if clean {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("member trace cleanup did not complete: %+v", state.Archived)
		}
		wait()
	}
	root := service.InspectForTest(f.S).Config().Root
	gone, err := trace.WorkstreamTraceGone(root, f.ID, f.Member)
	fixturekit.Must(t, err)
	if !gone {
		t.Fatal("archived image trace remains on disk")
	}
	if _, err := repo.StoredImage(f.Member, attachment.ID); err == nil {
		t.Fatal("archive retained image bytes")
	}
	response, err = http.Get(url)
	fixturekit.Must(t, err)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("archived endpoint: %d", response.StatusCode)
	}
	assertGroupImage(true)
	// Remaining traces and the pinned archive presentation carry metadata only.
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
	assertNoPreviewBytes(t, f.PNG, groupFeed())
	runs, err := os.ReadFile(f.CLIs.Runs)
	fixturekit.Must(t, err)
	if strings.Count(string(runs), "capture\n") != 1 {
		t.Fatalf("capture count: %q", runs)
	}
}

func assertNoPreviewBytes(t *testing.T, png []byte, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	fixturekit.Must(t, err)
	if bytes.Contains(data, png) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(png))) {
		t.Fatal("image bytes in retained metadata")
	}
}
