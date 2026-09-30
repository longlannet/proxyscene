package manager

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Compare the complete connection config, not provider remarks or URI spelling.
// Fresh parsing keeps runtime-assigned tags out of the identity. A legacy node
// that no longer passes import validation can only match its exact stored URL.
func subscriptionNodeIdentity(raw string) string {
	prepared, err := prepareNode(raw)
	if err != nil {
		return "raw:" + raw
	}
	encoded, err := json.Marshal(prepared.Parsed.Outbound)
	if err != nil {
		return "raw:" + raw
	}
	// XHTTP extra can contain RawMessage objects. Decode those too, preserving
	// numbers and array order, so nested object order and escaping do not matter.
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "raw:" + raw
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "raw:" + raw
	}
	return fmt.Sprintf("config:%s:%x", prepared.Parsed.Protocol, sha256.Sum256(canonical))
}

// Keep selections stable, then prefer manually retained nodes. Ties keep the
// first stored node. Other copies keep ownership from unrefreshed sources and
// are removed when no remaining source provides them, even if selected.
func subscriptionNodePriority(node Node, protected map[string]bool) int {
	if protected[node.ID] {
		return 2
	}
	if !node.SubscriptionManaged {
		return 1
	}
	return 0
}
