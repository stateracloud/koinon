# 0117. Simplified Technical English for public project text

- Status: Accepted
- Date: 2026-11-14
- Area: engineering-process
- Tags: adr-process, docs
- Issue: #964

## Context

Koinon is read by people from many countries, and many of them read English as a second language.
Machine translation and AI agents also read our text. Long sentences, idioms and words with several
meanings cause wrong readings in all three cases. Pull request bodies, issues and review comments are
the text that people read first, and they vary most between authors.

On 2026-11-13 the maintainer decided that public project text follows ASD-STE100 (Simplified
Technical English), and that an ADR records the rule.

## Decision

1. **Scope.** The rule covers text that people other than the author read:
   - pull request titles and bodies, issue titles and bodies, and review and issue comments;
   - documentation: `docs/` (ADRs included), `README.md`, `CONTRIBUTING.md`, `AGENTS.md`, `GLOSSARY.md`,
     chart and hack READMEs, and the docs site (`stateracloud/koinon-docs`, ADR 0108).
2. **Out of scope.**
   - Code, identifiers, and code comments (comments should be short and clear, but are not checked).
   - Commit subjects: they follow Conventional Commits. A commit body should follow this rule, but it
     is not checked.
   - Quoted text: logs, command output, error messages from other programs, quotes of other people,
     names of products, and the titles of standards.
   - Text that is not English (test fixtures, model prompts that test other languages).
3. **Core rules.** We apply the writing rules of ASD-STE100 in this short form. Write:
   - **One topic per sentence.** Do not join two instructions or two facts with "and" or a semicolon.
   - **Short sentences.** Procedures (steps): at most 20 words. Descriptions: at most 25 words.
   - **One paragraph, one topic.** A paragraph has at most six sentences. Start with the main point.
   - **Active voice** and the simple tenses (present, past, future). Write "The API returns an error",
     not "An error is returned by the API". Name the actor.
   - **Approved meanings.** Use a word in one meaning only. Use one word for one thing. Use the
     same noun every time (do not alternate "run", "job" and "execution" for one concept). Use the
     terms of `GLOSSARY.md`. Prefer the plain word ("use", not "utilize").
   - **No idioms, no phrasal verbs with figurative meaning, no slang.** Write "stop", not "kill off".
   - **Commands as imperatives.** Write "Run `make test`", not "You might want to run".
   - **Do not drop articles** ("the", "a"). Do not use contractions in docs.
   - **Warnings first.** Flag each warning as a warning. Put the safety or data-loss warning before the step.
4. **How we apply it.** Authors write in this form. Reviewers may ask for a rewrite of unclear text.
   Reviewers cite this ADR. A word that STE100 does not approve is not an error when a technical name needs
   it (for example "sandbox", "pod", "backup"). Technical names follow the project, not the dictionary.
5. **Checks.** No automatic check now. A linter for STE100 does not exist as an open tool that fits
   our CI (see Prior art). Sentence length and passive voice may get an advisory script later, in a new
   issue, after we see how often reviewers need it.
6. **Existing text.** We do not rewrite existing documents in bulk. We rewrite a document when we
   change it for another reason.

## Prior art

- **ASD-STE100, issue 9 (2025).** A controlled language from the aerospace and defence industry. It has
  writing rules (sentence and paragraph length, active voice, one meaning per word, no idioms) and a
  dictionary of approved words with one meaning each. The specification is copyrighted by ASD, so this
  ADR states the rules in our own words and does not copy the text. We use the writing rules fully. We
  do not adopt the dictionary as a check, because software terms are missing from it.
- **Plain language and style guides.** The [US plain language guidelines](https://www.plainlanguage.gov/guidelines/)
  (short sentences, active voice, "you", common words), the
  [Microsoft Writing Style Guide](https://learn.microsoft.com/en-us/style-guide/welcome/) and the
  [Google developer documentation style guide](https://developers.google.com/style) all give the same
  core: active voice, short sentences, no idioms, readable for a global and machine-translated
  audience. We take their rules for technical names and for commands as imperatives.
- **Projects with a controlled language.** Some documentation teams in regulated industries apply STE
  with checkers such as Acrolinx or HyperSTE. These are commercial tools. Open-source projects usually
  use a style guide without a controlled dictionary (Kubernetes and Write the Docs are examples).
  Therefore we take the STE100 rules and keep the check manual.

## Alternatives considered

- **Style guide only (Google or Microsoft).** Easier to adopt and has no dictionary, but it is longer
  and does not state a limit for sentence length. The maintainer chose STE100. Not chosen.
- **Enforce with a CI check.** A false-positive rate on technical text would make the check noisy, and
  the tools are commercial. Not chosen now; see decision 5.
- **Apply the rule to code comments and commit bodies.** Wider scope, but it adds review cost for text
  that few people read. Not chosen.

## Consequences

- Positive: text is easier to read and to translate for people and for agents; reviews can point to
  one rule.
- Negative: authors need to learn the rules, and some technical text reads less naturally.
- Negative: no automatic check, so the rule depends on review.
- Scope: user-facing product messages are out of scope for this ADR. A later decision can add them.
- Follow-up: a short advisory check for sentence length and passive voice, if reviewers ask for it.

## Links

- [0046](0046-adr-conventions.md) ADR conventions
- [0108](0108-docs-site-in-its-own-repository.md) docs site in its own repository
- [0115](0115-document-metadata.md) document metadata
- ASD-STE100 Simplified Technical English, issue 9: <https://www.asd-ste100.org/>
