// localStorage wrapped in try/catch: private windows and blocked storage
// must never break the app; settings then last for the page only.
export const local = {
  get(key: string, fallback: string): string {
    try {
      return localStorage.getItem('pyre.' + key) ?? fallback;
    } catch {
      return fallback;
    }
  },
  set(key: string, value: string) {
    try {
      localStorage.setItem('pyre.' + key, value);
    } catch {
      /* storage unavailable */
    }
  },
};
