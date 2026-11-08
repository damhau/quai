package repositories

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/damhau/quai/backend/internal/audit"
)

// connectionEvent is the audit event of a connection created, changed or deleted
// (docs/design/09-security-and-compliance.md#audit-log): the document by its digest, which the
// stored document can be checked against.
func connectionEvent(action, name, managedBy string, doc []byte, generation int64) audit.Event {
	d := map[string]any{"managedBy": managedBy}
	if doc != nil {
		sum := sha256.Sum256(doc)
		d["document"] = hex.EncodeToString(sum[:])
	}
	if generation > 0 {
		d["generation"] = generation
	}
	return audit.Event{Action: action, Object: audit.Object{Type: "repository", ID: name}, Details: d}
}
