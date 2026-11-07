#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Claude Code PreToolUse hook on the Bash tool (old issue 534; docs/research/2026-11-token-tiers.md).
# Reading several files in one Bash call was 20% of all subagent output. This hook refuses a command
# that prints files the expensive way:
#   - `cat`, `head` or `tail` of two or more files;
#   - two or more `sed -n` file reads in one command (or one `sed -n` over several files);
#   - a for/while/until loop whose body cats, seds, heads or tails a file;
#   - a bare `cat` of one source file (.go .ts .tsx .sh .yaml .yml .md .sql .proto).
# Allowed: output into a pipe, a redirect or a command substitution (`cat x | grep`, `cat > f <<EOF`,
# `v=$(cat f)`), heredoc bodies, anything inside quotes (a commit message, `gh --body "..."`), and
# /dev/*, /proc/*, *.log and task *.output files. The patterns are heuristic: the cases are in
# read-hooks_test.sh.
# On a violation it prints a PreToolUse "deny" decision with a one-line reason; otherwise nothing.
# Needs only bash, awk and jq.

# Needs bash 5 (ADR 0090); keep this line above any bash 4+ syntax.
[ "${BASH_VERSINFO:-0}" -ge 5 ] || { echo '{"systemMessage":"Koinon hooks skipped: they need bash 5 or newer; on macOS: brew install bash"}'; exit 0; }

set -uo pipefail

in=$(cat)
cmd=$(jq -r '.tool_input.command // ""' <<<"$in")
[ -n "$cmd" ] || exit 0
# Generated scripts and pasted data, not reads: allow without parsing (awk cost grows with size).
[ ${#cmd} -gt 65536 ] && exit 0

# Tokenize like a shell (quotes become Q, heredoc bodies and comments vanish), then judge each simple
# command. Prints the first violation, or nothing.
why=$(printf '%s\n' "$cmd" | awk -v sq="'" '
function push(k, v) { n++; tk[n] = k; tv[n] = v }
# add(x): extend the current word; a word past 1024 characters is no file name, so stop growing it
# (string concatenation is quadratic in some awks).
function add(x) { if (length(word) < 1024) word = word x; inword = 1 }
function flush() { if (inword) { push("W", word); word = ""; inword = 0 } }
# The command as one character array C[1..L], built per line: linear, where substr() per character
# over one long string is quadratic in BSD awk (macOS).
{ m = split($0, t, ""); for (j = 1; j <= m; j++) C[++L] = t[j]; C[++L] = "\n" }
function at(k) { return (k >= 1 && k <= L) ? C[k] : "" }
END {
  i = 1; nh = 0
  while (i <= L) {
    c = C[i]; c2 = c at(i + 1)
    if (c == "\n") {
      flush(); push("OP", ";"); i++
      for (h = 1; h <= nh; h++) {          # skip each pending heredoc body
        while (i <= L) {
          line = ""
          while (i <= L && C[i] != "\n") { if (length(line) < 1024) line = line C[i]; i++ }
          i++
          sub(/^[ \t]+/, "", line)
          if (line == hd[h]) break
        }
      }
      nh = 0; continue
    }
    if (c == " " || c == "\t") { flush(); i++; continue }
    if (c == "\\") {
      if (at(i + 1) == "\n") { i += 2; continue }
      add(at(i + 1)); i += 2; continue
    }
    if (c == sq) {
      i++; while (i <= L && C[i] != sq) i++
      i++; add("Q"); continue
    }
    if (c == "\"") {
      i++; while (i <= L && C[i] != "\"") { if (C[i] == "\\") i++; i++ }
      i++; add("Q"); continue
    }
    if (c == "#" && !inword) {
      while (i <= L && C[i] != "\n") i++
      continue
    }
    if (c == ";") { flush(); push("OP", ";"); i++; if (at(i) == ";") i++; continue }
    if (c2 == "&&") { flush(); push("OP", "&&"); i += 2; continue }
    if (c2 == "&>") { flush(); push("OP", ">"); i += 2; if (at(i) == ">") i++; continue }
    if (c == "&") { flush(); push("OP", ";"); i++; continue }
    if (c2 == "||") { flush(); push("OP", "||"); i += 2; continue }
    if (c2 == "|&") { flush(); push("OP", "|"); i += 2; continue }
    if (c == "|") { flush(); push("OP", "|"); i++; continue }
    if (c2 at(i + 2) == "$((") { add("$(("); i += 3; continue }
    if (c2 == "$(" || c2 == "<(" || c2 == ">(") { flush(); push("OP", "$("); i += 2; continue }
    if (c == "`") { flush(); push("OP", "`"); i++; continue }
    if (c == "(") { flush(); push("OP", "("); i++; continue }
    if (c == ")") { flush(); push("OP", ")"); i++; continue }
    if (c2 at(i + 2) == "<<<") { flush(); push("OP", "R<"); i += 3; continue }
    if (c2 == "<<") {
      flush(); i += 2; if (at(i) == "-") i++
      while (at(i) == " " || at(i) == "\t") i++
      d = ""
      while (i <= L) {
        x = C[i]
        if (x ~ /[ \t\n;|&<>()]/) break
        if (x != sq && x != "\"" && x != "\\" && length(d) < 1024) d = d x
        i++
      }
      hd[++nh] = d; push("OP", "HD"); continue
    }
    if (c == "<") { flush(); push("OP", "R<"); i++; continue }
    if (c == ">") {
      fd = ""
      if (inword && word ~ /^[0-9]+$/) { fd = word; word = ""; inword = 0 } else flush()
      op = (fd == "" || fd == "1") ? ">" : "R>"
      i++; if (at(i) == ">") i++
      if (at(i) == "&") {                    # a dup (2>&1, >&2): no target word follows
        i++
        while (at(i) ~ /[0-9-]/) i++
        if (op == "R>") continue
        push("OP", ">D"); continue
      }
      push("OP", op); continue
    }
    add(c); i++
  }
  flush(); push("OP", ";")

  ns = 0; sout = 0; tgt = ""; loops = 0; sedfiles = 0; depth = 0; bt = 0
  for (k = 1; k <= n; k++) {
    if (tk[k] == "W") {
      if (tgt == ">") { sout = 1; tgt = ""; continue }
      if (tgt != "") { tgt = ""; continue }
      sw[++ns] = tv[k]; continue
    }
    o = tv[k]
    if (o == ">") { tgt = ">"; continue }
    if (o == ">D") { sout = 1; continue }
    if (o == "R>" || o == "R<") { tgt = o; continue }
    if (o == "HD") continue
    if (o == "$(") {                         # save the outer command, judge the inner as captured
      depth++; save_ns[depth] = ns; save_sout[depth] = sout
      for (m = 1; m <= ns; m++) save_w[depth, m] = sw[m]
      ns = 0; sout = 0; continue
    }
    if (o == ")" && depth > 0) {
      judge(1); ns = save_ns[depth]; sout = save_sout[depth]
      for (m = 1; m <= ns; m++) sw[m] = save_w[depth, m]
      sw[++ns] = "Q"; depth--; continue
    }
    if (o == "`") { judge(1); bt = !bt; continue }
    judge(o == "|")
  }
}
# judge(piped): the simple command in sw[1..ns]. Resets the command state.
function judge(piped,   a, w, cmd, f, nf, m, hasn, inpl, script, skip) {
  piped = piped || sout || depth > 0 || bt
  a = 1
  while (a <= ns && (sw[a] ~ /^(do|then|else|elif|if|!|\{|\}|time|exec|command|builtin|nohup)$/ || sw[a] ~ /^[A-Za-z_][A-Za-z0-9_]*=/)) a++
  cmd = (a <= ns) ? sw[a] : ""
  sub(/.*\//, "", cmd)
  if (cmd == "for" || cmd == "while" || cmd == "until" || cmd == "select") loops++
  else if (cmd == "done" && loops > 0) loops--
  nf = 0; hasn = 0; inpl = 0; script = 0; skip = 0
  for (m = a + 1; m <= ns; m++) {
    w = sw[m]
    if (skip) { skip = 0; if (cmd == "sed") script = 1; continue }
    if (cmd == "sed") {
      if (w ~ /^-[A-Za-z]*n[A-Za-z]*$/ || w == "--quiet" || w == "--silent") hasn = 1
      if (w ~ /^-[A-Za-z]*i/ || w ~ /^--in-place/) inpl = 1
      if (w == "-e" || w == "-f") { skip = 1; continue }
      if (w ~ /^-/) continue
      if (!script) { script = 1; continue }
    } else if (cmd == "head" || cmd == "tail") {
      if (w ~ /^-[nc]$/ || w == "--lines" || w == "--bytes") { skip = 1; continue }
      if (w ~ /^-/) continue
    } else if (cmd == "cat") {
      if (w ~ /^-./) continue
    } else break
    if (w == "-" || w ~ /^\/(dev|proc)\// || w ~ /\.(log|output)$/) continue
    f[++nf] = w
  }
  if (!piped && !reason) {
    if (cmd == "cat" || cmd == "head" || cmd == "tail") {
      if (nf >= 2) reason = cmd " of " nf " files"
      else if (nf == 1 && loops > 0) reason = cmd " in a loop"
      else if (nf == 1 && cmd == "cat" && f[1] ~ /\.(go|ts|tsx|sh|yaml|yml|md|sql|proto)$/) reason = "cat of a whole source file"
    } else if (cmd == "sed" && hasn && !inpl && nf >= 1) {
      sedfiles += nf
      if (loops > 0) reason = "sed -n in a loop"
      else if (sedfiles >= 2) reason = sedfiles " sed -n reads in one command"
    }
  }
  ns = 0; sout = 0; tgt = ""
  if (reason && !printed) { print reason; printed = 1 }
}')

[ -n "$why" ] || exit 0
reason="Prints several files or a whole source file ($why): one file per call, Read with offset and limit, grep -n, or codegraph (docs/agents/subagent-brief.md)."
jq -nc --arg r "$reason" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
