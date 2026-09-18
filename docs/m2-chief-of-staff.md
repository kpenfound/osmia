# M2 chief-of-staff demonstration

A workstream's chief of staff talks with the owner, answers the workers'
questions or escalates them, and relays the owner's rulings. It keeps doing
this across service restarts. `TestM2ChiefOfStaffQuestionsAndInbox` in
`internal/service/m2_chief_of_staff_test.go` shows each step through the
service's local API client, which is what the `osmia` commands below call.
The librarian, the chief of staff and the workers are scripted fakes behind
core's fake enforcer, with an in-memory MCP transport. The test makes no
provider or GitHub calls and starts no process or container. It runs in a
temporary root with a local Git repository as the clone.

## Running it

Tests run only inside Dagger, never with `go test` on the host. The test runs
as part of `dagger check`. To run it alone:

```sh
dagger core container from --address golang:1.26-bookworm \
  with-directory --path /src --source . --exclude .git,.bees \
  with-workdir --path /src \
  with-exec --args=go,test,-count=1,-run,TestM2ChiefOfStaffQuestionsAndInbox,-v,./internal/service \
  combined-output
```

## Setup

The project is onboarded the way the
[onboarding walkthrough](m2-onboarding.md) describes: `osmia project add`,
a librarian extraction that succeeds, and a charter with one rule,
`1. Keep state in files under the root.` The root's configuration allows one
mason at a time (`[capacity] masons = 1`) and runs the chief of staff, mason
and reviewer roles in a container with a fixture image.

With the service stopped, the test fixture creates a workstream with three
worker threads, each with one queued turn: two masons and a reviewer. No M2
command creates worker threads. The service gives the workstream its chief of
staff when it starts.

Clock time does not advance in the test, so an
[event window](service.md#event-delivery) closes only when the test moves the
clock forward. Every backend session can be resumed, so each turn of a thread
resumes the session of the thread's previous turn.

`w_…` stands for the workstream ID below.

## The walkthrough

### 1. Talk to the chief of staff

```sh
osmia send w_… "Where do we stand?"
osmia conversation w_…
osmia status w_…
```

`send` records the message as a `queued` turn. The chief of staff's turn has
`set_status`, `answer`, `escalate`, `relay_ruling`, `route_amendment` and
`propose_charter`, and no file tools. It first calls `set_status` with an agent
line that names an agent ID. The call is refused with the reason
`agents[0] contains an Osmia or backend identifier ("agent_mason"); refer to
the work or the agent in words`, and nothing is stored. It then writes a
status in plain words and replies. `conversation` lists the message and the
reply, both `done`. `status` shows status revision 1: the goal, the attention
line, the note and one line per agent. None of these contains a project,
workstream, agent, thread, turn or session ID.

### 2. A worker asks

In the same pass, the first mason and the reviewer each call `ask`. Each
asking turn ends with the outcome `waiting`, and the thread parks. Questions 1
and 2 are `open`. The first mason no longer holds the only mason slot, so the
second mason's queued turn runs next. That turn checks that the first mason
is already parked.

### 3. The chief of staff answers one and escalates the other

Once the event window closes, the chief of staff gets one event turn with
both questions. It answers question 1 with the citation `charter#1` and
escalates question 2 with a rephrasing, what is blocked, two options and a
recommendation. In the next pass the answer becomes the first mason's next
turn on its own thread, and that turn resumes the mason's previous session.
The mason's thread is then idle. The reviewer stays parked.

### 4. The inbox survives a restart

```sh
osmia inbox
```

The inbox has one entry, number 1, in batch `escalation_2` (a batch is named
after its first question). The entry holds the rephrasing, what is blocked,
the options, the recommendation and the reviewer's question as the reviewer
asked it. The service is then stopped and started again. `osmia inbox` returns
the same entry, once, and no turn runs again.

### 5. The owner rules

```sh
osmia answer 1 "Keep the upload API as it is and add a new endpoint."
osmia send w_… "Did the upload API change?"
```

`answer` records the ruling, and the inbox is empty. Once the event window
closes, the chief of staff gets the ruling as an event turn. It calls
`relay_ruling` with its own rephrasing and the scope `notify`. In the next
pass the rephrased ruling becomes the reviewer's next turn on its own
thread, and that turn resumes the reviewer's previous session. The prompt
reads:

```text
The owner ruled on your question 2. The chief of staff relays the ruling.

You asked:
May I change the upload API's response?

Answer:
The upload API stays unchanged; add a new endpoint for resumable uploads.
```

The context of the first message said `No project-wide notices.` The context
of the second message ends with the ruling as a project notice:

```text
## Notices
- workstreams/w_…/questions/2/rulings.jsonl (record 2 revision 2, workstream w_…)
  notice: The upload API stays unchanged; add a new endpoint for resumable uploads.
```

### 6. The record

The service is stopped. Of all the threads in the workstream, only the chief
of staff's received event turns: two of them, one for the questions and one
for the ruling. The fake engine ran exactly these turns: the extraction, two
owner messages, the three first worker turns, two event turns and two answer
turns.

The trace holds, committed at `HEAD` under
`<root>/projects/<project-id>/workstreams/w_…/questions/`:

| File | What it shows |
| --- | --- |
| `1/question.jsonl` | The first mason's question |
| `1/rulings.jsonl` | The chief of staff's answer, what was sent back and its citation `charter#1` |
| `2/question.jsonl` | The reviewer's question, then revision 2 with the escalation: batch, rephrasing sent to the owner, what is blocked, options and recommendation |
| `2/rulings.jsonl` | Revision 1, the owner's ruling as given; revision 2, the chief of staff's relayed answer and its scope `notify` |

The [trace reference](trace.md#questions) documents these records.

## What is faked

The test injects fake engines and an in-memory MCP transport into
`service.Enforce`, the function `osmia serve` uses to build role turns. The
chief of staff therefore runs with the production grant, tools, workspace
and session directory. `osmia serve` grants thread turns to the chief of
staff and to the mason, whose turn works in its unit's workspace with
`file_read` and `file_write`. So that its fake workers can ask, the test grants the mason and
reviewer roles `ask` and gives their turns the ask tool as the asking agent.
The fake workers build no unit, so each works in the empty directory the chief
of staff is handed. The chief of staff's grant and tools are unchanged.
