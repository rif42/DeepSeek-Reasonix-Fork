package memoryreview

import (
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/memory"
	"reasonix/internal/provider"
)

// RenderTranscript turns a loaded session into a compact, reviewable transcript
// (system messages dropped, tool results truncated), capped at maxChars.
func RenderTranscript(sess *agent.Session, maxChars int) string {
	var b strings.Builder
	const maxToolResult = 2000
	for _, m := range sess.Messages {
		switch m.Role {
		case provider.RoleUser:
			b.WriteString("[user] ")
			b.WriteString(strings.TrimSpace(m.Content))
		case provider.RoleAssistant:
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "[assistant tool call] %s(%s)\n", tc.Name, truncate(tc.Arguments, 500))
				}
				continue
			}
			b.WriteString("[assistant] ")
			b.WriteString(strings.TrimSpace(m.Content))
		case provider.RoleTool:
			b.WriteString("[tool result] ")
			b.WriteString(truncate(strings.TrimSpace(m.Content), maxToolResult))
		case provider.RoleSystem:
			continue
		}
		b.WriteString("\n")
	}
	out := strings.TrimSpace(b.String())
	if maxChars > 0 && len(out) > maxChars {
		out = truncate(out, maxChars) + "\n…[transcript truncated]"
	}
	return out
}

// BuildPrompt constructs the review instruction: distill the transcript into
// durable memory facts as JSON, listing existing facts so the model avoids
// near-duplicates and prefers updates.
func BuildPrompt(transcript string, existing []memory.Memory) string {
	var b strings.Builder
	b.WriteString(`You are the background memory reviewer for an AI coding agent. Read the conversation transcript below and distill the durable, non-obvious facts worth remembering across sessions. Write the facts as JSON and nothing else.

Rules:
- Facts must be durable and self-contained: preferences, working style, project constraints/goals, external references, environment facts. Skip one-off task details and anything already obvious from code.
- Prefer updating an existing fact over creating a near-duplicate. If an existing fact needs a richer body, reuse its exact "name" and bump the content.
- "name" is a short kebab-case slug, unique per fact, e.g. "prefers-english".
- "title" is a short human-readable label; "description" is one line for the memory index.
- "body" is the full fact in Markdown (for type "feedback" include a "Why:" line and a "How to apply:" line).
- "type" is one of: user (who the user is), feedback (how to work, with why), project (ongoing goals/constraints), reference (external pointers).
- "scope" is "project" (default) or "global" (affects every workspace — only when the fact is about the user or universally applicable).
- Do NOT call any tools. Output ONLY a JSON object in this exact shape:
{"facts":[{"type":"project","scope":"project","name":"...","title":"...","description":"...","body":"..."}]}

Existing memory (avoid duplicates; reuse "name" to update):
`)
	for _, m := range existing {
		fmt.Fprintf(&b, "- %s [%s/%s] %s: %s\n", m.Name, m.Type, m.Scope, m.Title, m.Description)
	}
	b.WriteString("\nTranscript:\n")
	b.WriteString("```\n")
	b.WriteString(transcript)
	b.WriteString("\n```\n")
	return b.String()
}

// ParseFacts extracts the facts array from the review model's final response,
// tolerating a surrounding markdown code fence and trailing prose.
func ParseFacts(out string) ([]Fact, error) {
	s := strings.TrimSpace(out)
	if i := strings.Index(s, "```"); i >= 0 {
		if j := strings.LastIndex(s, "```"); j > i {
			s = strings.TrimSpace(s[i+3 : j])
		}
	}
	if i := strings.Index(s, "```json"); i >= 0 {
		s = strings.TrimSpace(s[i+len("```json"):])
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in review output")
	}
	var payload struct {
		Facts []Fact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &payload); err != nil {
		return nil, fmt.Errorf("invalid facts JSON: %w", err)
	}
	return payload.Facts, nil
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}
