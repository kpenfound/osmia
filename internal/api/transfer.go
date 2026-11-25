package api

import (
	"strings"

	"github.com/kpenfound/osmia/internal/config"
)

// The content types of the two routes the tailnet listener accepts without a
// JSON body: the read-only git fetch and the upload of a finished view.
const (
	GitUploadPackRequestType = "application/x-git-upload-pack-request"
	GitUploadPackResultType  = "application/x-git-upload-pack-result"
	GitAdvertisementType     = "application/x-git-upload-pack-advertisement"
	ViewArchiveType          = "application/x-osmia-view"
)

// GitService is the git service the read-only route serves.
const GitService = "git-upload-pack"

// GitPath is the URL path of the git route for project, which a git client
// takes as the base of its remote URL.
func GitPath(project config.ProjectID) string {
	return Prefix + "/git/" + string(project)
}

// GitRoute splits a request path under the git route into the project it
// names and the path below it, such as info/refs or git-upload-pack. Either may
// be empty.
func GitRoute(path string) (project config.ProjectID, rest string, ok bool) {
	tail, ok := strings.CutPrefix(path, Prefix+"/git/")
	if !ok {
		return "", "", false
	}
	id, rest, _ := strings.Cut(tail, "/")
	return config.ProjectID(id), rest, true
}

// GitFetchPath reports whether path is the route a git client posts a fetch
// request to.
func GitFetchPath(path string) bool {
	_, rest, ok := GitRoute(path)
	return ok && rest == GitService
}

// ViewPath is the path of a turn's view archive: the download of a view built
// from trace documents and the upload of the finished view.
func ViewPath(turn string) string {
	return Prefix + "/turns/" + turn + "/view"
}

// ViewRoute returns the turn a view archive path names.
func ViewRoute(path string) (turn string, ok bool) {
	tail, ok := strings.CutPrefix(path, Prefix+"/turns/")
	if !ok {
		return "", false
	}
	turn, rest, ok := strings.Cut(tail, "/")
	if !ok || turn == "" || rest != "view" {
		return "", false
	}
	return turn, true
}

// ViewAnswer is the definitive answer to an uploaded view.
type ViewAnswer string

const (
	// ViewAccepted means the view was taken for its turn.
	ViewAccepted ViewAnswer = "accepted"
	// ViewAlreadyRecorded means the turn's view was recorded before.
	ViewAlreadyRecorded ViewAnswer = "already_recorded"
	// ViewSuperseded means the turn's recovery began first, so the view is not
	// used.
	ViewSuperseded ViewAnswer = "superseded"
)

// ViewUploadResponse reports what became of an uploaded view.
type ViewUploadResponse struct {
	Answer ViewAnswer `json:"answer" enum:"accepted,already_recorded,superseded"`
}
