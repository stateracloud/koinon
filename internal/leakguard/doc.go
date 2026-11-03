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
// Cyrillic, Greek, Armenian and Latin look-alikes is folded to Latin, case is folded, the
// decimal digits of every script become ASCII digits, and every character that is not a letter
// or a decimal digit is a separator and is removed. Separators are removed, not collapsed, so
// "k-o-i-n-o-n" and "koinon" match. Digits are not folded to letters ("0" stays "0"): leetspeak
// is a transformation the guard does not undo (ADR 0084, Consequences). Invalid UTF-8 counts
// as a separator. The look-alike table is a subset of Unicode's confusables (the scripts above,
// letters only); a look-alike from another script, or a symbol used as a letter, is not folded.
//
// # Decoders (Decode)
//
// Before matching, the guard undoes the encodings of ADR 0084 decision 2 and their common
// spellings:
//   - URL percent-encoding ("+" needs no decoding: it and a space are both separators);
//   - backslash escapes (\xHH, \uHHHH, \UHHHHHHHH) and HTML character references;
//   - base64 (standard and URL-safe alphabets, padded or raw) and base32 (RFC 4648 alphabet);
//   - hex in either case, as one run, in groups joined by up to two separators ("6b:6f",
//     xxd's "6b6f 696e", "6b, 6f") or as \xHH and 0xHH bytes;
//   - an encoded stream cut into chunks of any length by any separators ("projected"): for each
//     alphabet (base64 standard, base64 URL-safe, base32, hex) the text is projected onto it,
//     every byte outside the alphabet dropped (and the x of 0x for hex), and the projection
//     decoded. There is no density gate, so a separator of any length between chunks of any
//     length is read through; the price is that prose is projected too (its letters are
//     base64 and base32 symbols), which resynchronisation turns into nothing, at a cost the
//     work and time budgets bound (a 1 MiB argument takes at most about 0.4 s of the 1 s a call has, so
//     the clock is a backstop that a benign argument within the size cap does not reach). A
//     projection keeps stretches of 16 or more decoded bytes that are 60% letters, digits or
//     spaces (ASCII punctuation and NULs do not count against them), so chance ASCII in
//     misaligned decodings does not become a variant; shorter stretches (5 or more bytes, base64
//     and base32) are kept only in a chain that touches its neighbours and is together that long
//     and wordy, as the pieces of a text encoded piece by piece and joined are.
//
// Each base64, base32 and hex run is decoded at each of its own alignments and also
// resynchronised (every alignment decoded, the stretches that are text joined in order), so two
// encoded parts at different alignments in one run ("<b1>Q<b2>") decode as one text. A run is
// replaced by its decoding only when the decoding holds text: at least 16 bytes, a stretch of
// 12 printable characters, 90% of it printable and half of it, punctuation and symbols aside,
// letters, digits or spaces (an attacker can put a "-" or an emoji between the letters), which
// random bytes almost never are (a misaligned decoding of an ordinary word stays as it was).
// The projection of a stream is also decoded whole at each alignment, so chunked base64 of
// non-ASCII text decodes; an NUL among the printable characters does not end their run.
// Of several decodings of one run (alignments, byte orders) the one with the most characters
// that fold to ASCII letters and digits wins. Every decoding is also read as UTF-16 (either byte order and
// alignment; PowerShell's -EncodedCommand is UTF-16LE base64), and that reading is kept when
// its letters and digits sit in at most two 256-character blocks, as one script's text does
// (spaces, punctuation and emoji between them are left out of the count). Data wrapped so
// that no 12 printable characters stand together is the "own cipher" case the URL rule
// backstops, and the URL rule runs only on fetch and search calls: for any other tool a text
// that decodes to nothing printable passes the taint check. Decoding nests up to Limits.MaxDepth (3) levels; text that still decodes further at
// that depth fails closed (ReasonDecodeDepth).
//
// # Split reassembly
//
// Text split across several arguments of one call is reassembled two ways: all arguments
// joined in call order, and every ordered pair of arguments (the last PairJoinBytes of one
// joined to the first PairJoinBytes of the other), so a two-way split matches in either order.
// The joined text of all pairs is bounded (MaxWorkBytes/64 before decoding): long arguments are
// joined shorter (halving PairJoinBytes down to 64 bytes) rather than running out of time. With
// more than MaxPairArgs non-empty arguments every pair is still checked when there are at most
// 16*MaxPairArgs^2 pairs and the joined text fits at 64 bytes a side; otherwise the call fails
// closed (ReasonBudgetArgs). Splits across calls are not reassembled; the per-host
// and per-run URL budgets bound them.
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
// a work counter over every byte decoded and normalised, and a wall-clock cap, which is also
// checked before and after the store lookup and bounds the lookup's context. A check that
// exceeds a budget fails closed: it returns a blocking Decision whose Reason names the budget.
//
// # Store limits
//
// MemoryStore caps the fingerprints per scope (oldest evicted), the scopes (least recently used
// evicted) and the total across scopes: an Add past the total fails with ErrStoreFull rather
// than evict another scope. Lookup reports a scope that lost fingerprints to eviction
// or was refused by a full store (Hits.Evicted), and Check copies it to Decision.Evicted
// without changing Allow: whether an evicted scope blocks, asks or passes is #349's choice. The
// store remembers the last 4*MaxScopes scopes evicted whole; an older one is forgotten
// (StoreStats.ForgottenEvictions counts them) and then reports Evicted false, so alert on
// that counter rather than rely on the flag alone.
//
// # Known limits
//
// The guard does not catch:
//   - text split into three or more pieces of k-1 (20) canonical characters or fewer each,
//     unless they are in call order; text reordered inside one argument (words swapped);
//   - reversed text, ciphers, compression and encodings other than the ones above (base85,
//     uuencode, punycode, ...);
//   - an encoded stream whose separators are themselves symbols of its alphabet ("/" between
//     chunks of standard base64, "a" to "f" between hex digits): the projection keeps them, and
//     only chunks of 16 or more decoded bytes still decode;
//   - data carried in the host labels of a URL is checked for length and randomness but not
//     charged to the per-host byte budget, and letters-only data is never judged random, so a
//     run can carry about 20 KiB of letters-only text across 20 hosts within the URL rule.
//
// # Notes for the wiring (#349)
//
//   - MemoryStore holds one mutex for every scope, so one large check (a 1 MiB argument looked
//     up in 255 ms) stalls every other agent's checks; a production store should shard by scope.
//   - The wiring must pass every string the call carries, including the elements of numeric
//     arrays (as text) and the keys of JSON objects, not only string values.
//   - The fetch server must parse a URL the way net/url.Parse does; a URL another parser reads
//     differently can reach a host CheckURL did not see.
package leakguard
