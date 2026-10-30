package service

import (
	"net/http"
	"strings"

	"github.com/kpenfound/osmia/internal/api"
	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

func imageURL(stream config.WorkstreamID, id string) string {
	return api.Prefix + "/images/" + string(stream) + "/" + id
}

func imageFeedEntry(v trace.Image) FeedEntry {
	return FeedEntry{Kind: "image", At: v.At, Turn: v.Turn, Image: &api.FeedImage{ID: v.ID, URL: imageURL(v.Workstream, v.ID), ContentType: v.ContentType}}
}

func memberImageEntries(repository *trace.Repository, stream config.WorkstreamID) ([]FeedEntry, error) {
	images, err := trace.Read[trace.Image](repository, stream)
	if err != nil {
		return nil, err
	}
	out := make([]FeedEntry, 0, len(images))
	for _, v := range images {
		out = append(out, imageFeedEntry(v))
	}
	return out, nil
}

// imageRequest uses the same handler and listener wrappers as every read endpoint.
func (s *Service) imageRequest(w http.ResponseWriter, r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, Prefix+"/images/")
	if !ok || r.Method != http.MethodGet {
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	raw, id, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(id, "/") {
		fail(w, NotFound)
		return true
	}
	_, stream, repository, apiErr := s.conversationTrace(raw)
	if apiErr != nil {
		fail(w, NotFound)
		return true
	}
	v, err := repository.StoredImage(stream, id)
	if err != nil {
		fail(w, NotFound)
		return true
	}
	w.Header().Set("Content-Type", v.ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(v.Data)
	return true
}
