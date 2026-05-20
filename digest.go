package searchable_history

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/collinpfeifer/beluga/pkg/model"
)

// BuildDigest converts session events into a digest (messages only, tools stripped).
// User messages are kept verbatim with "User: " prefix.
// Agent messages are kept verbatim with "Agent: " prefix.
// All other event types (tool calls, results, status transitions, errors,
// compacted, interrupts) are stripped entirely.
func BuildDigest(events []model.Event) string {
	var sb strings.Builder
	for _, evt := range events {
		switch evt.Type {
		case model.EventTypeUserMessage:
			var p model.UserMessagePayload
			if err := json.Unmarshal(evt.Data, &p); err != nil {
				continue
			}
			fmt.Fprintf(&sb, "User: %s\n", p.Content)
		case model.EventTypeAgentMessage:
			var p model.AgentMessagePayload
			if err := json.Unmarshal(evt.Data, &p); err != nil {
				continue
			}
			fmt.Fprintf(&sb, "Agent: %s\n", p.Content)
		default:
			// Skip all other event types: tool_call, tool_result, status_transition,
			// error, compacted, interrupt.
		}
	}
	return sb.String()
}
