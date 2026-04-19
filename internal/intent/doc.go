// Package intent contains the first implementation slice of tachyon's
// intent compiler. It currently focuses on source discovery, metadata
// validation, deterministic generated registry emission, and CLI-facing
// workflows. Later phases can extend the generated package from policy
// metadata into concrete request/response programs without changing the
// public command surface added here.
package intent
