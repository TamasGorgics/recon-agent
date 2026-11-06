package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestScriptedQueueAndRequestChecks(t *testing.T) {
	first := Response{ID: "one", Message: Message{Role: RoleAssistant, Content: "first"}}
	second := Response{ID: "two", Message: Message{Role: RoleAssistant, Content: "second"}}
	var checked Request
	client := NewScripted(
		ScriptedStep{Response: first, Check: func(req Request) error {
			checked = req
			return nil
		}},
		ScriptedStep{Response: second},
	)

	req := Request{Model: "m", System: "s", PromptVersion: "v1"}
	resp, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete first: %v", err)
	}
	if resp.ID != "one" {
		t.Fatalf("first ID = %q", resp.ID)
	}
	if checked.Model != "m" || checked.System != "s" {
		t.Fatalf("Check saw %+v", checked)
	}
	resp, err = client.Complete(context.Background(), req)
	if err != nil || resp.ID != "two" {
		t.Fatalf("Complete second = %+v, %v", resp, err)
	}
	if got, want := client.Calls(), 2; got != want {
		t.Fatalf("Calls = %d, want %d", got, want)
	}
	if got, want := client.Remaining(), 0; got != want {
		t.Fatalf("Remaining = %d, want %d", got, want)
	}
	if got, want := len(client.Requests()), 2; got != want {
		t.Fatalf("Requests = %d, want %d", got, want)
	}
}

func TestScriptedErrorAndCheckFailure(t *testing.T) {
	sentinel := errors.New("canned failure")
	checkErr := errors.New("bad request")
	client := NewScripted(
		ScriptedStep{Err: sentinel},
		ScriptedStep{Response: Response{ID: "never"}, Check: func(Request) error { return checkErr }},
	)
	if _, err := client.Complete(context.Background(), Request{}); !errors.Is(err, sentinel) {
		t.Fatalf("Complete error = %v, want %v", err, sentinel)
	}
	if _, err := client.Complete(context.Background(), Request{}); !errors.Is(err, checkErr) {
		t.Fatalf("Check error = %v, want %v", err, checkErr)
	}
	if got, want := client.Remaining(), 0; got != want {
		t.Fatalf("Remaining after check failure = %d, want %d", got, want)
	}
}

func TestScriptedExhaustion(t *testing.T) {
	client := NewScripted()
	if _, err := client.Complete(context.Background(), Request{}); !errors.Is(err, ErrScriptedExhausted) {
		t.Fatalf("Complete error = %v, want ErrScriptedExhausted", err)
	}
	if got, want := client.Calls(), 1; got != want {
		t.Fatalf("Calls = %d, want %d", got, want)
	}
	if got, want := client.Remaining(), 0; got != want {
		t.Fatalf("Remaining = %d, want %d", got, want)
	}
}

func TestScriptedConcurrentConsumesEachStepOnce(t *testing.T) {
	const steps = 50
	queue := make([]ScriptedStep, steps)
	for i := range queue {
		queue[i] = ScriptedStep{Response: Response{ID: "step"}}
	}
	client := NewScripted(queue...)
	var wg sync.WaitGroup
	for range steps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.Complete(context.Background(), Request{}); err != nil {
				t.Errorf("Complete: %v", err)
			}
		}()
	}
	wg.Wait()
	if got, want := client.Calls(), steps; got != want {
		t.Fatalf("Calls = %d, want %d", got, want)
	}
	if got, want := client.Remaining(), 0; got != want {
		t.Fatalf("Remaining = %d, want %d", got, want)
	}
}
