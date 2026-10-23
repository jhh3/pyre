// Types of the web JSON API (/api/...). Field names match the server
// exactly; see docs/web.md and internal/contracts (asks.go, webinbox.go).

export type AskState = 'open' | 'answered' | 'defaulted' | 'expired' | 'withdrawn' | 'superseded';

export interface Principal {
  kind: string;
  id: string;
  name?: string;
}

export interface AskOption {
  id: string;
  label: string;
  detail?: string;
}

// One item of a batched ask: the ask's options apply to each.
export interface AskItem {
  id: string;
  label: string;
  detail?: string;
  url?: string;
}

export interface AskAnswer {
  option_id?: string;
  /** Batched asks: the option per item; items left out take option_id. */
  items?: Record<string, string> | null;
  text?: string;
  by: Principal;
  by_default?: boolean;
  at: string;
  /** "chat": the owner wrote to the session itself instead of answering here (text is what he wrote). */
  source?: string;
  /** Other asks the same chat input answered. */
  with?: string[] | null;
}

export interface AskUpdate {
  seq: number;
  session_id?: string;
  text: string;
  at: string;
}

export interface Ask {
  ask_id: string;
  topic_key: string;
  project_id: string | null;
  session_id?: string;
  title: string;
  context: string;
  options: AskOption[];
  /** A batched ask: the options apply to each item. */
  items?: AskItem[] | null;
  /** The go-ahead asked for ("merge"); a standing grant for exactly it may answer at once. */
  action?: string;
  /** Set when a standing grant answered the ask (the owner was not asked). */
  grant_id?: string;
  /** Never answered by a standing grant. */
  irreversible?: boolean;
  /** A session's proposed standing grant or project fact (options approve / reject). */
  proposal?: AskProposal | null;
  default_option?: string;
  urgent?: boolean;
  deadline_at?: string | null;
  deadline_rule?: string;
  state: AskState;
  answer?: AskAnswer | null;
  updates?: AskUpdate[];
  count: number;
  superseded_by?: string;
  message_id?: string;
  thread_id?: string;
  decision_message_id?: string;
  created_at: string;
  updated_at: string;
}

export interface GrantProposal {
  scope?: 'project' | 'session';
  session_id?: string;
  action: string;
  option: string;
  condition?: string;
  expires_in_days?: number;
}

export interface AskProposal {
  kind: 'grant' | 'fact';
  grant?: GrantProposal | null;
  fact?: string;
  state?: 'pending' | 'approved' | 'applied' | 'rejected' | 'failed';
  result_id?: string;
  error?: string;
}

/** The owner's lasting go-ahead (internal/contracts/standing_grants.go). */
export interface StandingGrant {
  grant_id: string;
  scope: 'project' | 'session';
  project_id: string | null;
  session_id?: string;
  action: string;
  option: string;
  /** Informational note; never checked. */
  condition?: string;
  /** The owner's own words; empty when he approved without typing. */
  quote?: string;
  /** The proposal ask he approved. */
  ask_id?: string;
  proposed_by?: Principal | null;
  recorded_by: Principal;
  expires_at: string;
  used_count: number;
  last_used_at?: string | null;
  created_at: string;
  revoked_at?: string | null;
  revoke_reason?: string;
}

export interface HeldAnswer {
  ask_id: string;
  option_id?: string;
  items?: Record<string, string> | null;
  text?: string;
  held_at: string;
}

export interface HeldAnswers {
  items: HeldAnswer[] | null;
  sends_at?: string | null;
}

export interface SessionRef {
  id: string;
  title: string;
  state: string;
  node: string;
}

/** A session item shown as a thread: "waiting on you" or the session's own flag. */
export interface AttentionItem {
  attention_id: string;
  kind: 'waiting' | 'flag' | string;
  state: 'open' | 'read' | 'handled' | 'withdrawn' | 'snoozed';
  title: string;
  summary?: string;
  session_id?: string;
  count: number;
  created_at: string;
  updated_at: string;
}

export interface Thread {
  thread_id: string;
  kind: 'ask' | 'message' | 'attention';
  title: string;
  preview: string;
  project: { id: string; name: string } | null;
  session: SessionRef | null;
  ask: Ask | null;
  held: HeldAnswer | null;
  last_at: string;
  unread: boolean;
  message_count: number;
  needs_reply: boolean;
  /** kind "attention": the item. */
  attention?: AttentionItem | null;
}

export type DeliveryState = 'stored' | 'queued' | 'delivered' | 'read' | 'replied' | 'failed';

export interface Msg {
  message_id: string;
  from: { kind: 'owner' | 'session'; id: string; label: string };
  text: string;
  at: string;
  reply_to?: string;
  delivery: { state: DeliveryState; summary: string };
  ask_id?: string;
}

export interface SessionCard {
  id: string;
  title: string;
  state: string;
  node: string;
  is_lead: boolean;
  last_activity: { line: string; at: string } | null;
  /** Titles of the live monitors that wake this session (absent: none). */
  watching?: string[];
}

export interface ProjectCard {
  project_id: string;
  name: string;
  description: string;
  open_asks: number;
  waiting: boolean;
  leads: SessionCard[] | null;
  page_updated_at: string | null;
  page_summary: string;
  last_activity_at: string;
  live_sessions: number;
  active: boolean; // live sessions or an open ask
  progress: number | null; // the lead's status card
  phase: string;
  stale: boolean; // work running, lead hasn't updated the page/card lately
}

export interface Project {
  project_id: string;
  name: string;
  description?: string;
  archived?: boolean;
  updated_at?: string;
}

export interface ProjectPage {
  project_id: string;
  document?: { document_id: string; updated_at: string } | null;
  revision?: { revision_id: string; author: Principal; created_at: string; summary?: string } | null;
  content: string;
  status?: ProjectStatus | null;
  status_updated_at?: string | null;
}

// The lead's status card (contracts.ProjectStatus).
export interface StatusItem {
  title: string;
  url?: string;
  state?: string;
  note?: string;
  session?: string;
}

export interface ProjectStatus {
  progress?: number;
  phase?: string;
  eta?: string;
  in_flight?: StatusItem[] | null;
  ready_for_owner?: StatusItem[] | null;
  shipped?: StatusItem[] | null;
  blocked?: StatusItem[] | null;
}

// The project view's live card: the lead's part plus what Pyre sees.
export interface StatusCard {
  lead: ProjectStatus | null;
  updated_at: string | null;
  stale: boolean;
  stale_after_minutes: number;
  working: SessionCard[] | null;
  needs_you: SessionCard[] | null;
  idle: SessionCard[] | null;
  stuck: SessionCard[] | null;
}

export interface Artifact {
  id: string;
  title: string;
  url: string;
  updated_at: string;
}

export interface InboxResponse {
  threads: Thread[] | null;
  held: HeldAnswers;
}

export interface ThreadResponse {
  thread: Thread;
  messages: Msg[] | null;
}

export interface AnswerResult {
  ask_id: string;
  ask?: Ask;
  error?: { code: string; message: string };
}

export interface ArchiveResult {
  thread_id: string;
  thread?: Thread;
  error?: { code: string; message: string };
}

export interface ProjectDetail {
  project: Project;
  page: ProjectPage;
  sessions: SessionCard[] | null;
  asks: Ask[] | null;
  artifacts: Artifact[] | null;
  status?: StatusCard | null;
  /** Active standing grants of the project and its session trees. */
  grants?: StandingGrant[] | null;
}

export type Box = 'open' | 'done';
export type AnswerMode = 'now' | 'hold';
