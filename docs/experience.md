# Experience design

Pyre should be a pleasant place to see and direct work, while keeping the agent terminal immediately accessible. The initial UX described here is a design brief, not a pixel-perfect specification. Build an interactive prototype against the fake backend early and refine it using the scenarios in the product spec.

## Terminal overview

Three primary destinations: **Sessions**, **Projects**, and **Inbox**. Machines, accounts, connections, environments, and updates are available through the command palette and management views. Do not put all management tables on the home screen.

An illustrative layout:

```text
 Pyre          Sessions  Projects  Inbox 3                   / Search

 Sessions                       Current selection
 ● outfit API       server         Claude · working · updated 2s ago
 ? release review   mac         Project: Outfit · branch api-client
 ○ research         server         Account: personal-2 · health current
 ◌ scratch          laptop      Latest activity and terminal preview

 Needs you: review proposal      [Enter] Join  [m] Message  [p] Project
                                [:] Commands  [?] Help
```

This is illustrative. A polished implementation needs responsive sizing, restrained color, readable density, stable selection during updates, and visible keyboard hints. Status cannot rely on color alone. Search and the command palette should be usable with a narrow terminal and without a mouse.

### Session row

Show a useful name, harness, machine, current activity, last update, and an attention indicator. Project and account are available without making every row too wide. Distinguish working, waiting for input, idle, offline/stale, and failed. Unknown runtime state is a valid state.

Enter attaches to the real session through its verified adapter; detach returns to the same list/filter/selection. The UI itself does not own the agent lifetime. A session list should remain usable when a node is slow or offline.

### Launch flow

Choose a machine and Claude, Codex, or a supported shell. Choose a directory or optionally a project. Resolve defaults for account and environment, show the selected values, and launch. Project-specific worktree creation is available, not compulsory. A saved launch preference makes the common case short.

Natural-language orchestration uses the same launch operations. It must not need a separate wizard or a different definition of a session.

A session started by another session reports back to it without depending on prompt wording (W-childnotify). The hub sends the starting session an ordinary Pyre message from the child (queue mode, so the parent's doorbell and `[pyre-msg:…]` delivery apply, and a reply threads back to the child) when the child:

- ends a turn and waits for input. The message carries the child's last assistant message (at most 1500 characters), title, machine and state. There is one report per turn, and none when the child already messaged the parent during that turn;
- needs input (an approval or question). The message lists pending approval IDs so the parent can use `answer_approval`, or else tells the owner;
- exits or fails. The message gives the reason and exit code.

Nothing is sent to an archived parent, a parent without a live run, or an archived child, and the owner's inbox is never used as a fallback. Reports per child are rate-limited (12 per 10 minutes) so a parent and child that keep answering each other cannot loop. This is on by default when a session starts the child (`start_session report_to_parent`, `pyre start --no-report` to opt out). Owner-started sessions are unaffected. The choice is the reserved tag `report:parent` / `report:none` on the child, and editing the tags toggles it. The child's guide and environment (`$PYRE_PARENT_SESSION_ID`, `$PYRE_REPORT_TO_PARENT=1`) say who started it and that its turn summary is relayed. It need not message the parent unless asked, and it messages the owner only when told to. The TUI session facts and the Android session details show "Started by <parent title>", followed by "· reports to parent" when reports are on.

### Interaction and takeover

Messages enter the harness's supported queue or input mechanism. If a harness requires an explicit local/remote ownership switch, the UI says so and preserves the native session. Do not imply that all harnesses support concurrent typing, live steering, and every approval type.

Display queued messages and allow supported cancellation. An uncertain delivery stays visible with a reconciliation action; a retry button must not silently duplicate an unknown execution. Manual terminal input and remote approvals must reconcile against the same runtime state.

## Project home

The home should answer: what are we trying to accomplish, what changed, where is the current work, and what needs review?

- **Overview:** a short brief, useful links, current sessions, and recent activity.
- **Docs:** searchable versioned text, readable in the UI and editable through a normal editor workflow.
- **Artifacts:** previews/downloads, versions, producer/session provenance, and review comments.
- **Discussion:** project threads with links to sessions, documents, and outputs.
- **Work:** optional checklist/assignments/dependencies for projects that benefit from them.

The home must work for a project with no Git repository. Linking multiple repos should be normal. Native work directories remain on their nodes; the shared document and artifact area has its own durable published state.

### Documents

Open a document in the terminal reader, Android reader, or local editor. Editing starts from a known revision. Saving/publishing creates a version; if another writer has changed it, preserve the draft and show a conflict rather than losing either edit. A pending local draft and a published version are clearly different.

Agents use the same read/publish operations. A concise project brief can be supplied at session start; additional documentation is retrieved as needed rather than dumped into every prompt.

### Artifacts and review

Publish a file or result link with a title and optional description. A local path is not sufficient to make it shared. A successful publish receipt means bytes and metadata are retrievable independently of the producing node.

Support Markdown/text, images, PDFs, HTML previews, and downloadable arbitrary files. Use existing viewers and platform open/share behavior. Do not implement an office suite. HTML renders in an isolated viewer that cannot access hub credentials or administration APIs.

Comments attach to an exact artifact/document revision and can mention a session. The notification links back to the relevant review. A follow-up version preserves previous comments and makes the revision relationship visible. Mobile should make it easy to say “this part needs changing” and send that feedback to a worker.

## Inbox and notifications

The inbox gathers user-directed questions, approvals, failures, exhausted account pools, completed results needing review, and important messages. Collapse repeated events into one ongoing incident with updates. Allow quiet hours, per-project muting, and filtering.

Delivery events are not all attention events. An agent sending ten progress messages should not generate ten urgent phone alerts. Avoid recursive loops where a notification acknowledgement becomes another notification.

The same pending approval can be visible on Android and the terminal. Once the native runtime resolves it, withdraw stale controls on all surfaces. Never invent which client won if the harness only reports that a request was resolved.

## Android companion

Begin with HAPI's Kotlin/Compose client if the backend gate passes. Deliver a signed APK for sideloading with repeatable build instructions and an update route using the same signing identity.

Primary mobile capabilities:

1. See sessions and attention requests across the fleet.
2. Read/reply, answer structured questions, approve supported requests, interrupt, and start a session on an online machine.
3. Open project briefs, documentation, and published artifacts; comment and share feedback.
4. Receive useful background notifications while the screen is off, with reply/approval actions when supported.
5. View machine and account health and understand why work paused.

The first full-terminal path can open the existing authenticated web terminal. It is an explicit fallback until native terminal support is implemented, not a promised built-in Android terminal. The app may cache project material and drafts; queued offline actions must show pending status and reconcile after reconnect.

Native push is a separate dependency from session networking. A private Firebase-enabled build requires matching app/hub configuration. Do not claim push works simply because a debug APK can connect in the foreground. Verify on the user's actual Android setup, including network reachability through Twingate.

## Management views

**Machines:** online status, last seen, agent availability, versions, environment health, resource use, update/restart outcomes, and a direct SSH action where configured.

**Accounts:** provider, friendly name, health, usage/reset timestamps, active assignments, and fallback eligibility. Never display tokens. Clearly distinguish subscription usage from API billing.

**Connections:** service/account, verified status, machine availability, project/environment references, and the action required to make it usable on another node.

**Environments:** skills, plugins, tool versions, configuration layers, and pending drift. Show a concrete change preview when adopting or applying configuration; preserve personal changes.

## Design validation

Test keyboard-only navigation, narrow terminals, long session names, offline nodes, bursts of updates, search, and reconnect without selection loss. On Android test small screens, screen-off notifications, resumed drafts, artifact download/open, and stale approval removal. Capture screenshots/recordings of these flows using synthetic data for review.
