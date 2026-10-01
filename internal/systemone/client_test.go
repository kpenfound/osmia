package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ticket() Request {
	return Request{
		State: "Help! My payouts have been failing for 3 days.",
		Questions: map[string]Question{
			"department": Choice("Which team should handle this?",
				Option{Name: "billing", Description: "Payments, invoicing, refunds"},
				Option{Name: "technical", Description: "Bugs, outages, integrations"},
				Option{Name: "sales"},
			),
			"urgent":      Noul("Does this convey urgency?"),
			"frustration": Score("How frustrated is the customer?", "Calm", "Frustrated", "Very angry"),
		},
	}
}

const answered = `{
  "model": "jev-1.13.0",
  "answers": {
    "department": {"type": "choice", "choice": "billing", "probabilities": {"billing": 0.9, "technical": 0.08, "sales": 0.02}, "confidence": 0.85},
    "urgent": {"type": "noul", "noul": 0.95},
    "frustration": {"type": "score", "score": 1.2, "legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"}, "probabilities": {"0": 0.0, "1": 0.8, "2": 0.2}, "confidence": 0.7}
  },
  "usage": {"input_tokens": 310, "output_tokens": 30}
}`

func TestQuestionsUseTheProviderWireForm(t *testing.T) {
	body, err := ticket().Body("jev-latest")
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Model     string                     `json:"model"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.Model != "jev-latest" {
		t.Fatalf("body = %s, %v", body, err)
	}
	if got := string(wire.Questions["department"]); got != `{"type":"choice","instructions":"Which team should handle this?","criteria":{"billing":"Payments, invoicing, refunds","technical":"Bugs, outages, integrations","sales":null}}` {
		t.Fatalf("choice = %s; options must keep their order", got)
	}
	if got := string(wire.Questions["urgent"]); got != `{"type":"noul","instructions":"Does this convey urgency?"}` {
		t.Fatalf("noul = %s", got)
	}
	if got := string(wire.Questions["frustration"]); got != `{"type":"score","instructions":"How frustrated is the customer?","criteria":["Calm","Frustrated","Very angry"]}` {
		t.Fatalf("score = %s", got)
	}
	q := Noul("Is this urgent?")
	q.Yes, q.No = "Time-sensitive", "No urgency"
	data, _ := json.Marshal(q)
	if string(data) != `{"type":"noul","instructions":"Is this urgent?","criteria":{"false":"No urgency","true":"Time-sensitive"}}` {
		t.Fatalf("noul criteria = %s", data)
	}
}

func TestRequestsOutsideTheProviderLimitsAreRefusedBeforeSending(t *testing.T) {
	many := make([]Option, MaxChoiceOptions+1)
	for i := range many {
		many[i] = Option{Name: strings.Repeat("o", i+1)}
	}
	for name, r := range map[string]Request{
		"no state":       {Questions: ticket().Questions},
		"no questions":   {State: "x"},
		"empty id":       {State: "x", Questions: map[string]Question{"": Noul("q")}},
		"one option":     {State: "x", Questions: map[string]Question{"q": Choice("q", Option{Name: "a"})}},
		"duplicate":      {State: "x", Questions: map[string]Question{"q": Choice("q", Option{Name: "a"}, Option{Name: "a"})}},
		"too many":       {State: "x", Questions: map[string]Question{"q": Choice("q", many...)}},
		"one level":      {State: "x", Questions: map[string]Question{"q": Score("q", "only")}},
		"no instruction": {State: "x", Questions: map[string]Question{"q": Noul("")}},
		"mixed criteria": {State: "x", Questions: map[string]Question{"q": {Kind: KindNoul, Instructions: "q", Levels: []any{"a", "b"}}}},
	} {
		sent := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent = true }))
		_, err := Client{BaseURL: server.URL, Model: "m", APIKey: "k"}.Evaluate(context.Background(), r)
		server.Close()
		var e *Error
		if !errors.As(err, &e) || e.Kind != KindInvalidRequest || sent {
			t.Errorf("%s: err = %v, sent = %v", name, err, sent)
		}
	}
}

func TestClientSendsTheRequestAndValidatesTheAnswers(t *testing.T) {
	var got struct {
		path, auth string
		body       []byte
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		got.body, _ = io.ReadAll(r.Body)
		io.WriteString(w, answered)
	}))
	defer server.Close()
	resp, err := Client{BaseURL: server.URL + "/api/", Model: "~typesafe/jev-latest", APIKey: "secret"}.Evaluate(context.Background(), ticket())
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/v1/systemone" || got.auth != "Bearer secret" || !strings.Contains(string(got.body), `"model":"~typesafe/jev-latest"`) {
		t.Fatalf("request = %+v", got)
	}
	if resp.Model != "jev-1.13.0" || resp.Answers["department"].Choice != "billing" || resp.Answers["department"].Confidence != 0.85 || resp.Answers["urgent"].Noul != 0.95 || resp.Answers["frustration"].Score != 1.2 || resp.Usage.InputTokens != 310 || resp.Usage.CostKnown {
		t.Fatalf("response = %+v", resp)
	}
}

func TestClientReportsAProviderCostWhenGiven(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Replace(answered, `"output_tokens": 30`, `"output_tokens": 30, "cost": 0.000013`, 1))
	}))
	defer server.Close()
	resp, err := Client{BaseURL: server.URL, Model: "m", APIKey: "k"}.Evaluate(context.Background(), ticket())
	if err != nil || !resp.Usage.CostKnown || resp.Usage.CostUSD != 0.000013 {
		t.Fatalf("usage = %+v, %v", resp.Usage, err)
	}
}

func TestClientClassifiesFailuresWithoutLeakingTheKey(t *testing.T) {
	cases := []struct {
		name   string
		handle func(http.ResponseWriter, *http.Request)
		want   ErrorKind
		after  time.Duration
	}{
		{"rate limited", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		}, KindRateLimited, 7 * time.Second},
		{"overloaded", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(529) }, KindRateLimited, 0},
		{"server error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }, KindUnavailable, 0},
		{"unauthorized", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, KindUnauthorized, 0},
		{"invalid", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnprocessableEntity) }, KindRejected, 0},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://elsewhere.example/v1/systemone", http.StatusTemporaryRedirect)
		}, KindRejected, 0},
		{"not json", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>") }, KindMalformed, 0},
		{"missing answer", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"model":"jev-1.13.0","answers":{"urgent":{"type":"noul","noul":0.1}},"usage":{"input_tokens":1}}`)
		}, KindMalformed, 0},
		{"too large", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, strings.Repeat(" ", maxResponseBytes+1))
		}, KindMalformed, 0},
		{"slow", func(w http.ResponseWriter, r *http.Request) {
			// A drained body lets the server notice the client leaving.
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		}, KindTimeout, 0},
	}
	for _, c := range cases {
		server := httptest.NewServer(http.HandlerFunc(c.handle))
		timeout := 10 * time.Second
		if c.want == KindTimeout {
			timeout = 50 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		_, err := Client{BaseURL: server.URL, Model: "m", APIKey: "the-secret-key"}.Evaluate(ctx, ticket())
		cancel()
		server.Close()
		var e *Error
		if !errors.As(err, &e) || e.Kind != c.want || e.RetryAfter != c.after || strings.Contains(err.Error(), "the-secret-key") {
			t.Errorf("%s: err = %#v", c.name, err)
		}
	}
}

func TestClientWithoutAKeySendsNothing(t *testing.T) {
	sent := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent = true }))
	defer server.Close()
	_, err := Client{BaseURL: server.URL, Model: "m"}.Evaluate(context.Background(), ticket())
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindInvalidRequest || sent {
		t.Fatalf("err = %v, sent = %v", err, sent)
	}
}

func TestAnswersMustMatchTheirQuestions(t *testing.T) {
	r := ticket()
	for name, edit := range map[string]func(map[string]any){
		"wrong type":      func(a map[string]any) { a["urgent"] = map[string]any{"type": "choice", "noul": 0.1} },
		"noul range":      func(a map[string]any) { a["urgent"] = map[string]any{"type": "noul", "noul": 1.5} },
		"unknown option":  func(a map[string]any) { a["department"].(map[string]any)["choice"] = "legal" },
		"not most likely": func(a map[string]any) { a["department"].(map[string]any)["choice"] = "sales" },
		"missing option": func(a map[string]any) {
			delete(a["department"].(map[string]any)["probabilities"].(map[string]any), "sales")
		},
		"no confidence": func(a map[string]any) { delete(a["department"].(map[string]any), "confidence") },
		"bad sum": func(a map[string]any) {
			a["department"].(map[string]any)["probabilities"].(map[string]any)["sales"] = 0.5
		},
		"score off scale":  func(a map[string]any) { a["frustration"].(map[string]any)["score"] = 3.0 },
		"extra answer":     func(a map[string]any) { a["other"] = map[string]any{"type": "noul", "noul": 0.1} },
		"missing question": func(a map[string]any) { delete(a, "frustration") },
	} {
		var wire map[string]any
		json.Unmarshal([]byte(answered), &wire)
		edit(wire["answers"].(map[string]any))
		data, _ := json.Marshal(wire)
		if _, err := decode(data, r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := decode([]byte(answered), r); err != nil {
		t.Fatalf("valid response refused: %v", err)
	}
	if _, err := decode([]byte(strings.Replace(answered, `"usage": {"input_tokens": 310, "output_tokens": 30}`, `"usage": {}`, 1)), r); err == nil {
		t.Fatal("response without usage accepted")
	}
}

func TestRetryAfterAcceptsSecondsAndDates(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if d := retryAfter("12", now); d != 12*time.Second {
		t.Fatalf("seconds = %v", d)
	}
	if d := retryAfter(now.Add(time.Minute).Format(http.TimeFormat), now); d != time.Minute {
		t.Fatalf("date = %v", d)
	}
	if d := retryAfter("soon", now); d != 0 {
		t.Fatalf("invalid = %v", d)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientDefaultsToTypeSafe(t *testing.T) {
	var url string
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		url = r.URL.String()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(answered)), Header: http.Header{}}, nil
	})}
	if _, err := (Client{Model: "jev-latest", APIKey: "k", HTTPClient: client}).Evaluate(context.Background(), ticket()); err != nil {
		t.Fatal(err)
	}
	if url != "https://api.typesafe.ai/v1/systemone" {
		t.Fatalf("url = %s", url)
	}
}
