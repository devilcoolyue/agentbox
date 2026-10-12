package decision

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentbox/internal/collaboration"
	"agentbox/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testJevConfig() config.DecisionJev {
	return config.DecisionJev{Enabled: true, Model: config.DecisionJevModel, TimeoutMS: 100, MaxRequestBytes: 32 << 10, MaxConcurrency: 2, APIKey: "secret-test-key"}
}
func testRequest() Request {
	return Request{ID: "decision1", Purpose: "model_selection", TemplateVersion: TemplateVersion, State: "Fix the selected test", Roles: map[collaboration.Role][]Candidate{collaboration.Executor: {{Ref: "candidate_1", Description: "First configured model", Binding: collaboration.Binding{AccountID: "private-account", Model: "private-binding"}}, {Ref: "candidate_2", Description: "Second configured model"}}}}
}
func jsonResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

const validResponse = `{"model":"jev-1.13.0","answers":{"executor":{"type":"choice","choice":"candidate_2","probabilities":{"candidate_1":0.05,"candidate_2":0.9,"abstain":0.05},"confidence":0.8}},"usage":{"input_tokens":1000,"output_tokens":30}}`

func TestJevOfficialProtocolOpaqueRefsAndPrice(t *testing.T) {
	var count atomic.Int32
	j := newJev(testJevConfig(), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count.Add(1)
		if r.URL.String() != JevEndpoint || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer secret-test-key" || r.Header.Get("User-Agent") != "agentbox/decision" {
			t.Fatal("incorrect endpoint/auth")
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "private-account") || strings.Contains(string(raw), "private-binding") || strings.Contains(string(raw), "secret-test-key") {
			t.Fatal("binding or credentials leaked into body")
		}
		var request jevRequest
		if json.Unmarshal(raw, &request) != nil || request.Questions["executor"].Type != "choice" || len(request.Questions["executor"].Criteria) != 3 {
			t.Fatal("wrong official protocol", string(raw))
		}
		return jsonResponse(200, validResponse), nil
	}))
	bound, err := j.ReservedMicroUSD()
	if err != nil || bound != 2753 {
		t.Fatal(bound, err)
	}
	e, err := j.Evaluate(context.Background(), testRequest())
	if err != nil || e.Model != config.DecisionJevModel || !e.Dispatched || e.Suggestions[collaboration.Executor].CandidateRef != "candidate_2" || e.Usage.CostMicroUSD != 42 || e.Usage.CostStatus != "known" || count.Load() != 1 {
		t.Fatalf("%+v %v count=%d", e, err, count.Load())
	}
}
func TestJevNeverRetriesOrRedirectsAndRetainsErrorUsage(t *testing.T) {
	for _, status := range []int{302, 401, 429, 529} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			j := newJev(testJevConfig(), roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				r := jsonResponse(status, validResponse)
				r.Header.Set("Location", "https://other.invalid/secret")
				return r, nil
			}))
			e, err := j.Evaluate(context.Background(), testRequest())
			if err == nil || calls != 1 || !e.Usage.Known || e.Usage.CostMicroUSD != 42 {
				t.Fatalf("calls=%d eval=%+v error=%v", calls, e, err)
			}
		})
	}
}

func TestJevAccountingRoundsOnlyReservationUp(t *testing.T) {
	for tokens, want := range map[int64]int64{0: 0, 1: 0, 12: 1, 1000: 42, 65536: 2753} {
		if got := jevInputCost(tokens); got != want {
			t.Fatalf("tokens %d cost %d want %d", tokens, got, want)
		}
	}
}

func TestJevDuplicateEvidenceDoesNotSettleReservation(t *testing.T) {
	body := strings.Replace(validResponse, `"input_tokens":1000`, `"input_tokens":2000,"input_tokens":1000`, 1)
	j := newJev(testJevConfig(), roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(200, body), nil }))
	e, err := j.Evaluate(context.Background(), testRequest())
	if err == nil || e.Usage.Known || e.Usage.CostStatus != "unknown" {
		t.Fatal(e, err)
	}
}

func TestMindshubUsesOwnEndpointAliasAndFreePricing(t *testing.T) {
	cfg := testJevConfig()
	cfg.Gateway = "mindshub"
	calls := 0
	j := newJev(cfg, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != MindshubJevEndpoint || r.Header.Get("User-Agent") != "agentbox/decision" {
			t.Fatal("wrong endpoint", r.URL)
		}
		var body jevRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "jev" {
			t.Fatal("wrong gateway model", body.Model)
		}
		return jsonResponse(200, validResponse), nil
	}))
	if amount, err := j.ReservedMicroUSD(); err != nil || amount != 0 {
		t.Fatal(amount, err)
	}
	e, err := j.Evaluate(context.Background(), testRequest())
	if err != nil || calls != 1 || e.Provider != "mindshub" || e.Model != config.DecisionJevModel || e.Status != "suggest" || !e.Usage.Known || e.Usage.InputTokens != 1000 || e.Usage.OutputTokens != 30 || e.Usage.CostMicroUSD != 0 || e.Usage.CostStatus != "known" {
		t.Fatal(e, err)
	}
}

func TestMindshubUnknownUsageAndVersionNeverAssumeKnownZero(t *testing.T) {
	for name, body := range map[string]string{
		"missing usage":        `{"model":"jev-1.13.0","answers":{}}`,
		"unversioned response": strings.Replace(validResponse, "jev-1.13.0", "jev", 1),
		"new version":          strings.Replace(validResponse, "jev-1.13.0", "jev-1.14.0", 1),
		"context contract":     strings.Replace(validResponse, `"input_tokens":1000`, `"input_tokens":64001`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testJevConfig()
			cfg.Gateway = "mindshub"
			j := newJev(cfg, roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(200, body), nil }))
			e, err := j.Evaluate(context.Background(), testRequest())
			if err == nil || !e.Dispatched || e.Usage.CostStatus != "unknown" || e.Usage.CostMicroUSD != 0 {
				t.Fatal(e, err)
			}
		})
	}
	cfg := testJevConfig()
	cfg.Gateway = "mindshub"
	j := newJev(cfg, roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }))
	e, err := j.Evaluate(context.Background(), testRequest())
	if !errors.Is(err, context.DeadlineExceeded) || e.Usage.Known || e.Usage.CostStatus != "unknown" {
		t.Fatal(e, err)
	}
}

func TestMindshubErrorsDoNotRetryAndKeepReportedFreeUsage(t *testing.T) {
	cfg := testJevConfig()
	cfg.Gateway = "mindshub"
	calls := 0
	j := newJev(cfg, roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return jsonResponse(429, validResponse), nil }))
	e, err := j.Evaluate(context.Background(), testRequest())
	if err == nil || calls != 1 || !e.Usage.Known || e.Usage.CostStatus != "known" || e.Usage.CostMicroUSD != 0 || e.Usage.InputTokens != 1000 {
		t.Fatal(e, err, calls)
	}
	cfg.Gateway = "unexpected"
	j = newJev(cfg, roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unknown gateway dispatched"); return nil, nil }))
	e, err = j.Evaluate(context.Background(), testRequest())
	if !errors.Is(err, ErrBudgetUnknown) || e.Dispatched {
		t.Fatal(e, err)
	}
}
func TestJevTimeoutAndCancellationKeepUnknownCharge(t *testing.T) {
	j := newJev(testJevConfig(), roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }))
	e, err := j.Evaluate(context.Background(), testRequest())
	if !errors.Is(err, context.DeadlineExceeded) || !e.Dispatched || e.Usage.CostStatus != "unknown" {
		t.Fatal(e, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e, err = j.Evaluate(ctx, testRequest())
	if !errors.Is(err, context.Canceled) || e.Dispatched || e.Usage.CostStatus != "not_dispatched" {
		t.Fatal(e, err)
	}
}
func TestJevInvalidRequestIsNeverDispatched(t *testing.T) {
	tests := map[string]func(*Jev, *Request){
		"unpriced model": func(j *Jev, r *Request) { j.cfg.Model = "jev-latest" },
		"oversized text": func(j *Jev, r *Request) { r.State = strings.Repeat("中", 20000) },
		"duplicate refs": func(j *Jev, r *Request) { r.Roles[collaboration.Executor][1].Ref = "candidate_1" },
		"bad role": func(j *Jev, r *Request) {
			r.Roles["unknown"] = r.Roles[collaboration.Executor]
			delete(r.Roles, collaboration.Executor)
		},
		"wrong template": func(j *Jev, r *Request) { r.TemplateVersion = "future" },
		"disabled":       func(j *Jev, r *Request) { j.cfg.Enabled = false },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			j := newJev(testJevConfig(), roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected network"); return nil, nil }))
			req := testRequest()
			change(j, &req)
			e, err := j.Evaluate(context.Background(), req)
			if err == nil || e.Dispatched || e.Usage.CostStatus != "not_dispatched" {
				t.Fatal(e, err)
			}
		})
	}
}
func TestJevInvalidAnswersPreserveKnownUsage(t *testing.T) {
	tests := map[string]string{
		"foreign candidate":    strings.Replace(validResponse, `"choice":"candidate_2"`, `"choice":"outside"`, 1),
		"wrong type":           strings.Replace(validResponse, `"type":"choice"`, `"type":"score"`, 1),
		"nonmax choice":        strings.Replace(validResponse, `"choice":"candidate_2"`, `"choice":"candidate_1"`, 1),
		"invalid distribution": strings.Replace(validResponse, `"candidate_2":0.9`, `"candidate_2":0.7`, 1),
		"low confidence":       strings.Replace(validResponse, `"confidence":0.8`, `"confidence":0.3`, 1),
		"missing confidence":   strings.Replace(validResponse, `,"confidence":0.8`, "", 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			j := newJev(testJevConfig(), roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(200, body), nil }))
			e, err := j.Evaluate(context.Background(), testRequest())
			if err != nil || e.Status != "abstain" || e.Suggestions[collaboration.Executor].Status != "abstain" || e.Usage.CostMicroUSD != 42 {
				t.Fatal(e, err)
			}
		})
	}
}
func TestJevPerRoleAdviceAndUsageContract(t *testing.T) {
	body := strings.Replace(validResponse, `"answers":{`, `"answers":{"coordinator":{"type":"wrong"},`, 1)
	req := testRequest()
	req.Roles[collaboration.Coordinator] = req.Roles[collaboration.Executor]
	j := newJev(testJevConfig(), roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(200, body), nil }))
	e, err := j.Evaluate(context.Background(), req)
	if err != nil || e.Suggestions[collaboration.Coordinator].Status != "abstain" || e.Suggestions[collaboration.Executor].Status != "suggest" {
		t.Fatal(e, err)
	}
	for _, bad := range []string{strings.Replace(validResponse, "jev-1.13.0", "jev-unknown", 1), strings.Replace(validResponse, `"input_tokens":1000`, `"input_tokens":999999`, 1), strings.Replace(validResponse, `"input_tokens":1000`, `"input_tokens":-1`, 1)} {
		j := newJev(testJevConfig(), roundTripFunc(func(*http.Request) (*http.Response, error) { return jsonResponse(200, bad), nil }))
		e, err := j.Evaluate(context.Background(), testRequest())
		if err == nil || e.Usage.CostStatus != "unknown" {
			t.Fatal(e, err)
		}
	}
}
func TestServiceLimitCircuitAndGeneration(t *testing.T) {
	s := NewService()
	now := time.Now()
	s.now = func() time.Time { return now }
	release, err := s.Acquire(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(1, 1); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	release(true)
	release(true) // exactly one release and failure
	for i := 0; i < 2; i++ {
		r, err := s.Acquire(1, 1)
		if err != nil {
			t.Fatal(err)
		}
		r(true)
	}
	if _, err := s.Acquire(1, 1); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	probe, err := s.Acquire(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(1, 2); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("expected one probe", err)
	}
	probe(false)
	old, err := s.Acquire(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(2, 1); !errors.Is(err, ErrBusy) {
		t.Fatal("old generation lost capacity", err)
	}
	old(true)
	fresh, err := s.Acquire(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	fresh(false)
	if _, err := s.Acquire(1, 1); !errors.Is(err, ErrGeneration) {
		t.Fatal(err)
	}
}
