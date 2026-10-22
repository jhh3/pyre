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

export interface AskAnswer {
  option_id?: string;
  text?: string;
  by: Principal;
  by_default?: boolean;
  at: string;
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

export interface HeldAnswer {
  ask_id: string;
  option_id?: string;
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

export interface Thread {
  thread_id: string;
  kind: 'ask' | 'message';
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
  attachments?: number; // published objects attached (e.g. images)
}

export interface Upload {
  version_id: string;
  media_type: string;
  size: number;
  filename: string;
}

export interface SessionCard {
  id: string;
  title: string;
  state: string;
  node: string;
  is_lead: boolean;
  last_activity: { line: string; at: string } | null;
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
}

export type Box = 'open' | 'done';
export type AnswerMode = 'now' | 'hold';
