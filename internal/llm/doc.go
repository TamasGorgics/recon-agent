// Package llm defines the provider-neutral language-model boundary used by the
// agent. Client.Complete is non-streaming: callers send a model, prompt
// messages, optional tool definitions, and receive one mapped response.
//
// The package ships an OpenCode Console Inference adapter over OpenAI-compatible
// Chat Completions (OpenCode) and deterministic test doubles: Scripted queues
// canned responses and Cassette records, replays, or passes through responses.
// Resilience composition (retry, breaker, fallback) wraps any Client; this
// package never imports internal/agent so the dependency points one way.
package llm
