package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kpenfound/osmia/internal/config"
	"github.com/kpenfound/osmia/internal/trace"
)

// credentialWaitMarker is the phrase every MissingCredentialError carries, so
// the status of a workstream can recognise the wait in an operation's
// recorded failure.
const credentialWaitMarker = "waits for a GitHub credential"

// MissingCredentialError is the error a final review's operation retries on
// while the service holds no GitHub pull request credential: nothing is
// pushed and no pull request is opened or read, the workstream stays
// assembled, and the final read does not start. A restart with GITHUB_TOKEN
// set retries the operation through the publication it was waiting for.
type MissingCredentialError struct {
	Review int
	Action string
}

func (e *MissingCredentialError) Error() string {
	return fmt.Sprintf("final review %d %s: the service was started without GITHUB_TOKEN, so it cannot %s; restart osmia serve with GITHUB_TOKEN set and the review continues", e.Review, credentialWaitMarker, e.Action)
}

// credentialNoticeSubject is the workflow subject that carries the one chief
// of staff notice sent for a final review waiting for a credential.
func credentialNoticeSubject(review int) string {
	return fmt.Sprintf("final-review-%d-credential", review)
}

// missingCredential returns the MissingCredentialError of report's review
// after telling the chief of staff once, so the missing credential shows on
// the workstream, not only in the operation's retry record. A retry and a
// restart find the notice recorded and send nothing.
func (a *FinalReviewer) missingCredential(ctx context.Context, stream config.WorkstreamID, report FinalReport, action string) error {
	err := &MissingCredentialError{Review: report.Review, Action: action}
	subject := credentialNoticeSubject(report.Review)
	state, serr := a.Repository.Workflow(stream, subject)
	if serr != nil {
		return errors.Join(err, serr)
	}
	if state.Value != "" {
		return err
	}
	id := subject + "-notice"
	body := fmt.Sprintf("Final review %d of the assembled workstream waits for a GitHub credential. The service was started without GITHUB_TOKEN, so it has not pushed %s or opened the draft pull request, and the final read has not started. The workstream stays assembled. Ask the owner to restart osmia serve with GITHUB_TOKEN set; the review then pushes the commit, opens the draft and watches its checks as usual.", report.Review, report.Commit)
	h := trace.Header{Schema: "osmia.trace.transition", Version: trace.Version, ID: id, Revision: 1, Project: a.Repository.Project(), Workstream: stream, At: a.S.Now(), Actor: FinalReviewActor, Cause: report.Operation}
	tx := trace.Transaction{ExpectedVersion: state.Version,
		Transition: trace.Transition{Header: h, Subject: subject, From: state.Value, To: "noticed", Reason: "the service holds no GitHub credential"},
		Events:     []trace.Event{trace.Notice(id, "chief", body)}}
	if _, terr := a.Repository.Transact(ctx, tx); terr != nil && !errors.Is(terr, trace.ErrConflict) {
		return errors.Join(err, terr)
	}
	return err
}

// CredentialWait returns, in plain words, why the workstream's final review
// waits for a GitHub credential, or "" when it does not. It reads the
// failure the review's pending operation recorded on its latest attempt.
func CredentialWait(repository *trace.Repository, stream config.WorkstreamID) (string, error) {
	records, err := repository.Operations(stream)
	if err != nil {
		return "", err
	}
	for _, record := range records {
		if record.Acknowledged || record.Result != nil || record.Operation.Action != FinalReviewAction || len(record.History) == 0 {
			continue
		}
		last := record.History[len(record.History)-1]
		if strings.Contains(last.Failure, credentialWaitMarker) {
			return "waiting on owner: " + last.Failure, nil
		}
	}
	return "", nil
}
