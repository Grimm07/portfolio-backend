// backend/src/shared/validation.ts
export const MIN_FORM_TIME_MS = 3000;
export const MAX_NAME = 200;
export const MAX_MESSAGE = 5000;
export const MAX_EMAIL = 254;

export function isHoneypotTripped(s: { website?: string }): boolean {
  return !!s.website && s.website.trim().length > 0;
}

export function isTooFast(formTimestamp: number, now: number): boolean {
  return now - formTimestamp < MIN_FORM_TIME_MS;
}

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
export function isValidEmail(email: string): boolean {
  return typeof email === 'string' && email.length <= MAX_EMAIL && EMAIL_RE.test(email);
}

export function isValidMessage(message: unknown): boolean {
  return typeof message === 'string' && message.length > 0 && message.length <= MAX_MESSAGE;
}

/** Strip all control characters and cap length (fixes Worker header-injection bug). */
export function sanitizeName(name: string): string {
  // eslint-disable-next-line no-control-regex
  return name.replace(/[\r\n\t\x00-\x1f\x7f]/g, '').slice(0, MAX_NAME);
}
