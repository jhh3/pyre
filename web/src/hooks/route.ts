// Hash routing: #/inbox, #/done, #/thread/<id>, #/projects, #/projects/<id>.
// The lists take filters: #/inbox?project=<project_id>, #/inbox?session=<id>
// (a lead: the server includes its child sessions' threads).
import { useSyncExternalStore } from 'react';

export type Filter = { project?: string; session?: string };

export type Route =
  | { name: 'inbox'; filter: Filter }
  | { name: 'done'; filter: Filter }
  | { name: 'thread'; id: string }
  | { name: 'projects' }
  | { name: 'project'; id: string };

function parse(hash: string): Route {
  const [path, query = ''] = hash.replace(/^#\/?/, '').split('?');
  const q = new URLSearchParams(query);
  const filter: Filter = {};
  if (q.get('project')) filter.project = q.get('project')!;
  if (q.get('session')) filter.session = q.get('session')!;
  const parts = path.split('/');
  const id = parts[1] ? decodeURIComponent(parts.slice(1).join('/')) : '';
  switch (parts[0]) {
    case 'done':
      return { name: 'done', filter };
    case 'thread':
      return id ? { name: 'thread', id } : { name: 'inbox', filter };
    case 'projects':
      return id ? { name: 'project', id } : { name: 'projects' };
    default:
      return { name: 'inbox', filter };
  }
}

/** "#/inbox?project=prj_1" for a box and filter. */
export function listHref(box: 'open' | 'done', filter: Filter = {}) {
  const q = new URLSearchParams();
  if (filter.project) q.set('project', filter.project);
  if (filter.session) q.set('session', filter.session);
  const qs = q.toString();
  return (box === 'done' ? '#/done' : '#/inbox') + (qs ? '?' + qs : '');
}

export const filterKey = (f: Filter) => (f.project ? 'p:' + f.project : '') + (f.session ? 's:' + f.session : '');

let current = parse(location.hash);
let hash = location.hash;
const listeners = new Set<() => void>();
window.addEventListener('hashchange', () => {
  hash = location.hash;
  current = parse(hash);
  listeners.forEach((fn) => fn());
});

export const getRoute = () => current;

export function useRoute(): Route {
  return useSyncExternalStore(
    (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
    () => current,
  );
}

export function navigate(to: string, replace = false) {
  if (to === location.hash) return;
  if (replace) {
    history.replaceState(null, '', to);
    hash = to;
    current = parse(to);
    listeners.forEach((fn) => fn());
  } else location.hash = to;
}
