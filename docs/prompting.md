# Writing Osmia prompts

This page says how the text Osmia gives its own agents is written, so new text
stays aligned with Anthropic's current guidance. It covers every role's system
prompt, the operating policy, task text, tool descriptions, the "next step" text
a tool result returns, and the refusal and error text an agent reads.
[AGENTS.md](../AGENTS.md) holds the rules for changing the repository itself.

## Write plain sections

Give each role's base prompt these parts, in plain headed sections or short
paragraphs:

- the role's purpose and setting;
- its tools and gates;
- its part of the state machine;
- its output format;
- when it is done.

The base prompt must be enough for the role with no skills attached. Say
directly what to do. Do not use capitals, emphasis or threats to stress a rule.

## Give every rule its reason

A rule with its reason lets the agent judge a case the rule did not foresee.
Write the reason in the same sentence or the next one. The operating policy
renders each rule as a bullet that ends with `Reason:`, and its test fails when
a rule lacks one.

## Scope every rule

State where a rule applies and stop it there.

- Narrow an absolute rule to what it protects. "Never approve your own candidate"
  becomes a rule about who approves a candidate and why. "Never commission a
  revision" becomes "do not commission a revision only to widen a footprint".
- Text about units, plans and unit sizing reaches only a kind that has them. The
  policy reads `kinds.Kind` flags and documents (`Flags.Units`,
  `Documents.Planned()`), so a Hill Climb architect or committee turn gets none
  of it. Add the next kind-specific rule the same way, never by testing a kind's
  name.
- State a role's rule in that role's text. Do not make every role read it.

## Name no skill, and keep no general preference

Skills are configuration, and a deployment may load none, so no prompt, task,
tool description or tool result names a skill or depends on one being loaded.
Code style, comments, prose style, test style, review method and report layout
are preferences that skills carry. Keep them out of prompts.

Remove a sentence from an existing prompt only for one of these reasons:

- a skill configured for that role says the same thing;
- Anthropic's guidance says the sentence now backfires;
- it is duplicated, or out of date.

A role with no configured skills (architect, foreman, committee) loses only the
second and third kinds. Never remove a factory rule: owner gates, footprints, the
check gate, the design agent's limits, untrusted-input handling, output formats,
and Go alone deciding whether a Hill Climb attempt is kept. Reword them, give
them reasons, and keep their meaning. Record a removed preference that no
configured skill covers as a discovery for the skills project, with the role and
the removed text.

## Use one text for every model

Every profile reads the same text, and no code branches on the model or keeps a
per-model variant. Put model-specific advice by role, where it belongs to the
work:

- the architect is told to finish the whole task;
- the designer and the chief are told how to write a short report that stands
  alone, and the chief how to give progress updates;
- no role is asked to write out its reasoning in a reply;
- a mason is not told it owns the code.

## Change instructions in the newest turn

A resumed session must keep the system prompt it began with, because a changed
system prompt discards the model's earlier reasoning and some models reject the
request. Put what does not change for the session in the system prompt. Put
what changes during a conversation, such as the operating policy, the design
catalog and the brief's state, in the turn context block that opens the newest
turn (`coreadapter.TurnContext`). Its header tells the agent to follow the
newest block over an older copy, so the text itself needs no "this replaces
older instructions" framing.

## Write tools for the agent that calls them

Follow Anthropic's guidance on [writing tools for agents](https://www.anthropic.com/engineering/writing-tools-for-agents).

- A description says what the tool does, when to use it instead of a neighbour,
  what each input means, and what the result carries, including limits such as
  line counts and byte caps.
- A result returns what the agent needs for its next move, and its next-step
  text says what to do next, such as "End your turn now".
- A refusal or error says what was wrong and how to correct the call. It names
  the input or the tool to use instead.
- Tool and input names say what they do.

## Check a new prompt

Before you merge a prompt, check that:

1. each section above is present or deliberately not needed;
2. each rule has a reason and a scope;
3. no skill is named and no skill is needed to do the role's work;
4. nothing in it is a general preference;
5. nothing in it changes during a session, unless it sits in the turn context;
6. no sentence repeats text that the same agent already reads elsewhere.

## Sources

- [Prompting best practices](https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/claude-prompting-best-practices)
- [Writing effective tools for agents](https://www.anthropic.com/engineering/writing-tools-for-agents)
- [Effective context engineering for AI agents](https://anthropic.com/engineering/effective-context-engineering-for-ai-agents)
