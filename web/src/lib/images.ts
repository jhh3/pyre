// Images pasted or dropped onto the composer. Each is checked here for a
// quick answer (the server sniffs the bytes again and is the authority),
// previewed from a data: URL (the CSP allows img-src data:), and uploaded
// when the reply is sent.

export const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
export const MAX_IMAGE_BYTES = 20 << 20; // artifacts.MaxImageBytes
export const MAX_IMAGES = 10; // maxReplyImages

export type DraftImage = {
  key: string;
  name: string;
  size: number;
  preview: string; // data: URL
  file: Blob;
  version?: string; // set once uploaded, so a retry does not upload again
};

/** The files a paste or drop carries (images or not; add() sorts them out). */
export function filesOf(dt: DataTransfer | null): File[] {
  if (!dt) return [];
  const out: File[] = [];
  for (const item of Array.from(dt.items ?? [])) {
    if (item.kind !== 'file') continue;
    const f = item.getAsFile();
    if (f) out.push(f);
  }
  if (out.length === 0) out.push(...Array.from(dt.files ?? []));
  return out;
}

/** Whether a drag carries files (the item list is hidden until drop). */
export function dragHasFiles(dt: DataTransfer | null): boolean {
  return !!dt && Array.from(dt.types ?? []).includes('Files');
}

/** Why a file cannot be attached, or null. */
export function rejectReason(f: File): string | null {
  if (!IMAGE_TYPES.includes(f.type)) return `${f.name || 'This file'} is not a PNG, JPEG, GIF or WebP image.`;
  if (f.size > MAX_IMAGE_BYTES) return `${f.name || 'This image'} is larger than ${MAX_IMAGE_BYTES >> 20} MB.`;
  if (f.size === 0) return `${f.name || 'This image'} is empty.`;
  return null;
}

export function toDraft(f: File, key: string): Promise<DraftImage> {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve({ key, name: f.name || 'pasted image', size: f.size, preview: String(r.result), file: f });
    r.onerror = () => reject(r.error ?? new Error('Could not read the image.'));
    r.readAsDataURL(f);
  });
}

export function plural(n: number, word: string) {
  return `${n} ${word}${n === 1 ? '' : 's'}`;
}

let seq = 0;
/** A key for a draft image chip (unique within this tab). */
export function newImageKey() {
  return 'img' + ++seq + '-' + Date.now().toString(36);
}
