// Package toolcalling provides simulated tool calling support for clients
// (Claude Code, Codex, etc.) by injecting tool definitions into the message
// text sent to M365 Copilot and parsing tool call patterns from the response.
//
// M365 Copilot backend does not natively support client-defined tools.
// This package bridges the gap by:
//   - Injecting tool definitions as a system prompt prefix into the last user message
//   - Parsing tool call JSON blocks from M365 response text
//   - Converting tool role messages (OpenAI) and tool_result blocks (Anthropic)
//     back into text for the M365 backend
package toolcalling

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ToolDef represents a tool definition from the client request.
type ToolDef struct {
	Type     string      `json:"type"`
	Function ToolDefFunc `json:"function"`
	// Anthropic-style fields (flat, no "function" wrapper)
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

// ToolDefFunc is the OpenAI-style function definition inside a tool.
type ToolDefFunc struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ToolCall represents a parsed tool call from the M365 response.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toolCallIDCounter generates sequential tool call IDs.
var toolCallIDCounter int

// nextToolCallID returns a unique tool call ID.
func nextToolCallID() string {
	toolCallIDCounter++
	return fmt.Sprintf("call_%d", toolCallIDCounter)
}

// toolCallPattern matches JSON blocks wrapped in angle-bracket tool tags.
// The tag name is "tool" and the content is a JSON object with name and arguments.
var toolCallPattern = regexp.MustCompile(`(?s)<tool>\s*(\{.*?\})\s*</tool>`)

// InjectTools prepends tool definitions and instructions to the last user message.
// Returns the modified message text. If no tools are provided, returns the original text.
func InjectTools(messages []string, tools []ToolDef) []string {
	if len(tools) == 0 || len(messages) == 0 {
		return messages
	}

	instruction := buildToolInstruction(tools)
	result := make([]string, len(messages))
	copy(result, messages)

	// Find the last user message and prepend the tool instruction
	for i := len(result) - 1; i >= 0; i-- {
		result[i] = instruction + "\n\n" + result[i]
		break
	}

	return result
}

// buildToolInstruction creates the system prompt that tells the model about available tools
// and the format to use when calling them. The prompt is deliberately forceful to override
// the M365 backend's tendency to use its own built-in tools (code_interpreter, search)
// instead of the client-defined tools.
func buildToolInstruction(tools []ToolDef) string {
	var sb strings.Builder

	sb.WriteString("=== MANDATORY TOOL USE PROTOCOL ===\n")
	sb.WriteString("You are operating as a tool-calling assistant. The tools listed below are the ONLY tools you may use. ")
	sb.WriteString("You do NOT have access to code_interpreter, web search, or any other built-in tools for this request. ")
	sb.WriteString("Do NOT attempt to use code_interpreter, search, or any tool not listed below. ")
	sb.WriteString("Do NOT say that a tool is unavailable or not in your environment. The tools below ARE available to you.\n\n")

	sb.WriteString("When the user's request requires using a tool, you MUST respond with a tool call. ")
	sb.WriteString("A tool call is a JSON object wrapped in angle-bracket tool tags, like this:\n")
	sb.WriteString("<tool>{\"name\":\"tool_name\",\"arguments\":{\"key\":\"value\"}}</tool>\n\n")

	sb.WriteString("CRITICAL RULES:\n")
	sb.WriteString("1. When a tool is needed, output the tool call block. Do NOT use markdown code fences around it.\n")
	sb.WriteString("2. The tool call MUST be wrapped in <tool> and </tool> tags.\n")
	sb.WriteString("3. The JSON inside must have \"name\" and \"arguments\" fields. Arguments must be a JSON object.\n")
	sb.WriteString("4. Do NOT use your built-in code_interpreter or search tools. Use ONLY the tools listed below.\n")
	sb.WriteString("5. If the user asks you to read/write a file, list a directory, or perform any action that matches a tool below, you MUST call that tool.\n")
	sb.WriteString("6. Do NOT refuse by saying the tool is unavailable. It IS available. Call it.\n")
	sb.WriteString("7. You may include a brief explanation before the tool call block, but the tool call block MUST be present.\n\n")

	sb.WriteString("EXAMPLE:\n")
	sb.WriteString("User: Create a file called test.txt with content \"hello\"\n")
	sb.WriteString("Your response: I'll create that file for you.\n")
	sb.WriteString("<tool>{\"name\":\"write_file\",\"arguments\":{\"path\":\"test.txt\",\"content\":\"hello\"}}</tool>\n\n")

	sb.WriteString("=== AVAILABLE TOOLS ===\n")

	for _, tool := range tools {
		name := tool.Function.Name
		desc := tool.Function.Description
		params := tool.Function.Parameters

		// Handle Anthropic-style flat definition
		if name == "" {
			name = tool.Name
			desc = tool.Description
			params = tool.InputSchema
		}

		if name == "" {
			continue
		}

		sb.WriteString(fmt.Sprintf("Tool: %s\n", name))
		if desc != "" {
			sb.WriteString(fmt.Sprintf("Description: %s\n", desc))
		}
		if params != nil {
			if paramBytes, err := json.Marshal(params); err == nil {
				sb.WriteString(fmt.Sprintf("Parameters: %s\n", string(paramBytes)))
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("=== END TOOL DEFINITIONS ===\n")
	sb.WriteString("Remember: When a tool is needed, respond with the <tool>...</tool> block. Do NOT use code_interpreter or search. Do NOT refuse.\n")

	return sb.String()
}

// ParseToolCalls scans response text for <tool> blocks and extracts them.
// Returns the text with tool call blocks removed and the parsed tool calls.
// If no tool call blocks are found, returns the original text and nil.
func ParseToolCalls(text string) (string, []ToolCall) {
	matches := toolCallPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return text, nil
	}

	var calls []ToolCall
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}

		var parsed struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(m[1]), &parsed); err != nil {
			continue
		}
		if parsed.Name == "" {
			continue
		}

		calls = append(calls, ToolCall{
			ID:        nextToolCallID(),
			Name:      parsed.Name,
			Arguments: parsed.Arguments,
		})
	}

	if len(calls) == 0 {
		return text, nil
	}

	// Remove tool call blocks from text
	cleaned := toolCallPattern.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)

	return cleaned, calls
}

// FormatToolResult converts a tool result (from the client) into text
// that the M365 backend can understand in the next message.
func FormatToolResult(toolCallID, toolName, result string) string {
	return fmt.Sprintf("[Tool Result for %s (call_id: %s)]\n%s", toolName, toolCallID, result)
}

// FormatAssistantToolCall converts a previous assistant tool call (from conversation
// history) into text that the M365 backend can understand.
func FormatAssistantToolCall(toolName string, arguments json.RawMessage) string {
	return fmt.Sprintf("[Previous Tool Call: %s]\nArguments: %s", toolName, string(arguments))
}
