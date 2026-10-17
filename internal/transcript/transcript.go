// Package transcript turns each harness's own message JSON into one common
// shape, for replaying sessions from any harness the same way. It is a
// pure function of the stored payload, so the view built from it can be
// rebuilt whenever this code changes.
package transcript

import (
	"encoding/json"
	"strings"
)

// Roles in the common format.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool" // tool results fed back to the model
	RoleSystem    = "system"
	RoleUnknown   = "unknown"
)

// Part kinds.
const (
	KindText       = "text"
	KindToolCall   = "tool_call"
	KindToolResult = "tool_result"
	KindReasoning  = "reasoning"
	KindImage      = "image"
	KindOther      = "other" // a block this package doesn't know; Text names its type
)

// Message is one message in the common format.
type Message struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`
}

// Part is one piece of a message.
type Part struct {
	Kind   string          `json:"kind"`
	Text   string          `json:"text,omitempty"`
	Name   string          `json:"name,omitempty"`    // tool name
	CallID string          `json:"call_id,omitempty"` // pairs a tool call with its result
	Input  json.RawMessage `json:"input,omitempty"`   // tool call arguments
	Error  bool            `json:"error,omitempty"`   // a failed tool result
}

// Normalize reads one message as the named harness wrote it.
func Normalize(harness string, msg json.RawMessage) Message {
	var m Message
	switch harness {
	case "claude-code":
		m = claudeCode(msg)
	case "pi":
		m = pi(msg)
	case "codex":
		m = codex(msg)
	case "strands":
		m = strands(msg)
	default:
		m = Message{Role: RoleUnknown}
	}
	if m.Parts == nil {
		m.Parts = []Part{}
	}
	return m
}

// claudeCode: a transcript line, {type, message: {role, content}}.
// Content is a string or Anthropic content blocks.
func claudeCode(raw json.RawMessage) Message {
	var line struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"` // system lines
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &line) != nil {
		return Message{Role: RoleUnknown}
	}
	if line.Type == "system" {
		return Message{Role: RoleSystem, Parts: textOrBlocks(line.Content, anthropicBlock)}
	}
	m := Message{Role: line.Message.Role, Parts: textOrBlocks(line.Message.Content, anthropicBlock)}
	return toolRole(m)
}

func anthropicBlock(b json.RawMessage) Part {
	var v struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	json.Unmarshal(b, &v)
	switch v.Type {
	case "text":
		return Part{Kind: KindText, Text: v.Text}
	case "thinking":
		return Part{Kind: KindReasoning, Text: v.Thinking}
	case "tool_use":
		return Part{Kind: KindToolCall, Name: v.Name, CallID: v.ID, Input: v.Input}
	case "tool_result":
		return Part{Kind: KindToolResult, CallID: v.ToolUseID, Text: flatten(v.Content), Error: v.IsError}
	case "image":
		return Part{Kind: KindImage}
	}
	return Part{Kind: KindOther, Text: v.Type}
}

// pi: a session entry, {type: "message", message: {role, content, ...}}.
func pi(raw json.RawMessage) Message {
	var entry struct {
		Message struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"toolCallId"`
			ToolName   string          `json:"toolName"`
			IsError    bool            `json:"isError"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &entry) != nil {
		return Message{Role: RoleUnknown}
	}
	e := entry.Message
	if e.Role == "toolResult" {
		return Message{Role: RoleTool, Parts: []Part{{Kind: KindToolResult, Name: e.ToolName, CallID: e.ToolCallID, Text: flatten(e.Content), Error: e.IsError}}}
	}
	role := e.Role
	if role != RoleUser && role != RoleAssistant && role != RoleSystem {
		role = RoleUser // bashExecution, custom: things the person put in front of the model
	}
	return Message{Role: role, Parts: textOrBlocks(e.Content, func(b json.RawMessage) Part {
		var v struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(b, &v)
		switch v.Type {
		case "text":
			return Part{Kind: KindText, Text: v.Text}
		case "thinking":
			return Part{Kind: KindReasoning, Text: v.Thinking}
		case "toolCall":
			return Part{Kind: KindToolCall, Name: v.Name, CallID: v.ID, Input: v.Arguments}
		case "image":
			return Part{Kind: KindImage}
		}
		return Part{Kind: KindOther, Text: v.Type}
	})}
}

// codex: a rollout line, {type: "response_item", payload: {type, ...}}.
func codex(raw json.RawMessage) Message {
	var line struct {
		Payload struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Input     string          `json:"input"`
			CallID    string          `json:"call_id"`
			Output    json.RawMessage `json:"output"`
			Summary   []struct {
				Text string `json:"text"`
			} `json:"summary"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &line) != nil {
		return Message{Role: RoleUnknown}
	}
	p := line.Payload
	switch p.Type {
	case "message":
		role := p.Role
		if role == "developer" {
			role = RoleSystem
		}
		return Message{Role: role, Parts: textOrBlocks(p.Content, func(b json.RawMessage) Part {
			var v struct{ Type, Text string }
			json.Unmarshal(b, &v)
			switch v.Type {
			case "input_text", "output_text", "text":
				return Part{Kind: KindText, Text: v.Text}
			case "input_image":
				return Part{Kind: KindImage}
			}
			return Part{Kind: KindOther, Text: v.Type}
		})}
	case "function_call", "custom_tool_call":
		args := p.Arguments
		if p.Type == "custom_tool_call" {
			args = p.Input
		}
		input := json.RawMessage(args)
		if !json.Valid(input) {
			input, _ = json.Marshal(args)
		}
		return Message{Role: RoleAssistant, Parts: []Part{{Kind: KindToolCall, Name: p.Name, CallID: p.CallID, Input: input}}}
	case "function_call_output", "custom_tool_call_output":
		return Message{Role: RoleTool, Parts: []Part{{Kind: KindToolResult, CallID: p.CallID, Text: codexOutput(p.Output)}}}
	case "reasoning":
		var texts []string
		for _, s := range p.Summary {
			texts = append(texts, s.Text)
		}
		return Message{Role: RoleAssistant, Parts: []Part{{Kind: KindReasoning, Text: strings.Join(texts, "\n\n")}}}
	}
	return Message{Role: RoleAssistant, Parts: []Part{{Kind: KindOther, Text: p.Type}}}
}

// codexOutput is a string, or {"output": string, ...} in some versions.
func codexOutput(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var v struct {
		Output string `json:"output"`
	}
	if json.Unmarshal(raw, &v) == nil && v.Output != "" {
		return v.Output
	}
	return flatten(raw)
}

// strands: a Bedrock-style message, {role, content: [{text} | {toolUse} |
// {toolResult} | {reasoningContent} | {image}]}.
func strands(raw json.RawMessage) Message {
	var m struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return Message{Role: RoleUnknown}
	}
	out := Message{Role: m.Role}
	for _, b := range m.Content {
		var v struct {
			Text    *string `json:"text"`
			ToolUse *struct {
				ToolUseID string          `json:"toolUseId"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
			} `json:"toolUse"`
			ToolResult *struct {
				ToolUseID string          `json:"toolUseId"`
				Status    string          `json:"status"`
				Content   json.RawMessage `json:"content"`
			} `json:"toolResult"`
			Reasoning *struct {
				ReasoningText struct {
					Text string `json:"text"`
				} `json:"reasoningText"`
			} `json:"reasoningContent"`
			Image json.RawMessage `json:"image"`
		}
		json.Unmarshal(b, &v)
		switch {
		case v.Text != nil:
			out.Parts = append(out.Parts, Part{Kind: KindText, Text: *v.Text})
		case v.ToolUse != nil:
			out.Parts = append(out.Parts, Part{Kind: KindToolCall, Name: v.ToolUse.Name, CallID: v.ToolUse.ToolUseID, Input: v.ToolUse.Input})
		case v.ToolResult != nil:
			out.Parts = append(out.Parts, Part{Kind: KindToolResult, CallID: v.ToolResult.ToolUseID, Text: flatten(v.ToolResult.Content), Error: v.ToolResult.Status == "error"})
		case v.Reasoning != nil:
			out.Parts = append(out.Parts, Part{Kind: KindReasoning, Text: v.Reasoning.ReasoningText.Text})
		case v.Image != nil:
			out.Parts = append(out.Parts, Part{Kind: KindImage})
		default:
			out.Parts = append(out.Parts, Part{Kind: KindOther, Text: firstKey(b)})
		}
	}
	return toolRole(out)
}

// toolRole marks a user message that only carries tool results as the
// tool's: Anthropic and Bedrock formats send results as user turns.
func toolRole(m Message) Message {
	if m.Role != RoleUser || len(m.Parts) == 0 {
		return m
	}
	for _, p := range m.Parts {
		if p.Kind != KindToolResult {
			return m
		}
	}
	m.Role = RoleTool
	return m
}

// textOrBlocks reads content that is either a plain string or an array of
// blocks.
func textOrBlocks(raw json.RawMessage, block func(json.RawMessage) Part) []Part {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []Part{{Kind: KindText, Text: s}}
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	parts := make([]Part, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, block(b))
	}
	return parts
}

// flatten pulls the text out of tool-result content: a string, or blocks
// with "text" (or Strands' {"json": ...}).
func flatten(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string          `json:"text"`
		JSON json.RawMessage `json:"json"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var texts []string
		for _, b := range blocks {
			switch {
			case b.Text != "":
				texts = append(texts, b.Text)
			case len(b.JSON) > 0:
				texts = append(texts, string(b.JSON))
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(raw)
}

func firstKey(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	json.Unmarshal(raw, &m)
	for k := range m {
		return k
	}
	return ""
}
