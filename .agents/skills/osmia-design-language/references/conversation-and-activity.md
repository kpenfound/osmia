# Conversation and Activity

A workstream has two tabs, and the split is the point.

- **Conversation** is the default and holds what the owner reads and answers: owner messages, manager replies and actions, images, delivery briefs, owner command outcomes, and only the latest manager status. Decision cards sit above the composer. Machine events appear only as quiet one-line milestones: workstream state changes (ratified, building, assembled, delivered) and final review results. Tapping a milestone opens Activity at that entry. Sessions, unit transitions and check runs, failed ones included, never appear here.
- **Activity** holds every event the feed once showed, with no detail lost: sessions, unit and workstream transitions with reasons, check runs, final reviews and every manager status. Consecutive runs of events fold into an "N events" line that opens in place. The only filter is by unit; there are no event-type filters. Older manager statuses recede and the latest is clearly current.
- The tab, the unit filter and opened entries live in the URL, so loading a link restores the view and a debugging link can be shared.
- Unread markers count Conversation entries only.
- A workgroup keeps its shared conversation, and each member's machine events go to that member's Activity.
