package trace

import "testing"

func TestMemberResolutionUsesCurrentDocumentRevisions(t *testing.T) {
	docs := []Document{
		{Header: Header{ID: "final-publication", Revision: 2}, Content: `{"status":"prepared"}`},
		{Header: Header{ID: "final-publication", Revision: 1}, Content: `{"status":"opened","pull_request":42}`},
		{Header: Header{ID: "own-pull-request-outcome", Revision: 1}, Content: `{"pull_request":42,"outcome":"merged"}`},
	}
	evidence, err := MemberResolutionFrom(docs)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Resolution("delivered") != "unresolved" {
		t.Fatal("an earlier publication retired the member")
	}
	docs[0].Content = `{"status":"opened","pull_request":43}`
	evidence, err = MemberResolutionFrom(docs)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Resolution("delivered") != "unresolved" {
		t.Fatal("an earlier pull request retired the member")
	}
	docs = append(docs, Document{Header: Header{ID: "own-pull-request-outcome", Revision: 2}, Content: `{"pull_request":43,"outcome":"merged"}`})
	evidence, err = MemberResolutionFrom(docs)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Resolution("delivered") != "merged" {
		t.Fatal("the current pull request's merge did not resolve the member")
	}
}
