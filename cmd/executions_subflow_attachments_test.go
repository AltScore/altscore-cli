package cmd

import (
	"encoding/json"
	"errors"
	"testing"
)

func rows(n int) []json.RawMessage {
	out := make([]json.RawMessage, n)
	for i := range out {
		out[i] = json.RawMessage(`{"id":"a"}`)
	}
	return out
}

func TestSubflowAttachmentsPath(t *testing.T) {
	if got := subflowAttachmentsPath("exec-1", 2, 100, false); got != "/v1/executions/exec-1/output/subflow-attachments?page=2&per-page=100" {
		t.Fatalf("unexpected path %q", got)
	}
	if got := subflowAttachmentsPath("exec-1", 1, 50, true); got != "/v1/executions/exec-1/output/subflow-attachments?page=1&per-page=50&only-if-enabled=true" {
		t.Fatalf("unexpected path %q", got)
	}
}

func TestCollectSubflowAttachmentsWalksEveryPageAndStopsAtTheCount(t *testing.T) {
	asked := []int{}
	got, err := collectSubflowAttachments(func(page int) (subflowAttachmentsPage, error) {
		asked = append(asked, page)
		n := 2
		if page == 3 {
			n = 1
		}
		return subflowAttachmentsPage{Enabled: true, Count: 5, Truncated: page == 1, Data: rows(n)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 5 || got.Count != 5 || !got.Enabled || !got.Truncated {
		t.Fatalf("unexpected result %+v", got)
	}
	if len(asked) != 3 {
		t.Fatalf("asked pages %v, want 3", asked)
	}
}

func TestCollectSubflowAttachmentsStopsOnAnEmptyPage(t *testing.T) {
	calls := 0
	got, err := collectSubflowAttachments(func(page int) (subflowAttachmentsPage, error) {
		calls++
		return subflowAttachmentsPage{Count: 10}, nil
	})
	if err != nil || calls != 1 || len(got.Data) != 0 {
		t.Fatalf("calls=%d data=%d err=%v", calls, len(got.Data), err)
	}
}

func TestCollectSubflowAttachmentsReturnsTheError(t *testing.T) {
	boom := errors.New("boom")
	_, err := collectSubflowAttachments(func(page int) (subflowAttachmentsPage, error) {
		if page == 2 {
			return subflowAttachmentsPage{}, boom
		}
		return subflowAttachmentsPage{Count: 4, Data: rows(2)}, nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnEmptyAnswerStillPrintsAList(t *testing.T) {
	got, _ := collectSubflowAttachments(func(page int) (subflowAttachmentsPage, error) {
		return subflowAttachmentsPage{}, nil
	})
	encoded, _ := json.Marshal(got)
	if string(encoded) != `{"enabled":false,"count":0,"truncated":false,"data":[]}` {
		t.Fatalf("got %s", encoded)
	}
}
