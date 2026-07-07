# tg_down_core

A small Rust helper for Tg-Down, invoked by the Go application through the
`tg-down-core` subprocess bridge (stdin/stdout JSON).

Scope is intentionally limited to the one rule the Go app actually calls:

- media type classification
- MIME extension mapping
- file name sanitization
- download path planning

The Go implementation remains authoritative; it mirrors the same logic and is
used as a byte-for-byte fallback whenever the helper is absent or reports an
incompatible protocol version. Requests and responses carry a `protocol_version`
field that both sides must agree on, so a stale helper never silently overrides
updated Go path logic.
