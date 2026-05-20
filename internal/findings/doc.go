// Package findings collects vulnerability findings from cages,
// deduplicates them, sanitizes oversized payloads, and persists them
// for the assessment workflow to process. The package owns the NATS
// JetStream bus that cages publish to (one stream per assessment),
// the bloom-filter dedupe layer that catches near-duplicates without
// hammering Postgres, the validation rules that reject malformed
// findings at ingestion, and the Postgres-backed store.
//
// A finding moves from candidate to validated when a validation cage
// independently confirms the agent's proof, or to rejected if validation
// fails. The status transitions are owned by the assessment package;
// this package owns the data and the ingestion path.
package findings
