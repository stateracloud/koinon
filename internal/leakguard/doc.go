// SPDX-License-Identifier: Apache-2.0

// Package leakguard is the pure core of the leak guard of ADR 0084: it decides whether the
// arguments of an outbound MCP tool call carry private text, and whether an outbound URL or
// search query has the shape of an exfiltration. It does no I/O; koinon-api's guard endpoint
// and agentd (#349) call it, and the taint store behind the Store interface is persisted there.
//
// # Matching form (Normalise)
//
// Text is matched in a canonical form: each character is decomposed with Unicode NFKD
// (compatibility forms such as fullwidth letters, ligatures and mathematical letters become
// their plain letters), combining marks are dropped (so "é" matches "e"), a table of common
// Cyrillic, Greek and Latin look-alikes is folded to Latin, case is folded, and every character
// that is not a letter or a decimal digit is a separator and is removed. Separators are removed,
// not collapsed, so "k-o-i-n-o-n" and "koinon" match. Digits are not folded to letters
// ("0" stays "0"): leetspeak is a transformation the guard does not undo (ADR 0084,
// Consequences). Invalid UTF-8 counts as a separator.
//
// # Decoders (Decode)
//
// Before matching, the guard undoes the encodings of ADR 0084 decision 2: URL percent-encoding
// ("+" needs no decoding: it and a space are both separators), base64 (standard and URL-safe
// alphabets, padded or raw, at any 4-character alignment inside a longer run) and hex (either
// case, at either alignment). A base64 or hex run is replaced by its decoding only when the
// decoding holds text: a stretch of at least 12 printable characters, which random bytes almost
// never have (a misaligned decoding of an ordinary word stays as it was). Data wrapped so that
// no 12 printable characters stand together is the "own cipher" case the URL rule backstops. Decoding
// nests up to Limits.MaxDepth (3) levels; text that still decodes further at that depth fails
// closed (ReasonDecodeDepth).
//
// # Split reassembly
//
// Text split across several arguments of one call is reassembled two ways: all arguments
// joined in call order, and every ordered pair of arguments (the last PairJoinBytes of one
// joined to the first PairJoinBytes of the other), so a two-way split matches in either order.
// With more than MaxPairArgs arguments only adjacent arguments are also joined in reverse
// order. Splits across calls are
// not reassembled; the per-host and per-run URL budgets bound them.
//
// # Fingerprints (winnowing)
//
// The taint store holds winnowed fingerprints (Schleimer, Wilkerson and Aiken, 2003), never the
// private text. With threshold t (default 24, ADR 0084 decision 3) and window w (default 4),
// the k-gram length is k = t - w + 1 (default 21). Every k-gram of the canonical text gets a
// 64-bit fingerprint; the store keeps the minimum of every w consecutive fingerprints (density
// about 2/(w+1), 40% at w = 4). Winnowing guarantees that any shared canonical substring of at
// least t characters shares a stored fingerprint, and a query checks every k-gram, so:
//   - a match of t or more canonical characters is always caught;
//   - a match of k to t-1 characters may be caught (only when that k-gram was kept);
//   - nothing shorter than k characters ever matches.
//
// Fingerprints are keyed: the k-gram's polynomial hash (mod 2^61-1, base derived from the key)
// is encrypted with AES-128 under a key derived from the scope key, so whoever reads the store
// cannot test a guess of a short window without the key. Give each home generation its own key
// (the conservative answer to ADR 0084's open question on keying); the Fingerprinter that feeds
// a scope must be the one that checks it.
//
// # URL shape rule and budgets (URLPolicy)
//
// For hosts the org has not trusted (the list starts empty, ADR 0084 decision 5), an outbound
// URL is refused when it is longer than 256 bytes, has a query string longer than 128 bytes,
// carries user info, an IP literal or a non-default port, percent-encodes unreserved characters
// (encoded data), or has a component (a host label, or a run of letters and digits in the
// decoded path or query) of 32 or more characters or a random-looking one of 16 or more. Per run
// it may reach at most 20 untrusted hosts and send at most 1024 bytes of distinct path and query
// to each. A web search query (ADR 0084 decision 6) is refused when it is longer than 256
// characters, carries an opaque or random-looking component, or the run has sent more than
// 8192 bytes of queries. The numbers come from the G1 replay of rc.9 traffic (0 of 218 fetches
// blocked; longest component 13, longest URL 134, at most 14 hosts and 2.5 KB per run).
//
// # Budgets
//
// Every check is bounded: argument count, total input bytes, decode depth, variants per text,
// a work counter over every byte decoded and normalised, and a wall-clock cap. A check that
// exceeds a budget fails closed: it returns a blocking Decision whose Reason names the budget.
package leakguard
